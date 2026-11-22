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
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/controller/v1alpha1"
	"github.com/AaronRaj06/portcullis/internal/policy"
)

// ControllerName is the GatewayClass spec.controllerName this controller serves.
const ControllerName gwv1.GatewayController = "portcullis.dev/gateway-controller"

// Pools that only answer a status. Service pools are named namespace/name:port,
// so none can collide with these, and no policy may route_to them.
const (
	pool404 = "portcullis-404"
	pool500 = "portcullis-500"
	pool503 = "portcullis-503"
)

// Inputs is everything the translation reads, already listed from the cluster.
type Inputs struct {
	Classes    []gwv1.GatewayClass
	Gateways   []gwv1.Gateway
	Routes     []gwv1.HTTPRoute
	Services   []corev1.Service
	Slices     []discoveryv1.EndpointSlice
	Policies   []v1alpha1.PortcullisPolicy
	Namespaces []corev1.Namespace    // for allowedRoutes namespace selectors
	Grants     []gwv1.ReferenceGrant // for cross-namespace backendRefs
	Instances  int                   // data-plane replicas (for rate-limit sharing)

	Secrets         map[types.NamespacedName]*corev1.Secret // the Secrets named by secretRefs
	SystemNamespace string                                  // where the Gateway Services are
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

	Service      string // this Gateway's own Service
	Ports        []GatewayPort
	Programmable map[gwv1.SectionName]bool // the listeners the data plane can serve
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
	var gws []*gwv1.Gateway
	for i := range in.Gateways {
		if ours[string(in.Gateways[i].Spec.GatewayClassName)] {
			gws = append(gws, &in.Gateways[i])
		}
	}
	// A fixed order, so port allocation does not depend on the list's order.
	slices.SortFunc(gws, func(a, b *gwv1.Gateway) int { return cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name) })
	t.reservePorts()
	for _, g := range gws {
		t.gateway(g)
	}
	for i := range sortedRoutes(in.Routes) {
		t.route(&in.Routes[i])
	}
	for gk, res := range out.Gateways {
		for i := range res.Listeners {
			res.Listeners[i].AttachedRoutes = int32(len(t.attached[gk][res.Listeners[i].Name])) //nolint:gosec // bounded by the number of routes
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
	nsLabels map[string]labels.Set

	listeners  map[types.NamespacedName][]*listener
	usedPorts  map[int]bool
	knownPorts map[string]int // Service/port name -> target port

	routes        []config.Route
	pools         map[string]*config.Pool
	policyCfg     []config.Policy
	closed        map[string]bool                   // routes that answer 500: a policy they need cannot run
	routeOf       map[types.NamespacedName][]string // HTTPRoute -> portcullis route names
	gatewayRoutes map[types.NamespacedName][]string // Gateway -> portcullis route names
	attached      map[types.NamespacedName]map[gwv1.SectionName]map[types.NamespacedName]bool
}

func (t *translator) index() {
	t.services = map[types.NamespacedName]*corev1.Service{}
	for i := range t.in.Services {
		t.services[nsName(&t.in.Services[i])] = &t.in.Services[i]
	}
	t.slices = map[types.NamespacedName][]*discoveryv1.EndpointSlice{}
	t.nsLabels = map[string]labels.Set{}
	for _, n := range t.in.Namespaces {
		t.nsLabels[n.Name] = n.Labels
	}
	for i := range t.in.Slices {
		s := &t.in.Slices[i]
		if svc := s.Labels[discoveryv1.LabelServiceName]; svc != "" {
			k := types.NamespacedName{Namespace: s.Namespace, Name: svc}
			t.slices[k] = append(t.slices[k], s)
		}
	}
	t.attached = map[types.NamespacedName]map[gwv1.SectionName]map[types.NamespacedName]bool{}
	t.listeners = map[types.NamespacedName][]*listener{}
	t.usedPorts, t.knownPorts, t.closed = map[int]bool{}, map[string]int{}, map[string]bool{}
}

// allowedKinds is the listener's allowedRoutes.kinds; empty means "the
// protocol's default", which for HTTP is HTTPRoute.
func allowedKinds(l gwv1.Listener) []gwv1.RouteGroupKind {
	if l.AllowedRoutes == nil {
		return nil
	}
	return l.AllowedRoutes.Kinds
}

func isHTTPRoute(k gwv1.RouteGroupKind) bool {
	return deref(k.Group, gwv1.GroupName) == gwv1.GroupName && k.Kind == "HTTPRoute"
}

func allowsHTTPRoute(l gwv1.Listener) bool {
	ks := allowedKinds(l)
	return len(ks) == 0 || slices.ContainsFunc(ks, isHTTPRoute)
}

func invalidKinds(l gwv1.Listener) bool {
	return slices.ContainsFunc(allowedKinds(l), func(k gwv1.RouteGroupKind) bool { return !isHTTPRoute(k) })
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
	for pi, pr := range r.Spec.ParentRefs {
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
				r.Generation, "the parent Gateway is not accepted"), t.resolvedRefs(r)}
			statuses = append(statuses, st)
			continue
		}
		ports, names, reason := t.listenersFor(g, r, pr)
		if reason != "" {
			st.Conditions = []metav1.Condition{cond(string(gwv1.RouteConditionAccepted), reason, false, r.Generation, "no listener accepts this route"),
				t.resolvedRefs(r)}
			statuses = append(statuses, st)
			continue
		}
		st.Conditions = t.rules(r, gk, pi, ports)
		if meta.IsStatusConditionTrue(st.Conditions, string(gwv1.RouteConditionAccepted)) {
			if t.attached[gk] == nil {
				t.attached[gk] = map[gwv1.SectionName]map[types.NamespacedName]bool{}
			}
			for _, l := range names {
				if t.attached[gk][l] == nil {
					t.attached[gk][l] = map[types.NamespacedName]bool{}
				}
				t.attached[gk][l][key] = true // a set: several parentRefs may reach one listener
			}
		}
		statuses = append(statuses, st)
	}
	if statuses != nil {
		t.out.Routes[key] = statuses
	}
}

// binding is where a route is served on one port of a Gateway.
type binding struct {
	target, port int
	scheme       string
	hosts        []string // nil: any
	anyHost      bool
}

// listenersFor returns where the route is served on g and the listeners it
// attaches to, or a reason it attaches to none. A route can attach to a listener
// with a bad certificate; it just is not served there.
func (t *translator) listenersFor(g *gwv1.Gateway, r *gwv1.HTTPRoute, pr gwv1.ParentReference) ([]*binding, []gwv1.SectionName, string) {
	var out []*binding
	var names []gwv1.SectionName
	reason := string(gwv1.RouteReasonNoMatchingParent)
	for _, l := range t.listeners[nsName(g)] {
		if (pr.SectionName != nil && *pr.SectionName != l.Name) || (pr.Port != nil && *pr.Port != l.Port) {
			continue
		}
		if !allowsHTTPRoute(l.Listener) || !t.namespaceAllowed(l.Listener, g, r) {
			reason = string(gwv1.RouteReasonNotAllowedByListeners)
			continue
		}
		hs, ok := intersect(l.Hostname, r.Spec.Hostnames)
		if !ok {
			reason = string(gwv1.RouteReasonNoMatchingListenerHostname)
			continue
		}
		names = append(names, l.Name)
		if !l.serves {
			continue
		}
		i := slices.IndexFunc(out, func(b *binding) bool { return b.target == l.target })
		if i < 0 {
			i = len(out)
			out = append(out, &binding{target: l.target, port: int(l.Port), scheme: strings.ToLower(string(l.Protocol))})
		}
		out[i].anyHost = out[i].anyHost || hs == nil
		out[i].hosts = append(out[i].hosts, hs...)
	}
	if len(names) == 0 {
		return nil, nil, reason
	}
	for _, b := range out {
		if b.anyHost {
			b.hosts = nil // some listener on this port accepts every host
		}
		slices.Sort(b.hosts)
		b.hosts = slices.Compact(b.hosts)
	}
	return out, names, ""
}

func (t *translator) namespaceAllowed(l gwv1.Listener, g *gwv1.Gateway, r *gwv1.HTTPRoute) bool {
	from := gwv1.NamespacesFromSame
	var ns *gwv1.RouteNamespaces
	if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil {
		ns = l.AllowedRoutes.Namespaces
		from = deref(ns.From, from)
	}
	switch from {
	case gwv1.NamespacesFromAll:
		return true
	case gwv1.NamespacesFromSame:
		return g.Namespace == r.Namespace
	case gwv1.NamespacesFromSelector:
		if ns.Selector == nil {
			return false
		}
		sel, err := metav1.LabelSelectorAsSelector(ns.Selector)
		return err == nil && sel.Matches(t.nsLabels[r.Namespace])
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
//
// A rule with a match we cannot express is dropped. A rule with a filter we
// cannot honour answers 500: dropping it would let its requests fall through
// to a broader rule without the filter.
func (t *translator) rules(r *gwv1.HTTPRoute, gk types.NamespacedName, pi int, ports []*binding) []metav1.Condition {
	key := nsName(r)
	var unsupported, unresolved []string
	resolvedReason := string(gwv1.RouteReasonResolvedRefs)
	accepted := 0
	for _, rule := range r.Spec.Rules {
		if _, _, why := ruleFilters(rule); why == "" && unsupportedMatch(rule) == "" {
			accepted++
		}
	}
	for ri, rule := range r.Spec.Rules {
		if why := unsupportedMatch(rule); why != "" {
			unsupported = append(unsupported, fmt.Sprintf("rule %d is dropped: %s", ri, why))
			continue
		}
		var split []config.PoolRef
		edit, redir, why := ruleFilters(rule)
		if why != "" {
			unsupported = append(unsupported, fmt.Sprintf("rule %d answers 500: %s", ri, why))
			if accepted == 0 {
				continue // a route that is not accepted serves nothing
			}
			split = []config.PoolRef{{Pool: pool500}}
		} else {
			var problems []string
			var reason string
			split, problems, reason = t.backends(r, rule)
			if reason != "" {
				resolvedReason = reason
				unresolved = append(unresolved, problems...)
			}
		}
		matches := rule.Matches
		if len(matches) == 0 {
			matches = []gwv1.HTTPRouteMatch{{}}
		}
		for _, b := range ports {
			var rd *config.Redirect
			if redir != nil {
				rd = &config.Redirect{Scheme: b.scheme, Port: b.port, Host: redir.Host, Status: redir.Status}
			}
			for mi, m := range matches {
				name := fmt.Sprintf("%s/%s:%d/%s/%s/r%d/m%d", gk.Namespace, gk.Name, b.port, r.Namespace, r.Name, ri, mi)
				if pi > 0 { // parentRefs can name one Gateway several times (by sectionName or port)
					name += fmt.Sprintf("/p%d", pi)
				}
				cr := config.Route{Name: name, Port: b.target, Hosts: b.hosts, Path: "/", PathType: "Prefix",
					Backends: split, RequestHeaders: edit, Redirect: rd}
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
				t.routes = append(t.routes, cr)
				t.routeOf[key] = append(t.routeOf[key], name)
				t.gatewayRoutes[gk] = append(t.gatewayRoutes[gk], name)
			}
		}
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
			strings.Join(unsupported, "; ")))
	}
	conds = append(conds, cond(string(gwv1.RouteConditionResolvedRefs), resolvedReason, len(unresolved) == 0, r.Generation,
		strings.Join(unresolved, "; ")))
	return conds
}

// unsupportedMatch names the first match feature of rule that Portcullis cannot express.
func unsupportedMatch(rule gwv1.HTTPRouteRule) string {
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

// backends resolves a rule's backendRefs to weighted pools. An invalid ref
// keeps its share and answers 500 for it. A Service without ready endpoints
// gives its share to the others, so a rollout does not turn into errors; if
// none is left the rule answers 503.
func (t *translator) backends(r *gwv1.HTTPRoute, rule gwv1.HTTPRouteRule) ([]config.PoolRef, []string, string) {
	var split []config.PoolRef
	var problems []string
	reason, noEndpoints := "", false
	for _, br := range rule.BackendRefs {
		w := int(deref(br.Weight, 1))
		name := pool500
		svc, port, rs, problem := t.resolve(r, br.BackendObjectReference)
		if rs != "" {
			reason = string(rs)
			problems = append(problems, problem)
		} else {
			name = fmt.Sprintf("%s/%s:%d", svc.Namespace, svc.Name, port.Port)
			if t.pool(name, svc, port) == nil {
				noEndpoints = noEndpoints || w > 0
				continue
			}
		}
		if w > 0 {
			split = append(split, config.PoolRef{Pool: name, Weight: &w})
		}
	}
	if len(split) == 0 {
		status := pool500 // no backendRefs, or every weight is 0
		if noEndpoints {
			status = pool503
		}
		split = []config.PoolRef{{Pool: status}}
	}
	return split, problems, reason
}

// resolve checks one backendRef and returns its Service and port, or the
// ResolvedRefs reason and message why it cannot be used.
func (t *translator) resolve(r *gwv1.HTTPRoute, ref gwv1.BackendObjectReference) (*corev1.Service, *corev1.ServicePort, gwv1.RouteConditionReason, string) {
	if deref(ref.Group, "") != "" || deref(ref.Kind, "Service") != "Service" {
		return nil, nil, gwv1.RouteReasonInvalidKind, fmt.Sprintf("backend %s: only core Services are supported", ref.Name)
	}
	ns := string(deref(ref.Namespace, gwv1.Namespace(r.Namespace)))
	if ns != r.Namespace && !granted(t.in.Grants, "HTTPRoute", r.Namespace, "Service", ns, string(ref.Name)) {
		return nil, nil, gwv1.RouteReasonRefNotPermitted, fmt.Sprintf("backend %s/%s: no ReferenceGrant allows this cross-namespace reference", ns, ref.Name)
	}
	svc := t.services[types.NamespacedName{Namespace: ns, Name: string(ref.Name)}]
	if svc == nil {
		return nil, nil, gwv1.RouteReasonBackendNotFound, fmt.Sprintf("service %s/%s not found", ns, ref.Name)
	}
	if ref.Port == nil {
		return nil, nil, gwv1.RouteReasonUnsupportedValue, fmt.Sprintf("backend %s: port is required", ref.Name)
	}
	port := servicePort(svc, *ref.Port)
	if port == nil {
		return nil, nil, gwv1.RouteReasonBackendNotFound, fmt.Sprintf("service %s/%s has no port %d", ns, ref.Name, *ref.Port)
	}
	return svc, port, "", ""
}

// granted reports whether a ReferenceGrant in toNS lets fromKind objects in
// fromNS reference the core toKind object name.
func granted(grants []gwv1.ReferenceGrant, fromKind gwv1.Kind, fromNS string, toKind gwv1.Kind, toNS, name string) bool {
	for _, g := range grants {
		if g.Namespace != toNS {
			continue
		}
		from := slices.ContainsFunc(g.Spec.From, func(f gwv1.ReferenceGrantFrom) bool {
			return f.Group == gwv1.GroupName && f.Kind == fromKind && string(f.Namespace) == fromNS
		})
		to := slices.ContainsFunc(g.Spec.To, func(to gwv1.ReferenceGrantTo) bool {
			return to.Group == "" && to.Kind == toKind && (to.Name == nil || string(*to.Name) == name)
		})
		if from && to {
			return true
		}
	}
	return false
}

// resolvedRefs is the ResolvedRefs condition for a route that attached to no
// listener: the spec still wants it reported.
func (t *translator) resolvedRefs(r *gwv1.HTTPRoute) metav1.Condition {
	reason, ok, msgs := string(gwv1.RouteReasonResolvedRefs), true, []string(nil)
	for _, rule := range r.Spec.Rules {
		for _, br := range rule.BackendRefs {
			if _, _, rs, problem := t.resolve(r, br.BackendObjectReference); rs != "" {
				reason, ok, msgs = string(rs), false, append(msgs, problem)
			}
		}
	}
	return cond(string(gwv1.RouteConditionResolvedRefs), reason, ok, r.Generation, strings.Join(msgs, "; "))
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
				if !dialable(a) {
					continue
				}
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

// dialable refuses endpoint addresses that would point the proxy at itself, at
// the node's link-local services (cloud metadata) or at a name of the author's choice.
func dialable(addr string) bool {
	ip, err := netip.ParseAddr(addr)
	if err != nil || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	return !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsMulticast()
}

// policies validates each PortcullisPolicy on its own and attaches it to its
// target's routes. A policy is identified by namespace/name and may only
// route_to pools of Services in its own namespace. One that cannot run closes
// its target's routes (500) unless its onError is fail_open: a typo in a deny
// rule must not remove the deny rule.
func (t *translator) policies() {
	ps := slices.Clone(t.in.Policies)
	// Gateway-wide policies run before route policies; then by namespace/name.
	slices.SortFunc(ps, func(a, b v1alpha1.PortcullisPolicy) int {
		if a.Spec.TargetRef.Kind != b.Spec.TargetRef.Kind {
			return cmp.Compare(kindOrder(a.Spec.TargetRef.Kind), kindOrder(b.Spec.TargetRef.Kind))
		}
		return cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	poolsIn := map[string]map[string]bool{} // namespace -> its pools, including those without endpoints
	for n := range t.pools {
		ns, _, _ := strings.Cut(n, "/")
		if poolsIn[ns] == nil {
			poolsIn[ns] = map[string]bool{}
		}
		poolsIn[ns][n] = true
	}
	onRoute := map[string]int{}
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
		pools := poolsIn[p.Namespace]
		if pools == nil {
			pools = map[string]bool{} // nil would mean "any pool" to policy.Compile
		}
		if _, err := policy.Compile(p.Spec.Script, policy.Options{Pools: pools, Share: t.in.Instances}); err != nil {
			msg := err.Error()
			if p.Spec.OnError != "fail_open" {
				for _, r := range routes {
					t.closed[r] = true
				}
				if len(routes) > 0 {
					msg += fmt.Sprintf(" (%d route(s) answer 500 until this is fixed, because onError is fail_closed)", len(routes))
				}
			}
			set("Invalid", false, msg)
			continue
		}
		var runs []string
		for _, r := range routes {
			if onRoute[r] == config.MaxPolicies {
				t.closed[r] = true
				continue
			}
			onRoute[r]++
			runs = append(runs, r)
		}
		if over := len(routes) - len(runs); over > 0 {
			set("TooManyPolicies", false, fmt.Sprintf("%d route(s) are over %d policies and answer 500", over, config.MaxPolicies))
		} else {
			set("Accepted", true, fmt.Sprintf("attached to %d route(s)", len(routes)))
		}
		if len(runs) > 0 {
			t.policyCfg = append(t.policyCfg, config.Policy{ID: key.String(), Script: p.Spec.Script, Routes: runs, OnError: p.Spec.OnError})
		}
	}
}

func kindOrder(k string) int {
	if k == "Gateway" {
		return 0
	}
	return 1
}

// config assembles the final config. NewSnapshot validates it: a failure
// there would be a translation bug, reported on every Gateway rather than pushed.
func (t *translator) config() *config.Config {
	c := &config.Config{Instances: max(1, t.in.Instances), Routes: t.routes, Listeners: t.configListeners()}
	for i := range c.Routes {
		if r := &c.Routes[i]; t.closed[r.Name] {
			r.Backends, r.Redirect = []config.PoolRef{{Pool: pool500}}, nil
		}
	}
	for _, p := range t.policyCfg {
		p.Routes = slices.DeleteFunc(slices.Clone(p.Routes), func(r string) bool { return t.closed[r] })
		if len(p.Routes) > 0 { // none listed would mean every route
			c.Policies = append(c.Policies, p)
		}
	}
	for _, s := range []struct {
		name   string
		status int
	}{{pool404, 404}, {pool500, 500}, {pool503, 503}} {
		c.Pools = append(c.Pools, config.Pool{Name: s.name, Status: s.status})
	}
	for _, n := range slices.Sorted(maps.Keys(t.pools)) {
		if p := t.pools[n]; p != nil {
			c.Pools = append(c.Pools, *p)
		} else {
			c.Pools = append(c.Pools, config.Pool{Name: n, Status: 503}) // no endpoints: still a valid route_to target
		}
	}
	if len(c.Routes) == 0 {
		c.Routes = []config.Route{{Name: "portcullis:no-routes", Backends: []config.PoolRef{{Pool: pool404}}}}
	}
	return c
}

// ErrNoDataPlane is returned when there is no data-plane pod to push to.
var ErrNoDataPlane = errors.New("no data-plane pods found")
