package controller

import (
	"fmt"
	"slices"

	"github.com/AaronRaj06/portcullis/internal/config"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/controller/v1alpha1"
)

func testPolicy(ns, name, kind, target, script string) v1alpha1.PortcullisPolicy {
	p := v1alpha1.PortcullisPolicy{ObjectMeta: om(ns, name)}
	p.Spec.TargetRef.Kind, p.Spec.TargetRef.Name, p.Spec.Script = kind, target, script
	return p
}

func twoTenants() Inputs {
	g := gateway(gwv1.Listener{Name: "http", Port: 80, Protocol: gwv1.HTTPProtocolType,
		AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr(gwv1.NamespacesFromAll)}}})
	a := httpRoute("a", nil, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("app", 80, 1)}})
	b := httpRoute("b", []gwv1.Hostname{"b.example"}, gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{backendRef("private", 80, 1)}})
	b.Namespace = "victim"
	b.Spec.ParentRefs[0].Namespace = ptr(gwv1.Namespace("default"))
	svc, eps := service("private", 80), slice("private", 8080, true, "10.0.0.9")
	svc.Namespace, eps.Namespace = "victim", "victim"
	return Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{g}, Routes: []gwv1.HTTPRoute{a, b},
		Services: []corev1.Service{service("app", 80), svc}, Slices: []discoveryv1.EndpointSlice{slice("app", 8080, true, "10.0.0.1"), eps}}
}

func poolsOf(c *config.Config, route string) []string {
	var out []string
	for _, r := range c.Routes {
		if strings.Contains(r.Name, route) {
			for _, b := range r.Backends {
				out = append(out, b.Pool)
			}
		}
	}
	return out
}

func TestPolicyCannotRouteToAnotherNamespace(t *testing.T) {
	for _, pool := range []string{"victim/private:80", pool404} {
		in := twoTenants()
		in.Policies = []v1alpha1.PortcullisPolicy{testPolicy("default", "steal", "HTTPRoute", "a",
			fmt.Sprintf(`policy "steal" { on request { route_to(%q) } }`, pool))}
		out := Translate(in)
		mustValidate(t, out.Config)
		if len(out.Config.Policies) != 0 {
			t.Fatalf("route_to(%s) was accepted", pool)
		}
	}
	// A namespace with no pools must not mean "any pool".
	in := twoTenants()
	in.Routes = in.Routes[1:]
	in.Policies = []v1alpha1.PortcullisPolicy{testPolicy("default", "steal", "Gateway", "gw", `policy "steal" { on request { route_to("victim/private:80") } }`)}
	if out := Translate(in); len(out.Config.Policies) != 0 {
		t.Fatal("route_to from a namespace without pools was accepted")
	}
}

func TestPolicyNamesCannotCollideAcrossNamespaces(t *testing.T) {
	in := twoTenants()
	in.Policies = []v1alpha1.PortcullisPolicy{
		testPolicy("default", "lockdown", "HTTPRoute", "a", `policy "lockdown" { on request { log("noop") } }`),
		testPolicy("victim", "lockdown", "HTTPRoute", "b", `policy "lockdown" { on request { deny(403, "no") } }`)}
	out := Translate(in)
	mustValidate(t, out.Config)
	if ps := out.Config.Policies; len(ps) != 2 || ps[0].ID != "default/lockdown" || ps[1].ID != "victim/lockdown" {
		t.Fatalf("policies %+v", ps)
	}
}

func TestPolicySurvivesPoolWithoutEndpoints(t *testing.T) {
	in := twoTenants()
	b := &in.Routes[1].Spec.Rules[0]
	b.BackendRefs = append(b.BackendRefs, backendRef("canary", 80, 1))
	c := service("canary", 80)
	c.Namespace = "victim"
	in.Services = append(in.Services, c)
	in.Policies = []v1alpha1.PortcullisPolicy{testPolicy("victim", "guard", "HTTPRoute", "b",
		`policy "guard" { on request { if path.starts_with("/admin") { deny(403) } route_to("victim/canary:80") } }`)}
	out := Translate(in)
	mustValidate(t, out.Config)
	if len(out.Config.Policies) != 1 {
		t.Fatal("the deny policy vanished with the pool's endpoints")
	}
	if !slices.ContainsFunc(out.Config.Pools, func(p config.Pool) bool { return p.Name == "victim/canary:80" && p.Status == 503 }) {
		t.Fatal("the empty pool should answer 503")
	}
}

func TestInvalidPolicyFailsClosed(t *testing.T) {
	for onError, closed := range map[string]bool{"": true, "fail_closed": true, "fail_open": false} {
		in := twoTenants()
		p := testPolicy("victim", "typo", "HTTPRoute", "b", `policy "typo" { on request { if path { deny(403) } } }`)
		p.Spec.OnError = onError
		in.Policies = []v1alpha1.PortcullisPolicy{p, testPolicy("victim", "ok", "HTTPRoute", "b", `policy "ok" { on request { allow() } }`)}
		out := Translate(in)
		mustValidate(t, out.Config)
		if got := slices.Equal(poolsOf(out.Config, "/victim/b/"), []string{pool500}); got != closed {
			t.Errorf("onError %q: closed=%v, want %v", onError, got, closed)
		}
		if slices.Contains(poolsOf(out.Config, "/default/a/"), pool500) {
			t.Error("another tenant's route was closed")
		}
		for _, cp := range out.Config.Policies {
			if len(cp.Routes) == 0 {
				t.Fatal("a policy with no routes applies to every route")
			}
		}
	}
}

func TestPolicyLimitIsPerRoute(t *testing.T) {
	in := twoTenants()
	for i := range config.MaxPolicies + 1 {
		in.Policies = append(in.Policies, testPolicy("default", fmt.Sprintf("p%02d", i), "HTTPRoute", "a", `policy "p" { on request { } }`))
	}
	in.Policies = append(in.Policies, testPolicy("victim", "guard", "HTTPRoute", "b", `policy "g" { on request { deny(403) } }`))
	out := Translate(in)
	mustValidate(t, out.Config)
	if !slices.Equal(poolsOf(out.Config, "/default/a/"), []string{pool500}) {
		t.Fatal("the route over the limit must fail closed")
	}
	if ps := out.Config.Policies; len(ps) != 1 || ps[0].ID != "victim/guard" {
		t.Fatalf("the other tenant's policy must still apply: %d policies", len(ps))
	}
}

func TestEndpointsCannotPointAtTheProxyOrMetadata(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "169.254.169.254", "0.0.0.0", "::1", "::ffff:127.0.0.1", "fe80::1", "224.0.0.1", "localhost"} {
		if dialable(a) {
			t.Errorf("%s is dialable", a)
		}
	}
	for _, a := range []string{"10.0.0.1", "fd00::1", "192.168.1.4"} {
		if !dialable(a) {
			t.Errorf("%s is refused", a)
		}
	}
	in := twoTenants()
	in.Slices[0].Endpoints[0].Addresses = []string{"127.0.0.1"}
	if got := poolsOf(Translate(in).Config, "/default/a/"); !slices.Equal(got, []string{pool503}) {
		t.Fatalf("loopback endpoint was used: %v", got)
	}
}
