package policy

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AaronRaj06/portcullis/internal/ratelimit"
)

const edgeRules = `
policy "edge-rules" {
  on request {
    if path.starts_with("/admin") && !(client.ip in ["10.0.0.0/8", "192.168.0.0/16"]) {
      deny(403, "admin is internal only")
    }
    if header("user-agent").contains("sqlmap") { deny(403) }
    if rate_limit(client.ip, 100, "1m") { deny(429) }
    if header("x-canary") == "1" { route_to("canary") }
    set_header("x-frame-options", "DENY")
  }
  on response {
    remove_header("server")
  }
}`

func env(method, path, ip string, hdr ...string) *Env {
	h := http.Header{}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Add(hdr[i], hdr[i+1])
	}
	e := &Env{Method: method, Host: "example.com", Path: path, Header: h}
	if ip != "" {
		e.ClientIP = netip.MustParseAddr(ip)
	}
	return e
}

func mustCompile(t testing.TB, src string) *Program {
	t.Helper()
	p, err := Compile(src, Options{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

func TestEdgeRulesExample(t *testing.T) {
	p := mustCompile(t, edgeRules)
	cases := []struct {
		name   string
		env    *Env
		kind   VerdictKind
		status int
		pool   string
	}{
		{"admin from outside", env("GET", "/admin/users", "203.0.113.9"), Deny, 403, ""},
		{"admin from office", env("GET", "/admin/users", "10.1.2.3"), Continue, 0, ""},
		{"admin from mapped v4", env("GET", "/admin", "::ffff:192.168.1.1"), Continue, 0, ""},
		{"sqlmap", env("GET", "/", "1.2.3.4", "User-Agent", "sqlmap/1.7"), Deny, 403, ""},
		{"canary", env("GET", "/", "1.2.3.4", "X-Canary", "1"), Continue, 0, "canary"},
		{"plain", env("GET", "/", "1.2.3.4"), Continue, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := p.EvalRequest(c.env)
			if err != nil {
				t.Fatal(err)
			}
			if v.Kind != c.kind || v.Status != c.status || c.env.RouteTo != c.pool {
				t.Fatalf("got %+v pool %q, want kind %d status %d pool %q", v, c.env.RouteTo, c.kind, c.status, c.pool)
			}
			if v.Kind == Continue && c.env.Header.Get("X-Frame-Options") != "DENY" {
				t.Fatal("set_header not applied")
			}
		})
	}
	resp := &Env{RespHeader: http.Header{"Server": {"nginx"}, "X-Ok": {"1"}}}
	if err := p.EvalResponse(resp); err != nil || resp.RespHeader.Get("Server") != "" || resp.RespHeader.Get("X-Ok") != "1" {
		t.Fatalf("response phase: err %v header %v", err, resp.RespHeader)
	}
}

func TestRateLimitPerKey(t *testing.T) {
	p := mustCompile(t, `policy "rl" { on request { if rate_limit(client.ip, 3, "1h") { deny(429) } } }`)
	denied := 0
	for range 5 {
		if v, _ := p.EvalRequest(env("GET", "/", "1.1.1.1")); v.Kind == Deny {
			denied++
		}
	}
	if denied != 2 {
		t.Fatalf("denied %d of 5 with limit 3", denied)
	}
	if v, _ := p.EvalRequest(env("GET", "/", "2.2.2.2")); v.Kind != Continue {
		t.Fatal("another client has its own bucket")
	}
}

func TestExpressions(t *testing.T) {
	cases := []struct {
		cond string
		want bool
	}{
		{`method == "GET"`, true},
		{`method in ["POST", "PUT"]`, false},
		{`host == "example.com"`, true},
		{`path.ends_with(".php")`, true},
		{`path.len() > 5`, true},
		{`path.upper() == "/X/INDEX.PHP"`, true},
		{`header("X-Mixed").lower() == "abc"`, true},
		{`header("missing") == ""`, true},
		{`query("q") == "go lang"`, true},
		{`query("none") == ""`, true},
		{`cookie("session") == "s1"`, true},
		{`cookie("beta") == "yes"`, true},
		{`"a" + "b" == "ab"`, true},
		{`2 in [1, 2, 3]`, true},
		{`client.ip in ["2001:db8::/32", "127.0.0.1"]`, true},
		{`true && !false || false`, true},
		{`false || (1 < 2 && 2 <= 2 && 3 > 2 && 3 >= 4)`, false},
		{`1 != 2`, true},
	}
	for _, c := range cases {
		t.Run(c.cond, func(t *testing.T) {
			p := mustCompile(t, `policy "t" { on request { if `+c.cond+` { tag("yes") } } }`)
			e := env("GET", "/x/index.php", "127.0.0.1", "X-Mixed", "AbC", "Cookie", "session=s1; beta=\"yes\"")
			e.RawQuery = "q=go+lang&x=1"
			if _, err := p.EvalRequest(e); err != nil {
				t.Fatal(err)
			}
			if got := len(e.Tags) == 1; got != c.want {
				t.Fatalf("%s = %v, want %v", c.cond, got, c.want)
			}
		})
	}
}

func TestActions(t *testing.T) {
	p := mustCompile(t, `policy "a" {
	  on request {
	    if path == "/old" { redirect("/new", 308) }
	    if path.starts_with("/v1/") { rewrite_path("/api" + path) }
	    remove_header("cookie")
	    set_header("x-seen", header("x-seen") + "!")  # reads its own buffered write? no: reads the original
	    log("path " + path)
	    if path == "/ok" { allow() }
	    deny(418)
	  }
	}`)
	e := env("GET", "/old", "1.1.1.1")
	if v, _ := p.EvalRequest(e); v.Kind != Redirect || v.Status != 308 || v.Location != "/new" {
		t.Fatalf("redirect: %+v", v)
	}
	e = env("GET", "/v1/users", "1.1.1.1", "Cookie", "a=b", "X-Seen", "1")
	v, err := p.EvalRequest(e)
	if err != nil || v.Kind != Deny || v.Status != 418 || v.Message != "I'm a teapot" {
		t.Fatalf("deny: %+v %v", v, err)
	}
	if e.Path != "/api/v1/users" || e.Header.Get("Cookie") != "" || e.Header.Get("X-Seen") != "1!" {
		t.Fatalf("effects of a denied run still apply up to the deny: path %q header %v", e.Path, e.Header)
	}
	if len(e.Logs) != 1 || e.Logs[0] != "a: path /api/v1/users" {
		t.Fatalf("logs %q", e.Logs)
	}
	e = env("GET", "/ok", "1.1.1.1")
	if v, _ := p.EvalRequest(e); v.Kind != Allow {
		t.Fatalf("allow: %+v", v)
	}
}

func TestResponsePhase(t *testing.T) {
	p := mustCompile(t, `policy "r" { on response {
	  if response.status >= 500 && response.header("content-type") == "text/html" { set_header("cache-control", "no-store") }
	  if header("x-debug") == "1" { set_header("x-upstream-status", "seen") }
	} }`)
	e := env("GET", "/", "1.1.1.1", "X-Debug", "1")
	e.Status, e.RespHeader = 503, http.Header{"Content-Type": {"text/html"}}
	if err := p.EvalResponse(e); err != nil {
		t.Fatal(err)
	}
	if e.RespHeader.Get("Cache-Control") != "no-store" || e.RespHeader.Get("X-Upstream-Status") != "seen" || e.Header.Get("Cache-Control") != "" {
		t.Fatalf("resp %v req %v", e.RespHeader, e.Header)
	}
}

// TestEffectsAreAtomic: a runtime error discards every effect of that policy.
func TestEffectsAreAtomic(t *testing.T) {
	p := mustCompile(t, `policy "x" { on request { set_header("x-a", "1") route_to("p") tag("t") rewrite_path(header("x-path")) } }`)
	e := env("GET", "/orig", "1.1.1.1", "X-Path", "no-slash")
	_, err := p.EvalRequest(e)
	var re *RuntimeError
	if !errors.As(err, &re) || re.Pos != "1:73" {
		t.Fatalf("want a runtime error at 1:73, got %v", err)
	}
	if e.Header.Get("X-A") != "" || e.RouteTo != "" || len(e.Tags) != 0 || e.Path != "/orig" {
		t.Fatalf("effects leaked: %+v", e)
	}
	e = env("GET", "/", "1.1.1.1", "X-Path", "/a\r\nX-Evil: 1")
	if _, err := p.EvalRequest(e); err == nil {
		t.Fatal("CR/LF in a rewritten path must fail")
	}
}

func TestHeaderInjectionIsARuntimeError(t *testing.T) {
	p := mustCompile(t, `policy "x" { on request { set_header("x-a", header("x-in")) } }`)
	if _, err := p.EvalRequest(env("GET", "/", "", "X-In", "a\nb")); err == nil {
		t.Fatal("newline in a header value must fail")
	}
}

func TestStepBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString(`policy "big" { on request {`)
	for range 200 {
		b.WriteString(` if path == "/x" || path.len() > 0 { tag("t") }`)
	}
	b.WriteString(`} }`)
	p, err := Compile(b.String(), Options{MaxSteps: 500})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.EvalRequest(env("GET", "/", ""))
	if err == nil || !strings.Contains(err.Error(), "budget exceeded: steps") {
		t.Fatalf("want step budget error, got %v", err)
	}
	if p2, _ := Compile(b.String(), Options{Timeout: time.Second}); p2 != nil { // steps only; -race on a busy host can exceed 1ms
		if _, err := p2.EvalRequest(env("GET", "/", "")); err != nil {
			t.Fatalf("default budget should be enough: %v", err)
		}
	}
}

func TestTimeBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString(`policy "slow" { on request {`)
	for range 200 {
		b.WriteString(` if header("x").contains("needle") { tag("t") }`)
	}
	b.WriteString(`} }`)
	p, err := Compile(b.String(), Options{Timeout: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.EvalRequest(env("GET", "/", "", "X", strings.Repeat("a", 1<<16)))
	if err == nil || !strings.Contains(err.Error(), "budget exceeded: time") {
		t.Fatalf("want time budget error, got %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`policy "p" { on request { deny(403) `, `1:37: expected "}", found end of input`},
		{`policy "p" { on reqest {} }`, `1:17: expected "request" or "response"`},
		{`policy "p" { on request { if path = "/" {} } }`, `1:35: unexpected '=' (did you mean "=="?)`},
		{"policy \"p\" {\n on request {\n  if hedder(\"x\") == \"1\" { deny(403) }\n }\n}", `3:6: unknown function "hedder"`},
		{`policy "p" { on request { deny(200) } }`, `1:32: deny status 200 is outside 400..599`},
		{`policy "p" { on request { if path { deny(403) } } }`, `1:30: if condition must be bool, found string`},
		{`policy "p" { on request { if path == 1 { deny(403) } } }`, `cannot compare string == int`},
		{`policy "p" { on request { path.len() } }`, `the value of len(...) is unused`},
		{`policy "p" { on request { if deny(403) {} } }`, `deny is an action and has no value`},
		{`policy "p" { on response { deny(403) } }`, `deny is only allowed in "on request"`},
		{`policy "p" { on request { if response.status == 200 {} } }`, `response.status is only available in "on response"`},
		{`policy "p" { on request { if client.ip in ["10.0.0.0/33"] {} } }`, `"10.0.0.0/33" is not an IP address or CIDR`},
		{`policy "p" { on request { if rate_limit(path, 0, "1m") {} } }`, `limit must be at least 1`},
		{`policy "p" { on request { if rate_limit(path, 5, "1ms") {} } }`, `must be a duration from "1s" to "24h"`},
		{`policy "p" { on request { set_header("content-length", "0") } }`, `header Content-Length cannot be changed`},
		{`policy "p" { on request { set_header("bad name", "0") } }`, `"bad name" is not a valid header name`},
		{`policy "p" { on request { rewrite_path("nope") } }`, `rewrite_path "nope" must start with /`},
		{`policy "p" { on request { if 1 < 2 < 3 {} } }`, `comparisons cannot be chained`},
		{`policy "p" { on request {} on request {} }`, `duplicate "on request" handler`},
		{`policy "p" {} policy "q" {}`, `one policy per script`},
		{`policy "p" { on request { if "a" in [1] {} } }`, `cannot look for string in a list of ints`},
		{`policy "p" { on request { if path in [path] {} } }`, `list elements must be string or integer literals`},
		{`policy "p" { on request { tag("a\q") } }`, `1:33: unknown escape \q`},
		{`policy "p" { on request { if client == 1 {} } }`, `client is a namespace`},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			_, err := Compile(c.src, Options{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestRouteToUnknownPool(t *testing.T) {
	_, err := Compile(`policy "p" { on request { route_to("nope") } }`, Options{Pools: map[string]bool{"web": true}})
	if err == nil || !strings.Contains(err.Error(), `1:36: unknown pool "nope"`) {
		t.Fatalf("got %v", err)
	}
}

func TestAllTypeErrorsReported(t *testing.T) {
	_, err := Compile(`policy "p" { on request { deny(1) deny(2) } }`, Options{})
	var l ErrorList
	if !errors.As(err, &l) || len(l) != 2 {
		t.Fatalf("want 2 errors, got %v", err)
	}
}

func TestLimits(t *testing.T) {
	deep := `policy "p" { on request { if ` + strings.Repeat("(", 100) + "true" + strings.Repeat(")", 100) + ` {} } }`
	if _, err := Compile(deep, Options{}); err == nil || !strings.Contains(err.Error(), "nesting is deeper") {
		t.Fatalf("deep: %v", err)
	}
	if _, err := Compile(strings.Repeat(" ", MaxSource+1), Options{}); err == nil {
		t.Fatal("oversized source accepted")
	}
}

func TestMatchesAndNow(t *testing.T) {
	p := mustCompile(t, `policy "m" { on request {
	  if path.matches("^/users/[0-9]+$") { tag("user") }
	  if now() >= 1767225600 && now() < 1767229200 { deny(503, "maintenance") }
	} }`)
	e := env("GET", "/users/42", "")
	e.Now = 1767225000 // before the window
	if v, _ := p.EvalRequest(e); v.Kind != Continue || len(e.Tags) != 1 {
		t.Fatalf("before window: %+v tags %v", v, e.Tags)
	}
	e = env("GET", "/users/abc", "")
	e.Now = 1767226000
	if v, _ := p.EvalRequest(e); v.Kind != Deny || len(e.Tags) != 0 {
		t.Fatalf("in window: %+v tags %v", v, e.Tags)
	}
	if _, err := Compile(`policy "m" { on request { if path.matches("(") {} } }`, Options{}); err == nil ||
		!strings.Contains(err.Error(), "1:43: invalid regular expression") {
		t.Fatalf("bad regex: %v", err)
	}
}

// TestRateLimitSites: a named limiter keeps its identity when the script moves,
// and Share divides the limit between replicas.
func TestRateLimitSites(t *testing.T) {
	sites := map[string]int{}
	o := Options{Share: 3, Limiter: func(site string, limit int, _ time.Duration) *ratelimit.Limiter {
		sites[site] = limit
		return ratelimit.New(limit, time.Hour, 0)
	}}
	for _, src := range []string{
		`policy "p" { on request { if rate_limit(client.ip, 10, "1m", "login") { deny(429) } } }`,
		`policy "p" { on request { tag("moved") if rate_limit(client.ip, 10, "1m", "login") { deny(429) } } }`,
		`policy "p" { on request { if rate_limit(client.ip, 10, "1m") { deny(429) } } }`,
	} {
		if _, err := Compile(src, o); err != nil {
			t.Fatal(err)
		}
	}
	// ceil(10/3) = 4 per replica; both named versions map to one site.
	if len(sites) != 2 || sites["p@name:login/4/1m0s"] != 4 || sites["p@1:30/4/1m0s"] != 4 {
		t.Fatalf("sites %v", sites)
	}
}

func TestEnvReset(t *testing.T) {
	p := mustCompile(t, `policy "q" { on request { if query("a") == "1" { tag("a") } } }`)
	e := env("GET", "/", "")
	e.RawQuery = "a=1"
	_, _ = p.EvalRequest(e)
	h := e.Header
	e.Reset()
	e.Header, e.RawQuery = h, "a=2"
	_, _ = p.EvalRequest(e)
	if len(e.Tags) != 0 {
		t.Fatal("Reset must drop the cached query and old tags")
	}
}
