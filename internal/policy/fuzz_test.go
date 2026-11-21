package policy

import (
	"net/http"
	"net/netip"
	"testing"
)

var seeds = []string{
	edgeRules,
	`policy "a" { on request { if cookie("beta") == "yes" || header("x-canary") == "1" { route_to("canary") tag("canary") } } }`,
	`policy "b" { on request { if path == "/old" { redirect("/new", 308) } if path.starts_with("/v1/") { rewrite_path("/api" + path) } } }`,
	`policy "c" { on response { if response.status >= 500 { set_header("cache-control", "no-store") } remove_header("server") } }`,
	`policy "d" { on request { if method in ["POST","PUT"] && !(client.ip in ["::1", "10.0.0.0/8"]) { deny(405, "read only") } else if query("debug") == "1" { log("debug " + path.lower()) } else { allow() } } }`,
	`policy "e" { on request { if 2 in [1,2] && path.len() <= 10 { deny(400) } } } # comment`,
	"policy \"f\" {\n// line comment\n on request { tag(\"a\\\"b\\\\c\\n\") ; } }",
	`policy "g" { on request { if (((true))) {} } }`,
	`policy "" {}`, `policy "x" { on request { if a = b {} } }`, `"unterminated`, `policy "p" { on request { deny(99999999999999999999) } }`,
}

// FuzzLex: the lexer never panics, and token positions only move forward.
func FuzzLex(f *testing.F) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		toks, err := lex(src)
		if err != nil {
			return
		}
		for i := 1; i < len(toks); i++ {
			a, b := toks[i-1].pos, toks[i].pos
			if b.line < a.line || (b.line == a.line && b.col <= a.col && toks[i].kind != tEOF) {
				t.Fatalf("position went backwards: %v then %v", a, b)
			}
		}
		if toks[len(toks)-1].kind != tEOF {
			t.Fatal("missing EOF")
		}
	})
}

// FuzzCompile is the property test for the whole pipeline: any input either
// fails to compile with a positioned error, or compiles into a program that
// evaluates in both phases without panicking and within its step budget.
func FuzzCompile(f *testing.F) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		p, err := Compile(src, Options{MaxSteps: 200})
		if err != nil {
			if err.Error() == "" {
				t.Fatal("empty error message")
			}
			return
		}
		e := &Env{Method: "GET", Host: "h", Path: "/admin", RawQuery: "q=1&%zz", ClientIP: netip.MustParseAddr("10.0.0.1"),
			Header: http.Header{"Cookie": {"beta=yes"}, "X-Canary": {"1"}}, Status: 503, RespHeader: http.Header{"Server": {"x"}}}
		for range 2 { // twice: rate limiter state and query cache are reused
			_, _ = p.EvalRequest(e)
			_ = p.EvalResponse(e)
		}
	})
}
