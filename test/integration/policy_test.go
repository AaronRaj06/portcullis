package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// policyConfig: two pools; edge rules on every route. Loopback is a trusted
// proxy, so tests choose the client address with X-Forwarded-For.
func policyConfig(stable, canary, version string, extra string) string {
	return fmt.Sprintf(`version: %q
trustedProxies: ["127.0.0.0/8", "::1/128"]
pools:
  - {name: stable, backends: [{address: %q}]}
  - {name: canary, backends: [{address: %q}]}
routes:
  - {name: web, backends: [{pool: stable}]}
policies:
  - script: |
      policy "edge-rules" {
        on request {
          if path.starts_with("/admin") && !(client.ip in ["10.0.0.0/8", "192.168.0.0/16"]) {
            deny(403, "admin is internal only")
          }
          if path.starts_with("/api") && rate_limit(client.ip, 5, "1m") { deny(429) }
          if header("x-canary") == "1" || cookie("beta") == "yes" { route_to("canary") tag("canary") }
          set_header("x-policy", "edge-rules")
        }
        on response {
          remove_header("x-backend-secret")
          set_header("x-frame-options", "DENY")
        }
      }
%s`, version, stable, canary, extra)
}

func policyProxy(t *testing.T, cfg string) (*config.Manager, string) {
	t.Helper()
	h, err := proxy.New(proxy.Options{Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(context.Background(), h, proxy.DefaultLimits, "", nil, nil)
	if _, err := mgr.Apply([]byte(cfg), "test"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return mgr, srv.URL
}

func echoBackend(t *testing.T, name string) string {
	srv := backend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Secret", "s3cret")
		_, _ = fmt.Fprintf(w, "%s %s policy=%s", name, r.URL.Path, r.Header.Get("X-Policy"))
	})
	return strings.TrimPrefix(srv.URL, "http://")
}

func from(ip string, kv ...string) map[string]string {
	m := map[string]string{"X-Forwarded-For": ip}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// Phase 5 "done when": the admin-lockdown, rate-limit and canary-routing examples work end to end.
func TestPolicyExamplesEndToEnd(t *testing.T) {
	stable, canary := echoBackend(t, "stable"), echoBackend(t, "canary")
	mgr, base := policyProxy(t, policyConfig(stable, canary, "v1", ""))

	t.Run("admin lockdown", func(t *testing.T) {
		code, body := do(t, "GET", base+"/admin/users", from("203.0.113.5"))
		if code != 403 || !strings.Contains(body, "admin is internal only") || !strings.Contains(body, "request_id") {
			t.Fatalf("outside client: %d %s", code, body)
		}
		if code, body := do(t, "GET", base+"/admin/users", from("10.1.2.3")); code != 200 || body != "stable /admin/users policy=edge-rules" {
			t.Fatalf("office client: %d %s", code, body)
		}
	})

	t.Run("rate limit per client, kept across reload", func(t *testing.T) {
		for i := range 5 {
			if code, _ := do(t, "GET", base+"/api/x", from("198.51.100.1")); code != 200 {
				t.Fatalf("request %d: %d", i+1, code)
			}
		}
		if code, _ := do(t, "GET", base+"/api/x", from("198.51.100.1")); code != 429 {
			t.Fatalf("6th request: %d, want 429", code)
		}
		if code, _ := do(t, "GET", base+"/api/x", from("198.51.100.2")); code != 200 {
			t.Fatalf("another client: %d", code)
		}
		// A reload with an unchanged rate_limit call site must not reset the bucket.
		st, err := mgr.Apply([]byte(policyConfig(stable, canary, "v2", "")), "test")
		if err != nil || !st.Changed {
			t.Fatalf("reload: %+v %v", st, err)
		}
		if code, _ := do(t, "GET", base+"/api/x", from("198.51.100.1")); code != 429 {
			t.Fatalf("after reload: %d, want 429 (counters kept)", code)
		}
	})

	t.Run("canary routing", func(t *testing.T) {
		cases := []struct {
			hdr  map[string]string
			want string
		}{
			{from("192.0.2.1"), "stable"},
			{from("192.0.2.1", "X-Canary", "1"), "canary"},
			{from("192.0.2.1", "Cookie", "beta=yes"), "canary"},
		}
		for _, c := range cases {
			if _, body := do(t, "GET", base+"/", c.hdr); !strings.HasPrefix(body, c.want+" ") {
				t.Fatalf("%v: got %q, want %s", c.hdr, body, c.want)
			}
		}
	})

	t.Run("response phase", func(t *testing.T) {
		resp, err := http.Get(base + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("X-Backend-Secret") != "" || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("response headers: %v", resp.Header)
		}
	})
}

func TestInvalidPolicyNeverReplacesGoodConfig(t *testing.T) {
	stable, canary := echoBackend(t, "stable"), echoBackend(t, "canary")
	mgr, base := policyProxy(t, policyConfig(stable, canary, "v1", ""))
	bad := strings.Replace(policyConfig(stable, canary, "v2", ""), `route_to("canary")`, `route_to("canary2")`, 1)
	_, err := mgr.Apply([]byte(bad), "test")
	if err == nil || !strings.Contains(err.Error(), `policies[0].script: 7:72: unknown pool "canary2"`) {
		t.Fatalf("want a positioned error, got %v", err)
	}
	if mgr.Status().Version != "v1" {
		t.Fatal("bad config replaced the good one")
	}
	if code, _ := do(t, "GET", base+"/admin", from("203.0.113.5")); code != 403 {
		t.Fatal("old policy no longer enforced")
	}
}

func TestPolicyBudgetFailClosedAndOpen(t *testing.T) {
	stable := echoBackend(t, "stable")
	cfg := func(onError string) string {
		return fmt.Sprintf(`pools: [{name: p, backends: [{address: %q}]}]
routes: [{name: r, backends: [{pool: p}]}]
policies:
  - {maxSteps: 3, onError: %s, script: 'policy "tiny" { on request { if path == "/" || path == "/x" { tag("t") } } }'}
  - {script: 'policy "after" { on request { set_header("x-policy", "after") } }'}
`, stable, onError)
	}
	_, base := policyProxy(t, cfg("fail_closed"))
	var e struct {
		Error string `json:"error"`
	}
	code, body := do(t, "GET", base+"/x", nil)
	if err := json.Unmarshal([]byte(body), &e); code != 500 || err != nil || e.Error != "Internal Server Error" {
		t.Fatalf("fail_closed: %d %s", code, body)
	}
	_, base = policyProxy(t, cfg("fail_open"))
	if code, body := do(t, "GET", base+"/x", nil); code != 200 || body != "stable /x policy=after" {
		t.Fatalf("fail_open: %d %s (the next policy must still run)", code, body)
	}
}

func TestPolicyRedirectAndRewrite(t *testing.T) {
	stable := echoBackend(t, "stable")
	_, base := policyProxy(t, fmt.Sprintf(`pools: [{name: p, backends: [{address: %q}]}]
routes: [{name: r, backends: [{pool: p}]}]
policies:
  - script: |
      policy "paths" {
        on request {
          if path == "/old" { redirect("/new", 308) }
          if path.starts_with("/v1/") { rewrite_path("/api" + path) }
        }
      }
`, stable))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(base + "/old")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 308 || resp.Header.Get("Location") != "/new" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, body := do(t, "GET", base+"/v1/users?id=7", nil); body != "stable /api/v1/users policy=" {
		t.Fatalf("rewrite: %q", body)
	}
}
