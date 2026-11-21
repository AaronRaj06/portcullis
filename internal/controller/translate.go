// Package controller is the Portcullis control plane: it reads Gateway API
// resources (GatewayClass, Gateway, HTTPRoute), Services, EndpointSlices and
// PortcullisPolicies, translates them into one Portcullis config snapshot,
// pushes it to every data-plane pod, and writes status conditions back.
//
// translate.go is the pure part (objects in, config and statuses out), so it
// is unit-tested without a cluster. reconcile.go talks to the API server and
// push.go to the data planes.
package controller

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/controller/v1alpha1"
	"github.com/AaronRaj06/portcullis/internal/policy"
)

// ControllerName is the GatewayClass spec.controllerName this controller serves.
const ControllerName gwv1.GatewayController = "portcullis.dev/gateway-controller"

// sentinelPool exists for routes that must answer by policy alone (no valid
// backend, or no routes at all): the config schema needs a pool with a
// backend, and the attached deny(...) runs before any backend is picked.
const sentinelPool = "portcullis-sentinel"

// Inputs is everything the translation reads, already listed from the cluster.
type Inputs struct {
	Classes   []gwv1.GatewayClass
	Gateways  []gwv1.Gateway
	Routes    []gwv1.HTTPRoute
	Services  []corev1.Service
	Slices    []discoveryv1.EndpointSlice
	Policies  []v1alpha1.PortcullisPolicy
	Instances int // data-plane replicas (for rate-limit sharing)
}

// Output is the config to push and the status each object should get.
// Programmed conditions are added by the reconciler after the push.
type Output struct {
	Config   *config.Config
	Classes  map[string][]metav1.Condition
	Gateways map[types.NamespacedName]*GatewayResult
	Routes   map[types.NamespacedName][]gwv1.RouteParentStatus
	Policies map[types.NamespacedName][]metav1.Condition
}

// GatewayResult is a Gateway's computed status, minus Programmed.
type GatewayResult struct {
	Conditions []metav1.Condition
	Listeners  []gwv1.ListenerStatus
}

func cond(t, reason string, ok bool, gen int64, msg string) metav1.Condition {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen}
}

func nsName(o metav1.Object) types.NamespacedName {
	return types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}
}

// Translate builds the config and statuses. It never fails: problems become
// status conditions, and the config always validates.
func Translate(in Inputs) *Output {
	out := &Output{
		Classes:  map[string][]metav1.Condition{},
		Gateways: map[types.NamespacedName]*GatewayResult{},
		Routes:   map[types.NamespacedName][]gwv1.RouteParentStatus{},
		Policies: map[types.NamespacedName][]metav1.Condition{},
	}
	t := &translator{in: in, out: out, pools: map[string]*config.Pool{}, gateways: map[types.NamespacedName]*gwv1.Gateway{},
		routeOf: map[types.NamespacedName][]string{}, gatewayRoutes: map[types.NamespacedName][]string{}}
	t.index()
	ours := map[string]bool{}
	for _, c := range in.Classes {
		if c.Spec.ControllerName == ControllerName {
			ours[c.Name] = true
			out.Classes[c.Name] = []metav1.Condition{cond(string(gwv1.GatewayClassConditionStatusAccepted),
				string(gwv1.GatewayClassReasonAccepted), true, c.Generation, "handled by portcullis")}
		}
	}
	for i := range in.Gateways {
		g := &in.Gateways[i]
		if ours[string(g.Spec.GatewayClassName)] {
			t.gateway(g)
		}
	}
	for i := range sortedRoutes(in.Routes) {
		t.route(&in.Routes[i])
	}
	for gk, res := range out.Gateways {
		for i := range res.Listeners {
			res.Listeners[i].AttachedRoutes = t.attached[gk][string(res.Listeners[i].Name)]
		}
	}
	t.policies()
	out.Config = t.config()
	return out
}

func sortedRoutes(rs []gwv1.HTTPRoute) []gwv1.HTTPRoute {
	// Older routes win conflicts, as Gateway API asks; then by name.
	slices.SortStableFunc(rs, func(a, b gwv1.HTTPRoute) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return rs
}

type translator struct {
	in       Inputs
	out      *Output
	services map[types.NamespacedName]*corev1.Service
	slices   map[types.NamespacedName][]*discoveryv1.EndpointSlice
	gateways map[types.NamespacedName]*gwv1.Gateway // ours and accepted

	routes        []config.Route
	pools         map[string]*config.Pool
	extra         []config.Policy                   // controller-made policies (deny 500/503)
	routeOf       map[types.NamespacedName][]string // HTTPRoute -> portcullis route names
	gatewayRoutes map[types.NamespacedName][]string // Gateway -> portcullis route names
	attached      map[types.NamespacedName]map[string]int32
}

func (t *translator) index() {
	t.services = map[types.NamespacedName]*corev1.Service{}
	for i := range t.in.Services {
		t.services[nsName(&t.in.Services[i])] = &t.in.Services[i]
	}
	t.slices = map[types.NamespacedName][]*discoveryv1.EndpointSlice{}
	for i := range t.in.Slices {
		s := &t.in.Slices[i]
		if svc := s.Labels[discoveryv1.LabelServiceName]; svc != "" {
			k := types.NamespacedName{Namespace: s.Namespace, Name: svc}
			t.slices[k] = append(t.slices[k], s)
		}
	}
	t.attached = map[types.NamespacedName]map[string]int32{}
}

// gateway accepts HTTP listeners and rejects the others (HTTPS/TLS
// termination by the Gateway is not implemented yet).
func (t *translator) gateway(g *gwv1.Gateway) {
	res := &GatewayResult{}
	valid := 0
	for _, l := range g.Spec.Listeners {
		ls := gwv1.ListenerStatus{Name: l.Name, SupportedKinds: []gwv1.RouteGroupKind{{Group: ptr(gwv1.Group(gwv1.GroupName)), Kind: "HTTPRoute"}}}
		if l.Protocol == gwv1.HTTPProtocolType {
			valid++
			ls.Conditions = []metav1.Condition{
				cond(string(gwv1.ListenerConditionAccepted), string(gwv1.ListenerReasonAccepted), true, g.Generation, ""),
				cond(string(gwv1.ListenerConditionResolvedRefs), string(gwv1.ListenerReasonResolvedRefs), true, g.Generation, ""),
			}
		} else {
			ls.SupportedKinds = nil
			ls.Conditions = []metav1.Condition{
				cond(string(gwv1.ListenerConditionAccepted), string(gwv1.ListenerReasonUnsupportedProtocol), false, g.Generation,
					fmt.Sprintf("protocol %s is not supported; use HTTP", l.Protocol)),
				cond(string(gwv1.ListenerConditionResolvedRefs), string(gwv1.ListenerReasonResolvedRefs), true, g.Generation, ""),
			}
		}
		res.Listeners = append(res.Listeners, ls)
	}
	ok := valid > 0
	reason, msg := string(gwv1.GatewayReasonAccepted), ""
	switch {
	case !ok:
		reason, msg = string(gwv1.GatewayReasonListenersNotValid), "no listener with protocol HTTP"
	case valid < len(g.Spec.Listeners): // still Accepted, but the spec wants the reason to flag the bad listeners
		reason, msg = string(gwv1.GatewayReasonListenersNotValid), "some listeners are not HTTP and were not accepted"
	}
	res.Conditions = []metav1.Condition{cond(string(gwv1.GatewayConditionAccepted), reason, ok, g.Generation, msg)}
	t.out.Gateways[nsName(g)] = res
	if ok {
		t.gateways[nsName(g)] = g
	}
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// route translates one HTTPRoute for each of its parent Gateways that is ours.
func (t *translator) route(r *gwv1.HTTPRoute) {
	key := nsName(r)
	var statuses []gwv1.RouteParentStatus
	for _, pr := range r.Spec.ParentRefs {
		if deref(pr.Group, gwv1.GroupName) != gwv1.GroupName || deref(pr.Kind, "Gateway") != "Gateway" {
			continue
		}
		gk := types.NamespacedName{Namespace: string(deref(pr.Namespace, gwv1.Namespace(r.Namespace))), Name: string(pr.Name)}
		if _, known := t.out.Gateways[gk]; !known {
			continue // not a Gateway of ours: another controller reports on it
		}
		st := gwv1.RouteParentStatus{ParentRef: pr, ControllerName: ControllerName}
		g := t.gateways[gk]
		if g == nil {
			st.Conditions = []metav1.Condition{cond(string(gwv1.RouteConditionAccepted), string(gwv1.RouteReasonNoMatchingParent), false,
				r.Generation, "the parent Gateway is not accepted")}
			statuses = append(statuses, st)
			continue
		}
		hosts, listeners, reason := t.listenersFor(g, r, pr)
		if reason != "" {
			st.Conditions = []metav1.Condition{cond(string(gwv1.RouteConditionAccepted), reason, false, r.Generation, "no listener accepts this route")}
			statuses = append(statuses, st)
			continue
		}
		st.Conditions = t.rules(r, gk, hosts)
		if meta.IsStatusConditionTrue(st.Conditions, string(gwv1.RouteConditionAccepted)) {
			for _, l := range listeners {
				if t.attached[gk] == nil {
					t.attached[gk] = map[string]int32{}
				}
				t.attached[gk][l]++
			}
		}
		statuses = append(statuses, st)
	}
	if statuses != nil {
		t.out.Routes[key] = statuses
	}
}

// listenersFor returns the hostnames the route serves on g and the listeners
// it attaches to, or a reason it attaches to none.
func (t *translator) listenersFor(g *gwv1.Gateway, r *gwv1.HTTPRoute, pr gwv1.ParentReference) ([]string, []string, string) {
	var hosts, names []string
	anyHost := false
	reason := string(gwv1.RouteReasonNoMatchingParent)
	for _, l := range g.Spec.Listeners {
		if l.Protocol != gwv1.HTTPProtocolType || (pr.SectionName != nil && *pr.SectionName != l.Name) ||
			(pr.Port != nil && *pr.Port != l.Port) {
			continue
		}
		if !namespaceAllowed(l, g, r) {
			reason = string(gwv1.RouteReasonNotAllowedByListeners)
			continue
		}
		hs, ok := intersect(l.Hostname, r.Spec.Hostnames)
		if !ok {
			reason = string(gwv1.RouteReasonNoMatchingListenerHostname)
			continue
		}
		if hs == nil {
			anyHost = true
		}
		hosts = append(hosts, hs...)
		names = append(names, string(l.Name))
	}
	if len(names) == 0 {
		return nil, nil, reason
	}
	if anyHost {
		hosts = nil // some listener accepts every host
	}
	slices.Sort(hosts)
	return slices.Compact(hosts), names, ""
}

// shortcut: allowedRoutes "Selector" (namespace labels) is treated as not
// allowed; supporting it needs Namespace objects in Inputs.
func namespaceAllowed(l gwv1.Listener, g *gwv1.Gateway, r *gwv1.HTTPRoute) bool {
	from := gwv1.NamespacesFromSame
	if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.From != nil {
		from = *l.AllowedRoutes.Namespaces.From
	}
	switch from {
	case gwv1.NamespacesFromAll:
		return true
	case gwv1.NamespacesFromSame:
		return g.Namespace == r.Namespace
	}
	return false
}

// intersect returns the route hostnames that the listener hostname admits.
// nil with ok=true means "any host".
func intersect(listener *gwv1.Hostname, route []gwv1.Hostname) ([]string, bool) {
	if listener == nil || *listener == "" {
		if len(route) == 0 {
			return nil, true
		}
		out := make([]string, len(route))
		for i, h := range route {
			out[i] = string(h)
		}
		return out, true
	}
	l := string(*listener)
	if len(route) == 0 {
		return []string{l}, true
	}
	var out []string
	for _, h := range route {
		switch s := string(h); {
		case s == l, hostMatches(l, s):
			out = append(out, s) // the route's name is the narrower (or equal)
		case hostMatches(s, l):
			out = append(out, l) // the listener's name is the narrower
		}
	}
	return out, len(out) > 0
}

// hostMatches reports whether pattern (maybe "*.example.com") covers host.
func hostMatches(pattern, host string) bool {
	suffix, ok := strings.CutPrefix(pattern, "*")
	return ok && strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}

// rules translates the route's rules for one parent and returns the
// Accepted, ResolvedRefs (and if needed PartiallyInvalid) conditions.
func (t *translator) rules(r *gwv1.HTTPRoute, gk types.NamespacedName, hosts []string) []metav1.Condition {
	key := nsName(r)
	var unsupported, unresolved []string
	resolvedReason := string(gwv1.RouteReasonResolvedRefs)
	accepted := 0
	for ri, rule := range r.Spec.Rules {
		if why := unsupportedRule(rule); why != "" {
			unsupported = append(unsupported, fmt.Sprintf("rule %d: %s", ri, why))
			continue
		}
		split, problems, reason := t.backends(r, rule)
		if reason != "" {
			resolvedReason = reason
			unresolved = append(unresolved, problems...)
		}
		matches := rule.Matches
		if len(matches) == 0 {
			matches = []gwv1.HTTPRouteMatch{{}}
		}
		for mi, m := range matches {
			name := fmt.Sprintf("%s/%s/%s/%s/r%d/m%d", gk.Namespace, gk.Name, r.Namespace, r.Name, ri, mi)
			cr := config.Route{Name: name, Hosts: hosts, Path: "/", PathType: "Prefix"}
			if m.Path != nil {
				cr.Path = deref(m.Path.Value, "/")
				if deref(m.Path.Type, gwv1.PathMatchPathPrefix) == gwv1.PathMatchExact {
					cr.PathType = "Exact"
				}
			}
			if m.Method != nil {
				cr.Methods = []string{string(*m.Method)}
			}
			for _, h := range m.Headers {
				cr.Headers = append(cr.Headers, config.HeaderMatch{Name: string(h.Name), Value: h.Value})
			}
			if len(split) == 0 { // every backend is invalid or has no endpoints
				status, msg := 500, "no valid backend"
				if reason == "" {
					status, msg = 503, "no ready endpoints"
				}
				cr.Backends = []config.PoolRef{{Pool: sentinelPool}}
				t.extra = append(t.extra, config.Policy{Routes: []string{name},
					Script: fmt.Sprintf("policy %q { on request { deny(%d, %q) } }", "portcullis:"+name, status, msg)})
			} else {
				cr.Backends = split
			}
			t.routes = append(t.routes, cr)
			t.routeOf[key] = append(t.routeOf[key], name)
			t.gatewayRoutes[gk] = append(t.gatewayRoutes[gk], name)
		}
		accepted++
	}
	var conds []metav1.Condition
	if accepted == 0 && len(r.Spec.Rules) > 0 {
		conds = append(conds, cond(string(gwv1.RouteConditionAccepted), string(gwv1.RouteReasonUnsupportedValue), false, r.Generation,
			strings.Join(unsupported, "; ")))
	} else {
		conds = append(conds, cond(string(gwv1.RouteConditionAccepted), string(gwv1.RouteReasonAccepted), true, r.Generation, ""))
	}
	if len(unsupported) > 0 && accepted > 0 {
		conds = append(conds, cond(string(gwv1.RouteConditionPartiallyInvalid), string(gwv1.RouteReasonUnsupportedValue), true, r.Generation,
			"dropped: "+strings.Join(unsupported, "; ")))
	}
	conds = append(conds, cond(string(gwv1.RouteConditionResolvedRefs), resolvedReason, len(unresolved) == 0, r.Generation,
		strings.Join(unresolved, "; ")))
	return conds
}

// unsupportedRule names the first feature of rule that Portcullis cannot honour.
// Dropping the rule is safer than serving it without, say, its header filter.
func unsupportedRule(rule gwv1.HTTPRouteRule) string {
	if len(rule.Filters) > 0 {
		return "filters are not supported yet"
	}
	for _, br := range rule.BackendRefs {
		if len(br.Filters) > 0 {
			return "backendRef filters are not supported"
		}
	}
	for _, m := range rule.Matches {
		if m.Path != nil && deref(m.Path.Type, gwv1.PathMatchPathPrefix) == gwv1.PathMatchRegularExpression {
			return "RegularExpression path matches are not supported"
		}
		if len(m.QueryParams) > 0 {
			return "query parameter matches are not supported"
		}
		for _, h := range m.Headers {
			if deref(h.Type, gwv1.HeaderMatchExact) != gwv1.HeaderMatchExact {
				return "RegularExpression header matches are not supported"
			}
		}
	}
	return ""
}

// backends resolves a rule's backendRefs to weighted pools. A ref that is
// invalid (reason set) or whose Service has no ready endpoints is left out of
// the split.
func (t *translator) backends(r *gwv1.HTTPRoute, rule gwv1.HTTPRouteRule) ([]config.PoolRef, []string, string) {
	var split []config.PoolRef
	var problems []string
	reason := ""
	fail := func(rs gwv1.RouteConditionReason, format string, a ...any) {
		reason = string(rs)
		problems = append(problems, fmt.Sprintf(format, a...))
	}
	for _, br := range rule.BackendRefs {
		ref := br.BackendObjectReference
		if deref(ref.Group, "") != "" || deref(ref.Kind, "Service") != "Service" {
			fail(gwv1.RouteReasonInvalidKind, "backend %s: only core Services are supported", ref.Name)
			continue
		}
		ns := string(deref(ref.Namespace, gwv1.Namespace(r.Namespace)))
		if ns != r.Namespace { // shortcut: ReferenceGrant is not implemented, so cross-namespace refs are refused
			fail(gwv1.RouteReasonRefNotPermitted, "backend %s/%s: cross-namespace references need a ReferenceGrant, which is not supported", ns, ref.Name)
			continue
		}
		svc := t.services[types.NamespacedName{Namespace: ns, Name: string(ref.Name)}]
		if svc == nil {
			fail(gwv1.RouteReasonBackendNotFound, "service %s/%s not found", ns, ref.Name)
			continue
		}
		if ref.Port == nil {
			fail(gwv1.RouteReasonUnsupportedValue, "backend %s: port is required", ref.Name)
			continue
		}
		port := servicePort(svc, *ref.Port)
		if port == nil {
			fail(gwv1.RouteReasonBackendNotFound, "service %s/%s has no port %d", ns, ref.Name, *ref.Port)
			continue
		}
		name := fmt.Sprintf("%s/%s:%d", ns, svc.Name, port.Port)
		pool := t.pool(name, svc, port)
		w := int(deref(br.Weight, 1))
		if pool == nil || w == 0 {
			continue
		}
		split = append(split, config.PoolRef{Pool: name, Weight: &w})
	}
	return split, problems, reason
}

func servicePort(svc *corev1.Service, port int32) *corev1.ServicePort {
	for i := range svc.Spec.Ports {
		if p := &svc.Spec.Ports[i]; p.Port == port && cmp.Or(p.Protocol, corev1.ProtocolTCP) == corev1.ProtocolTCP {
			return p
		}
	}
	return nil
}

// pool returns the pool for a Service port, built from its ready endpoints,
// or nil if there are none.
func (t *translator) pool(name string, svc *corev1.Service, sp *corev1.ServicePort) *config.Pool {
	if p, ok := t.pools[name]; ok {
		return p
	}
	var addrs []string
	for _, s := range t.slices[nsName(svc)] {
		var port int32
		for _, ep := range s.Ports {
			if deref(ep.Name, "") == sp.Name && deref(ep.Protocol, corev1.ProtocolTCP) == corev1.ProtocolTCP && ep.Port != nil {
				port = *ep.Port
			}
		}
		if port == 0 {
			continue
		}
		for _, e := range s.Endpoints {
			if !deref(e.Conditions.Ready, true) || deref(e.Conditions.Terminating, false) {
				continue
			}
			for _, a := range e.Addresses {
				addrs = append(addrs, net.JoinHostPort(a, strconv.Itoa(int(port))))
			}
		}
	}
	slices.Sort(addrs)
	addrs = slices.Compact(addrs)
	if len(addrs) == 0 {
		t.pools[name] = nil
		return nil
	}
	p := &config.Pool{Name: name}
	for _, a := range addrs {
		p.Backends = append(p.Backends, config.Backend{Address: a})
	}
	t.pools[name] = p
	return p
}

// policies validates each PortcullisPolicy on its own, so one bad script never
// blocks the others, and attaches the good ones to their target's routes.
func (t *translator) policies() {
	ps := slices.Clone(t.in.Policies)
	// Gateway-wide policies run before route policies; then by namespace/name.
	slices.SortFunc(ps, func(a, b v1alpha1.PortcullisPolicy) int {
		if a.Spec.TargetRef.Kind != b.Spec.TargetRef.Kind {
			return cmp.Compare(kindOrder(a.Spec.TargetRef.Kind), kindOrder(b.Spec.TargetRef.Kind))
		}
		return cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	poolNames := map[string]bool{}
	for n, p := range t.pools {
		if p != nil {
			poolNames[n] = true
		}
	}
	names := map[string]types.NamespacedName{}
	for i := range ps {
		p := &ps[i]
		key := nsName(p)
		set := func(reason string, ok bool, msg string) {
			t.out.Policies[key] = []metav1.Condition{cond("Accepted", reason, ok, p.Generation, msg)}
		}
		target := types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.TargetRef.Name}
		var routes []string
		var found bool
		switch p.Spec.TargetRef.Kind {
		case "Gateway":
			_, found = t.gateways[target]
			routes = t.gatewayRoutes[target]
		case "HTTPRoute":
			routes, found = t.routeOf[target]
			if !found {
				_, found = t.out.Routes[target] // exists but has no translated rules
			}
		}
		if !found {
			set("TargetNotFound", false, fmt.Sprintf("%s %s is not found or not handled by portcullis", p.Spec.TargetRef.Kind, target))
			continue
		}
		prog, err := policy.Compile(p.Spec.Script, policy.Options{Pools: poolNames})
		if err != nil {
			set("Invalid", false, err.Error())
			continue
		}
		if prev, dup := names[prog.Name]; dup {
			set("Conflicted", false, fmt.Sprintf("policy name %q is already used by %s", prog.Name, prev))
			continue
		}
		names[prog.Name] = key
		set("Accepted", true, fmt.Sprintf("attached to %d route(s)", len(routes)))
		if len(routes) == 0 {
			continue
		}
		t.extra = append(t.extra, config.Policy{Script: p.Spec.Script, Routes: routes, OnError: p.Spec.OnError})
	}
}

func kindOrder(k string) int {
	if k == "Gateway" {
		return 0
	}
	return 1
}

// config assembles the final config and validates it. A failure here would be
// a translation bug: it is reported on every Gateway rather than pushed.
func (t *translator) config() *config.Config {
	c := &config.Config{Instances: max(1, t.in.Instances), Routes: t.routes, Policies: t.extra}
	// The sentinel is never dialled (a deny always runs first); port 9 is "discard".
	c.Pools = append(c.Pools, config.Pool{Name: sentinelPool, Backends: []config.Backend{{Address: "127.0.0.1:9"}}})
	names := make([]string, 0, len(t.pools))
	for n, p := range t.pools {
		if p != nil {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		c.Pools = append(c.Pools, *t.pools[n])
	}
	if len(c.Routes) == 0 {
		c.Routes = []config.Route{{Name: "portcullis:no-routes", Backends: []config.PoolRef{{Pool: sentinelPool}}}}
		c.Policies = append(c.Policies, config.Policy{Routes: []string{"portcullis:no-routes"},
			Script: `policy "portcullis:no-routes" { on request { deny(404, "no HTTPRoute is attached") } }`})
	}
	return c
}

// ErrNoDataPlane is returned when there is no data-plane pod to push to.
var ErrNoDataPlane = errors.New("no data-plane pods found")
