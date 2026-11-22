package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/testcert"
)

func TestHTTPSCertificates(t *testing.T) {
	crt, key := testcert.New(t, "example.org")
	good := &corev1.Secret{Data: map[string][]byte{"tls.crt": crt, "tls.key": key}}
	bad := &corev1.Secret{Data: map[string][]byte{"tls.crt": []byte("Hello"), "tls.key": []byte("Hello")}}
	secrets := map[types.NamespacedName]*corev1.Secret{
		{Namespace: "default", Name: "good"}: good, {Namespace: "default", Name: "bad"}: bad, {Namespace: "vault", Name: "good"}: good}
	grant := func(fromKind, name string) gwv1.ReferenceGrant {
		return gwv1.ReferenceGrant{ObjectMeta: om("vault", "g"), Spec: gwv1.ReferenceGrantSpec{
			From: []gwv1.ReferenceGrantFrom{{Group: gwv1.GroupName, Kind: gwv1.Kind(fromKind), Namespace: "default"}},
			To:   []gwv1.ReferenceGrantTo{{Kind: "Secret", Name: ptr(gwv1.ObjectName(name))}}}}
	}
	for _, tc := range []struct {
		name, ns, reason string
		grants           []gwv1.ReferenceGrant
		reads            int // Secrets the controller may GET
	}{
		{"good", "default", "ResolvedRefs", nil, 1},
		{"missing", "default", "InvalidCertificateRef", nil, 1},
		{"bad", "default", "InvalidCertificateRef", nil, 1},
		{"../../configmaps/x", "default", "InvalidCertificateRef", nil, 0},
		{"good", "vault", "RefNotPermitted", nil, 0},
		{"good", "vault", "RefNotPermitted", []gwv1.ReferenceGrant{grant("HTTPRoute", "good")}, 0},
		{"good", "vault", "RefNotPermitted", []gwv1.ReferenceGrant{grant("Gateway", "other")}, 0},
		{"good", "vault", "ResolvedRefs", []gwv1.ReferenceGrant{grant("Gateway", "good")}, 1},
	} {
		g := gateway(gwv1.Listener{Name: "https", Port: 443, Protocol: gwv1.HTTPSProtocolType, TLS: &gwv1.ListenerTLSConfig{
			CertificateRefs: []gwv1.SecretObjectReference{{Name: gwv1.ObjectName(tc.name), Namespace: ptr(gwv1.Namespace(tc.ns))}}}})
		in := Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{g}, Grants: tc.grants, Secrets: secrets,
			Routes: []gwv1.HTTPRoute{httpRoute("r", nil, gwv1.HTTPRouteRule{})}}
		if n := len(secretRefs(in.Classes, in.Gateways, in.Grants)); n != tc.reads {
			t.Errorf("%s/%s: %d secret reads, want %d", tc.ns, tc.name, n, tc.reads)
		}
		out := Translate(in)
		mustValidate(t, out.Config)
		l := out.Gateways[nsName(&g)].Listeners[0]
		if c := condition(t, l.Conditions, "ResolvedRefs"); c.Reason != tc.reason {
			t.Errorf("%s/%s: %+v, want %s", tc.ns, tc.name, c, tc.reason)
		}
		ok := tc.reason == "ResolvedRefs"
		if l.AttachedRoutes != 1 || len(l.SupportedKinds) != 1 || (len(out.Config.Listeners) == 1) != ok {
			t.Errorf("%s/%s: attached %d, listeners %d", tc.ns, tc.name, l.AttachedRoutes, len(out.Config.Listeners))
		}
	}
}

func TestGatewaysAreIsolatedAndPortsAreStable(t *testing.T) {
	a, b := gateway(), gateway()
	a.UID, b.UID, b.Name = "uid-a", "uid-b", "other"
	ra, rb := httpRoute("ra", nil, gwv1.HTTPRouteRule{}), httpRoute("rb", nil, gwv1.HTTPRouteRule{})
	rb.Spec.ParentRefs[0].Name = "other"
	in := Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{a, b}, Routes: []gwv1.HTTPRoute{ra, rb}, SystemNamespace: "system"}
	out := Translate(in)
	mustValidate(t, out.Config)
	if rs := out.Config.Routes; len(rs) != 2 || rs[0].Port == rs[1].Port || rs[0].Port == 0 || rs[1].Port == 0 {
		t.Fatalf("both gateways share a port: %+v", rs)
	}

	// The recorded port wins over the hash, unless someone forged the record.
	recorded := corev1.Service{ObjectMeta: om("system", serviceName(&a)),
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http-80", TargetPort: intstr.FromInt32(12345)}}}}
	recorded.Labels = map[string]string{managedByLabel: managedByValue}
	forged := *recorded.DeepCopy()
	forged.Namespace, forged.Spec.Ports[0].TargetPort = "tenant", intstr.FromInt32(15555)
	in.Services = []corev1.Service{forged, recorded}
	out = Translate(in)
	var got []int
	for _, l := range out.Config.Listeners {
		got = append(got, l.Port)
	}
	if !slices.Contains(got, 12345) || slices.Contains(got, 15555) {
		t.Fatalf("ports %v", got)
	}
}

func TestListenerConflicts(t *testing.T) {
	out := Translate(Inputs{Classes: []gwv1.GatewayClass{class()}, Gateways: []gwv1.Gateway{gateway(
		gwv1.Listener{Name: "a", Port: 80, Protocol: gwv1.HTTPProtocolType},
		gwv1.Listener{Name: "b", Port: 80, Protocol: gwv1.HTTPSProtocolType},
		gwv1.Listener{Name: "c", Port: 81, Protocol: gwv1.HTTPProtocolType},
		gwv1.Listener{Name: "d", Port: 81, Protocol: gwv1.HTTPProtocolType})}})
	mustValidate(t, out.Config)
	ls := out.Gateways[types.NamespacedName{Namespace: "default", Name: "gw"}].Listeners
	for i, want := range []string{"ProtocolConflict", "ProtocolConflict", "HostnameConflict", "HostnameConflict"} {
		if c := condition(t, ls[i].Conditions, "Conflicted"); c.Reason != want {
			t.Errorf("listener %d: %+v, want %s", i, c, want)
		}
	}
}

func TestFiltersAndInvalidShares(t *testing.T) {
	in := twoTenants()
	in.Routes[0].Spec.Rules = []gwv1.HTTPRouteRule{
		{Filters: []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gwv1.HTTPHeaderFilter{Add: []gwv1.HTTPHeader{{Name: "X-A", Value: "1"}}}}},
			BackendRefs: []gwv1.HTTPBackendRef{backendRef("app", 80, 7), backendRef("missing", 80, 3)}},
		{Filters: []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gwv1.HTTPRequestRedirectFilter{Hostname: ptr(gwv1.PreciseHostname("example.org"))}}}},
		{Filters: []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gwv1.HTTPHeaderFilter{Remove: []string{"bad name"}}}}},
	}
	out := Translate(in)
	mustValidate(t, out.Config)
	rs := out.Config.Routes
	var got [3]int // index in rs of each rule
	for i := range rs {
		for ri, s := range []string{"/default/a/r0/", "/default/a/r1/", "/default/a/r2/"} {
			if len(poolsOf(&config.Config{Routes: rs[i : i+1]}, s)) > 0 {
				got[ri] = i
			}
		}
	}
	if r := rs[got[0]]; r.RequestHeaders == nil || len(r.Backends) != 2 || r.Backends[1].Pool != pool500 || *r.Backends[1].Weight != 3 {
		t.Errorf("header rule: %+v", r)
	}
	if d := rs[got[1]].Redirect; d == nil || d.Status != 302 || d.Scheme != "http" || d.Port != 80 || d.Host != "example.org" {
		t.Errorf("redirect rule: %+v", d)
	}
	if r := rs[got[2]]; r.Backends[0].Pool != pool500 || r.RequestHeaders != nil {
		t.Errorf("a rule with a bad filter must answer 500: %+v", r)
	}
}
