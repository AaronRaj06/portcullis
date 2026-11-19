package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AaronRaj06/portcullis/internal/lb"
)

func testPool(name string) *lb.Pool {
	bs := []*lb.Backend{lb.NewBackend(name+":80", 1)}
	return &lb.Pool{Name: name, Scheme: "http", Backends: bs, Balancer: lb.NewRoundRobin(bs)}
}

func TestRouterPrecedence(t *testing.T) {
	p := testPool("p")
	rt, err := NewRouter([]Route{
		{Name: "catch-all", Pool: p},
		{Name: "any-api", Path: "/api", Pool: p},
		{Name: "app-root", Hosts: []string{"app.example.com"}, Pool: p},
		{Name: "app-api", Hosts: []string{"app.example.com"}, Path: "/api", Pool: p},
		{Name: "app-api-v1", Hosts: []string{"APP.example.com."}, Path: "/api/v1", Pool: p},
		{Name: "app-login-exact", Hosts: []string{"app.example.com"}, Path: "/login", Exact: true, Pool: p},
		{Name: "app-login-prefix", Hosts: []string{"app.example.com"}, Path: "/login", Pool: p},
		{Name: "app-api-post", Hosts: []string{"app.example.com"}, Path: "/api", Methods: []string{"POST"}, Pool: p},
		{Name: "app-api-canary", Hosts: []string{"app.example.com"}, Path: "/api", Methods: []string{"POST"},
			Headers: []HeaderMatch{{"x-canary", "1"}}, Pool: p},
		{Name: "wild", Hosts: []string{"*.example.com"}, Pool: p},
		{Name: "wild-deeper", Hosts: []string{"*.eu.example.com"}, Pool: p},
		{Name: "wild-admin", Hosts: []string{"*.example.com"}, Path: "/admin", Pool: p},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, host, path string
		hdr                map[string]string
		want               string
	}{
		{"GET", "app.example.com", "/", nil, "app-root"},
		{"GET", "App.Example.com:8443", "/x", nil, "app-root"}, // case and port ignored
		{"GET", "app.example.com", "/api", nil, "app-api"},
		{"GET", "app.example.com", "/api/v1/users", nil, "app-api-v1"}, // longest prefix
		{"GET", "app.example.com", "/apiv1", nil, "app-root"},          // prefix matches whole segments only
		{"GET", "app.example.com", "/login", nil, "app-login-exact"},   // exact beats equal-length prefix
		{"GET", "app.example.com", "/login/sso", nil, "app-login-prefix"},
		{"POST", "app.example.com", "/api/x", nil, "app-api-post"}, // method match beats none
		{"POST", "app.example.com", "/api", map[string]string{"X-Canary": "1"}, "app-api-canary"},
		{"POST", "app.example.com", "/api", map[string]string{"X-Canary": "0"}, "app-api-post"},
		{"GET", "shop.example.com", "/", nil, "wild"},
		{"GET", "a.b.example.com", "/admin/x", nil, "wild-admin"},
		{"GET", "shop.eu.example.com", "/admin", nil, "wild-deeper"}, // longer wildcard suffix wins
		{"GET", "example.com", "/api", nil, "any-api"},               // *.example.com does not match the apex
		{"GET", "other.org", "/", nil, "catch-all"},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		got := rt.Match(r)
		if got == nil || got.Name != tc.want {
			name := "<nil>"
			if got != nil {
				name = got.Name
			}
			t.Errorf("%s %s%s %v: matched %q, want %q", tc.method, tc.host, tc.path, tc.hdr, name, tc.want)
		}
	}
}

func TestRouterFallsBackToLessSpecificHost(t *testing.T) {
	p := testPool("p")
	rt, err := NewRouter([]Route{
		{Name: "app-api-only", Hosts: []string{"app.example.com"}, Path: "/api", Pool: p},
		{Name: "wild", Hosts: []string{"*.example.com"}, Pool: p},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://app.example.com/other", nil)
	if got := rt.Match(r); got == nil || got.Name != "wild" {
		t.Fatalf("got %v, want fallback to wildcard route", got)
	}
}

func TestRouterNoMatch(t *testing.T) {
	rt, err := NewRouter([]Route{{Name: "a", Hosts: []string{"a.test"}, Pool: testPool("p")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.Match(httptest.NewRequest("GET", "http://b.test/", nil)); got != nil {
		t.Fatalf("matched %q for an unrouted host", got.Name)
	}
}

func TestRouterValidation(t *testing.T) {
	p := testPool("p")
	for name, routes := range map[string][]Route{
		"empty":          nil,
		"no name":        {{Pool: p}},
		"duplicate name": {{Name: "a", Pool: p}, {Name: "a", Pool: p}},
		"no pool":        {{Name: "a"}},
		"empty pool":     {{Name: "a", Pool: &lb.Pool{Balancer: lb.NewRoundRobin(nil)}}},
		"relative path":  {{Name: "a", Path: "api", Pool: p}},
		"bad wildcard":   {{Name: "a", Hosts: []string{"a.*.com"}, Pool: p}},
	} {
		if _, err := NewRouter(routes); err == nil {
			t.Errorf("%s: NewRouter accepted invalid config", name)
		}
	}
}

func TestRouterPools(t *testing.T) {
	a, b := testPool("a"), testPool("b")
	rt, err := NewRouter([]Route{{Name: "1", Pool: a}, {Name: "2", Path: "/x", Pool: a}, {Name: "3", Path: "/y", Pool: b}})
	if err != nil {
		t.Fatal(err)
	}
	if ps := rt.Pools(); len(ps) != 2 {
		t.Fatalf("Pools() = %d pools, want 2 distinct", len(ps))
	}
}

func BenchmarkRouterMatch(b *testing.B) {
	p := testPool("p")
	var routes []Route
	for _, h := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		for _, path := range []string{"/", "/api", "/api/v1", "/static", "/login"} {
			routes = append(routes, Route{Name: h + path, Hosts: []string{h}, Path: path, Pool: p})
		}
	}
	rt, err := NewRouter(routes)
	if err != nil {
		b.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://b.example.com/api/v1/users", nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rt.Match(r)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM": "example.com", "example.com:8080": "example.com", "example.com.": "example.com",
		"[::1]:443": "::1", "[::1]": "::1", "10.0.0.1:80": "10.0.0.1",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
