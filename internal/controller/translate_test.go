package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/controller/v1alpha1"
)

func om(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1}
}

func class() gwv1.GatewayClass {
	return gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "portcullis"}, Spec: gwv1.GatewayClassSpec{ControllerName: ControllerName}}
}

func gateway(listeners ...gwv1.Listener) gwv1.Gateway {
	if len(listeners) == 0 {
		listeners = []gwv1.Listener{{Name: "http", Port: 80, Protocol: gwv1.HTTPProtocolType}}
	}
	return gwv1.Gateway{ObjectMeta: om("default", "gw"), Spec: gwv1.GatewaySpec{GatewayClassName: "portcullis", Listeners: listeners}}
}

func service(name string, port int32) corev1.Service {
	return corev1.Service{ObjectMeta: om("default", name), Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: port}}}}
}

func slice(svc string, port int32, ready bool, ips ...string) discoveryv1.EndpointSlice {
	s := discoveryv1.EndpointSlice{ObjectMeta: om("default", svc+"-abc"), Ports: []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(port)}}}
	s.Labels = map[string]string{discoveryv1.LabelServiceName: svc}
	for _, ip := range ips {
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: ptr(ready)}})
	}
	return s
}

func backendRef(name string, port int32, weight int32) gwv1.HTTPBackendRef {
	return gwv1.HTTPBackendRef{BackendRef: gwv1.BackendRef{
		BackendObjectReference: gwv1.BackendObjectReference{Name: gwv1.ObjectName(name), Port: ptr(port)},
		Weight:                 ptr(weight)}}
}

func httpRoute(name string, hosts []gwv1.Hostname, rules ...gwv1.HTTPRouteRule) gwv1.HTTPRoute {
	return gwv1.HTTPRoute{ObjectMeta: om("default", name), Spec: gwv1.HTTPRouteSpec{
		CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw"}}},
		Hostnames:       hosts, Rules: rules}}
}

// mustValidate: whatever the controller produces must pass the data plane's own validation.
func mustValidate(t *testing.T, c *config.Config) {
	t.Helper()
	if err := c.Validate(); err != nil {
		t.Fatalf("translated config is invalid: %v", err)
	}
}

func condition(t *testing.T, cs []metav1.Condition, typ string) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(cs, typ)
	if c == nil {
		t.Fatalf("no %s condition in %+v", typ, cs)
	}
	return *c
}

func TestCanarySplitWithEndpoints(t *testing.T) {
	in := Inputs{
		Classes:  []gwv1.GatewayClass{class()},
		Gateways: []gwv1.Gateway{gateway()},
		Services: []corev1.Service{service("v1", 80), service("v2", 80)},
		Slices: []discoveryv1.EndpointSlice{slice("v1", 8080, true, "10.0.0.1", "10.0.0.2"), slice("v2", 8080, true, "10.0.0.3"),
			slice("v1", 8080, false, "10.0.0.9")}, // not ready: left out
		Routes: []gwv1.HTTPRoute{httpRoute("app", []gwv1.Hostname{"app.example.com"}, gwv1.HTTPRouteRule{
			BackendRefs: []gwv1.HTTPBackendRef{backendRef("v1", 80, 90), backendRef("v2", 80, 10)}})},
		Instances: 2,
	}
	out := Translate(in)
	mustValidate(t, out.Config)
	if out.Config.Instances != 2 || len(out.Config.Routes) != 1 {
		t.Fatalf("config: %+v", out.Config)
	}
	r := out.Config.Routes[0]
	if r.Name != "default/gw/default/app/r0/m0" || r.Hosts[0] != "app.example.com" || r.Path != "/" ||
		r.Backends[0].Pool != "default/v1:80" || *r.Backends[0].Weight != 90 || *r.Backends[1].Weight != 10 {
		t.Fatalf("route: %+v", r)
	}
	var v1 *config.Pool
	for i := range out.Config.Pools {
		if out.Config.Pools[i].Name == "default/v1:80" {
			v1 = &out.Config.Pools[i]
		}
	}
	if v1 == nil || len(v1.Backends) != 2 || v1.Backends[0].Address != "10.0.0.1:8080" {
		t.Fatalf("pool v1: %+v", v1)
	}
	st := out.Routes[types.NamespacedName{Namespace: "default", Name: "app"}]
	if len(st) != 1 || condition(t, st[0].Conditions, "Accepted").Status != metav1.ConditionTrue ||
		condition(t, st[0].Conditions, "ResolvedRefs").Status != metav1.ConditionTrue || st[0].ControllerName != ControllerName {
		t.Fatalf("route status: %+v", st)
	}
	gw := out.Gateways[types.NamespacedName{Namespace: "default", Name: "gw"}]
	if condition(t, gw.Conditions, "Accepted").Status != metav1.ConditionTrue || len(gw.Listeners) != 1 {
		t.Fatalf("gateway: %+v", gw)
	}
}

func TestMatchesAndRejections(t *testing.T) {
	exact := gwv1.PathMatchExact
	regex := gwv1.PathMatchRegularExpression
	get := gwv1.HTTPMethodGet
	other := gwv1.Namespace("other")
	in := Inputs{
		Classes:  []gwv1.GatewayClass{class()},
		Gateways: []gwv1.Gateway{gateway()},
		Services: []corev1.Service{service("web", 80)},
		Slices:   []discoveryv1.EndpointSlice{slice("web", 8080, true, "10.0.0.1")},
		Routes: []gwv1.HTTPRoute{
			httpRoute("mixed", nil,
				gwv1.HTTPRouteRule{
					Matches: []gwv1.HTTPRouteMatch{
						{Path: &gwv1.HTTPPathMatch{Type: &exact, Value: ptr("/login")}, Method: &get},
						{Headers: []gwv1.HTTPHeaderMatch{{Name: "X-Beta", Value: "1"}}},
					},
					BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80, 1)}},
				gwv1.HTTPRouteRule{ // unsupported: dropped, route is PartiallyInvalid
					Matches:     []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: &regex, Value: ptr("/a.*")}}},
					BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80, 1)}},
			),
			httpRoute("bad-refs", nil, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{
				{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "web", Namespace: &other, Port: ptr(gwv1.PortNumber(80))}}},
			}}),
			httpRoute("missing", nil, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("nope", 80, 1)}}),
		},
	}
	out := Translate(in)
	mustValidate(t, out.Config)

	mixed := out.Routes[types.NamespacedName{Namespace: "default", Name: "mixed"}][0].Conditions
	if c := condition(t, mixed, "PartiallyInvalid"); c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "RegularExpression") {
		t.Fatalf("partially invalid: %+v", c)
	}
	var names []string
	for _, r := range out.Config.Routes {
		names = append(names, r.Name)
		if r.Name == "default/gw/default/mixed/r0/m0" && (r.PathType != "Exact" || r.Path != "/login" || r.Methods[0] != "GET") {
			t.Fatalf("exact match: %+v", r)
		}
		if r.Name == "default/gw/default/mixed/r0/m1" && (r.Headers[0].Name != "X-Beta" || r.Path != "/") {
			t.Fatalf("header match: %+v", r)
		}
	}
	if len(names) != 4 { // two matches of mixed, bad-refs, missing
		t.Fatalf("routes %v", names)
	}
	bad := out.Routes[types.NamespacedName{Namespace: "default", Name: "bad-refs"}][0].Conditions
	if c := condition(t, bad, "ResolvedRefs"); c.Reason != "RefNotPermitted" || c.Status != metav1.ConditionFalse {
		t.Fatalf("cross-namespace: %+v", c)
	}
	missing := out.Routes[types.NamespacedName{Namespace: "default", Name: "missing"}][0].Conditions
	if c := condition(t, missing, "ResolvedRefs"); c.Reason != "BackendNotFound" {
		t.Fatalf("missing: %+v", c)
	}
	// A route with no valid backend answers 500 by a generated policy.
	found := false
	for _, p := range out.Config.Policies {
		if strings.Contains(p.Script, "deny(500") && p.Routes[0] == "default/gw/default/missing/r0/m0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no deny(500) policy for the unresolved route: %+v", out.Config.Policies)
	}
}

func TestNoEndpointsIs503AndNoRoutesIs404(t *testing.T) {
	in := Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{gateway()},
		Services: []corev1.Service{service("web", 80)},
		Routes:   []gwv1.HTTPRoute{httpRoute("r", nil, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80, 1)}})}}
	out := Translate(in)
	mustValidate(t, out.Config)
	if len(out.Config.Policies) != 1 || !strings.Contains(out.Config.Policies[0].Script, "deny(503") {
		t.Fatalf("policies %+v", out.Config.Policies)
	}
	empty := Translate(Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{gateway()}})
	mustValidate(t, empty.Config)
	if empty.Config.Routes[0].Name != "portcullis:no-routes" {
		t.Fatalf("empty: %+v", empty.Config.Routes)
	}
}

func TestListenerHostnamesAndClasses(t *testing.T) {
	wild := gwv1.Hostname("*.example.com")
	gw := gateway(gwv1.Listener{Name: "web", Port: 80, Protocol: gwv1.HTTPProtocolType, Hostname: &wild},
		gwv1.Listener{Name: "tls", Port: 443, Protocol: gwv1.HTTPSProtocolType})
	foreign := gwv1.Gateway{ObjectMeta: om("default", "theirs"), Spec: gwv1.GatewaySpec{GatewayClassName: "other"}}
	in := Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{gw, foreign},
		Services: []corev1.Service{service("web", 80)}, Slices: []discoveryv1.EndpointSlice{slice("web", 80, true, "10.0.0.1")},
		Routes: []gwv1.HTTPRoute{
			httpRoute("match", []gwv1.Hostname{"a.example.com", "b.other.org"}, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80, 1)}}),
			httpRoute("nomatch", []gwv1.Hostname{"x.other.org"}, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80, 1)}}),
		}}
	out := Translate(in)
	mustValidate(t, out.Config)
	if _, ok := out.Gateways[types.NamespacedName{Namespace: "default", Name: "theirs"}]; ok {
		t.Fatal("a Gateway of another class must be left alone")
	}
	g := out.Gateways[types.NamespacedName{Namespace: "default", Name: "gw"}]
	if c := condition(t, g.Listeners[1].Conditions, "Accepted"); c.Reason != "UnsupportedProtocol" {
		t.Fatalf("https listener: %+v", c)
	}
	if h := out.Config.Routes[0].Hosts; len(h) != 1 || h[0] != "a.example.com" {
		t.Fatalf("hosts %v", h)
	}
	if c := condition(t, out.Routes[types.NamespacedName{Namespace: "default", Name: "nomatch"}][0].Conditions, "Accepted"); c.Reason != "NoMatchingListenerHostname" {
		t.Fatalf("nomatch: %+v", c)
	}
}

func TestPolicies(t *testing.T) {
	pol := func(name, kind, target, script string) v1alpha1.PortcullisPolicy {
		return v1alpha1.PortcullisPolicy{ObjectMeta: om("default", name), Spec: v1alpha1.PolicySpec{
			TargetRef: v1alpha1.TargetRef{Group: gwv1.GroupName, Kind: kind, Name: target}, Script: script}}
	}
	in := Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{gateway()},
		Services: []corev1.Service{service("web", 80)}, Slices: []discoveryv1.EndpointSlice{slice("web", 80, true, "10.0.0.1")},
		Routes: []gwv1.HTTPRoute{httpRoute("app", nil, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80, 1)}})},
		Policies: []v1alpha1.PortcullisPolicy{
			pol("block-admin", "HTTPRoute", "app", `policy "block-admin" { on request { if path.starts_with("/admin") { deny(403) } } }`),
			pol("gw-wide", "Gateway", "gw", `policy "gw" { on request { if rate_limit(client.ip, 5, "1s") { deny(429) } route_to("default/web:80") } }`),
			pol("broken", "HTTPRoute", "app", "policy \"broken\" {\n on request { if path { deny(403) } }\n}"),
			pol("dup", "HTTPRoute", "app", `policy "block-admin" { on request { } }`),
			pol("orphan", "HTTPRoute", "nope", `policy "orphan" { on request { } }`),
		}}
	out := Translate(in)
	mustValidate(t, out.Config)
	want := map[string]string{"block-admin": "Accepted", "gw-wide": "Accepted", "broken": "Invalid", "dup": "Conflicted", "orphan": "TargetNotFound"}
	for name, reason := range want {
		c := condition(t, out.Policies[types.NamespacedName{Namespace: "default", Name: name}], "Accepted")
		if c.Reason != reason {
			t.Errorf("%s: %+v, want %s", name, c, reason)
		}
	}
	if c := out.Policies[types.NamespacedName{Namespace: "default", Name: "broken"}][0]; !strings.Contains(c.Message, "2:18: if condition must be bool") {
		t.Errorf("broken: message %q should carry line:col", c.Message)
	}
	// Gateway-wide policies run first.
	if len(out.Config.Policies) != 2 || !strings.HasPrefix(out.Config.Policies[0].Script, `policy "gw"`) {
		t.Fatalf("attached policies: %+v", out.Config.Policies)
	}
}
