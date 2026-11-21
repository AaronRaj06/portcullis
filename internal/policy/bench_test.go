package policy

import (
	"net/http"
	"net/netip"
	"testing"
)

// BenchmarkEdgeRules evaluates the language spec's main example on a request
// that passes every rule (the common case), so every condition is evaluated.
func BenchmarkEdgeRules(b *testing.B) {
	p := mustCompile(b, `policy "edge-rules" {
  on request {
    if path.starts_with("/admin") && !(client.ip in ["10.0.0.0/8", "192.168.0.0/16"]) { deny(403, "admin is internal only") }
    if header("user-agent").contains("sqlmap") { deny(403) }
    if rate_limit(client.ip, 1000000000, "1s") { deny(429) }
    if header("x-canary") == "1" { route_to("canary") }
    set_header("x-frame-options", "DENY")
  }
}`)
	h := http.Header{"User-Agent": {"curl/8.0"}}
	e := &Env{Method: "GET", Host: "example.com", Path: "/api/users", ClientIP: netip.MustParseAddr("203.0.113.7"), Header: h}
	b.ReportAllocs()
	for b.Loop() {
		e.RouteTo, e.Path = "", "/api/users"
		if _, err := p.EvalRequest(e); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSimpleDeny is one header comparison: the floor cost of a policy.
func BenchmarkSimpleDeny(b *testing.B) {
	p := mustCompile(b, `policy "p" { on request { if header("x-block") == "1" { deny(403) } } }`)
	e := &Env{Method: "GET", Path: "/", Header: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := p.EvalRequest(e); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Compile(edgeRules, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
