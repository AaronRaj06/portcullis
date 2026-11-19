package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AaronRaj06/portcullis/internal/health"
	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// named is a backend that answers with its name and counts hits.
type named struct {
	name string
	srv  *httptest.Server
	hits atomic.Int64
}

func (n *named) addr() string { return strings.TrimPrefix(n.srv.URL, "http://") }

func namedBackends(t *testing.T, names ...string) []*named {
	t.Helper()
	out := make([]*named, len(names))
	for i, nm := range names {
		n := &named{name: nm}
		n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.hits.Add(1)
			_, _ = io.WriteString(w, n.name)
		}))
		t.Cleanup(n.srv.Close)
		out[i] = n
	}
	return out
}

func addrs(ns []*named) []string {
	a := make([]string, len(ns))
	for i, n := range ns {
		a[i] = n.addr()
	}
	return a
}

// do sends one request and returns status and body.
func do(t *testing.T, method, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// deadAddr returns a loopback address where nothing listens (connect is refused).
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	_ = ln.Close()
	return a
}

func TestRoutingThroughProxy(t *testing.T) {
	ns := namedBackends(t, "api", "admin", "web", "canary")
	pool := func(n *named) *lb.Pool { return newPool(n.name, "http", lb.NewRoundRobin, n.addr()) }
	rt := mustRouter(t,
		proxy.Route{Name: "web", Hosts: []string{"*.example.com"}, Pool: pool(ns[2])},
		proxy.Route{Name: "api", Hosts: []string{"app.example.com"}, Path: "/api", Pool: pool(ns[0])},
		proxy.Route{Name: "admin", Hosts: []string{"app.example.com"}, Path: "/api/admin", Methods: []string{"GET"}, Pool: pool(ns[1])},
		proxy.Route{Name: "canary", Hosts: []string{"app.example.com"}, Path: "/api", Headers: []proxy.HeaderMatch{{Name: "x-canary", Value: "1"}}, Pool: pool(ns[3])},
	)
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = rt })
	for _, tc := range []struct {
		method, host, path string
		hdr                map[string]string
		want               string
	}{
		{"GET", "app.example.com", "/api/users", nil, "api"},
		{"GET", "app.example.com", "/api/admin/x", nil, "admin"},
		{"POST", "app.example.com", "/api/admin/x", nil, "api"}, // method mismatch falls to the prefix route
		{"GET", "app.example.com", "/api/users", map[string]string{"X-Canary": "1"}, "canary"},
		{"GET", "shop.example.com", "/api/users", nil, "web"},
	} {
		hdr := map[string]string{"Host": tc.host}
		for k, v := range tc.hdr {
			hdr[k] = v
		}
		if code, body := do(t, tc.method, base+tc.path, hdr); code != 200 || body != tc.want {
			t.Errorf("%s %s%s: %d %q, want 200 %q", tc.method, tc.host, tc.path, code, body, tc.want)
		}
	}
	if code, _ := do(t, "GET", base+"/", map[string]string{"Host": "other.org"}); code != http.StatusNotFound {
		t.Errorf("unrouted host: %d, want 404", code)
	}
}

func TestRoundRobinAndWeightedThroughProxy(t *testing.T) {
	t.Run("round robin", func(t *testing.T) {
		ns := namedBackends(t, "a", "b", "c")
		base, _ := startProxy(t, "", func(o *proxy.Options) {
			o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: newPool("p", "http", lb.NewRoundRobin, addrs(ns)...)})
		})
		for i := 0; i < 300; i++ {
			do(t, "GET", base+"/", nil)
		}
		for _, n := range ns {
			if n.hits.Load() != 100 {
				t.Errorf("%s got %d, want exactly 100", n.name, n.hits.Load())
			}
		}
	})
	t.Run("weighted 3:1", func(t *testing.T) {
		ns := namedBackends(t, "heavy", "light")
		bs := []*lb.Backend{lb.NewBackend(ns[0].addr(), 3), lb.NewBackend(ns[1].addr(), 1)}
		base, _ := startProxy(t, "", func(o *proxy.Options) {
			o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: &lb.Pool{Name: "p", Scheme: "http", Backends: bs, Balancer: lb.NewWeightedRoundRobin(bs)}})
		})
		for i := 0; i < 400; i++ {
			do(t, "GET", base+"/", nil)
		}
		if ns[0].hits.Load() != 300 || ns[1].hits.Load() != 100 {
			t.Errorf("got %d:%d, want exactly 300:100", ns[0].hits.Load(), ns[1].hits.Load())
		}
	})
}

// With requests that stay open, least-connections (and P2C with two backends,
// which always samples both) must fill backends evenly.
func TestLoadAwareBalancersThroughProxy(t *testing.T) {
	for name, algo := range map[string]func([]*lb.Backend) lb.Balancer{
		"least_conn": lb.NewLeastConnections,
		"p2c":        lb.NewPowerOfTwoChoices,
	} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			arrived := make(chan string, 16)
			var hold []string
			for _, nm := range []string{"x", "y"} {
				srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
					arrived <- nm
					<-release
				}))
				defer srv.Close()
				hold = append(hold, strings.TrimPrefix(srv.URL, "http://"))
			}
			base, _ := startProxy(t, "", func(o *proxy.Options) {
				o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: newPool("p", "http", algo, hold...)})
			})
			got := map[string]int{}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if resp, err := http.Get(base + "/"); err == nil {
						resp.Body.Close()
					}
				}()
				got[wait(t, arrived, 2*time.Second, "request to arrive")]++ // one at a time, so in-flight counts are exact
			}
			close(release)
			wg.Wait()
			if got["x"] != 4 || got["y"] != 4 {
				t.Errorf("open requests split %v, want 4/4", got)
			}
		})
	}
}

func TestConsistentHashThroughProxy(t *testing.T) {
	ns := namedBackends(t, "a", "b", "c")
	base, _ := startProxy(t, "", func(o *proxy.Options) {
		o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: newPool("p", "http",
			func(bs []*lb.Backend) lb.Balancer {
				return lb.NewConsistentHash(bs, lb.HeaderKey("X-User"), lb.DefaultVnodes)
			},
			addrs(ns)...)})
	})
	owners := map[string]bool{}
	for u := 0; u < 30; u++ {
		user := map[string]string{"X-User": fmt.Sprintf("user-%d", u)}
		_, first := do(t, "GET", base+"/", user)
		owners[first] = true
		for i := 0; i < 5; i++ {
			if _, again := do(t, "GET", base+"/", user); again != first {
				t.Fatalf("user-%d moved from %s to %s", u, first, again)
			}
		}
	}
	if len(owners) != 3 {
		t.Errorf("30 users landed on %d backends, want all 3", len(owners))
	}
}

func TestPassiveEjectionAndReadmissionThroughProxy(t *testing.T) {
	good := namedBackends(t, "good")[0]
	var broken atomic.Bool
	broken.Store(true)
	var flakyHits atomic.Int64
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flakyHits.Add(1)
		if broken.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "flaky")
	}))
	defer flaky.Close()

	pool := newPool("p", "http", lb.NewRoundRobin, good.addr(), strings.TrimPrefix(flaky.URL, "http://"))
	pool.EjectAfter, pool.EjectFor = 2, 300*time.Millisecond
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })

	for i := 0; i < 4; i++ { // round robin: two of these reach flaky and fail
		do(t, "GET", base+"/", nil)
	}
	if flakyHits.Load() != 2 {
		t.Fatalf("flaky hit %d times before ejection, want 2", flakyHits.Load())
	}
	for i := 0; i < 20; i++ {
		if code, body := do(t, "GET", base+"/", nil); code != 200 || body != "good" {
			t.Fatalf("after ejection got %d %q, want only the good backend", code, body)
		}
	}
	if flakyHits.Load() != 2 {
		t.Fatalf("ejected backend received %d more requests", flakyHits.Load()-2)
	}

	broken.Store(false)
	time.Sleep(350 * time.Millisecond) // EjectFor elapses: timed re-admission
	seen := false
	for i := 0; i < 4; i++ {
		if _, body := do(t, "GET", base+"/", nil); body == "flaky" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("recovered backend not re-admitted after EjectFor")
	}
}

func TestActiveHealthCheckThroughProxy(t *testing.T) {
	var sick atomic.Bool
	mk := func(name string, isSick func() bool) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" && isSick() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, name)
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	pool := newPool("p", "http", lb.NewRoundRobin, mk("a", func() bool { return false }), mk("b", sick.Load))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	health.Run(ctx, http.DefaultClient, pool, health.Config{Path: "/healthz", Interval: 10 * time.Millisecond, Timeout: time.Second, Healthy: 1, Unhealthy: 2}, nil, nil)
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })

	sick.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for pool.Backends[1].Healthy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		if _, body := do(t, "GET", base+"/", nil); body != "a" {
			t.Fatalf("unhealthy backend served a request (%q)", body)
		}
	}
	sick.Store(false)
	deadline = time.Now().Add(2 * time.Second)
	for !pool.Backends[1].Healthy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := map[string]int{}
	for i := 0; i < 10; i++ {
		_, body := do(t, "GET", base+"/", nil)
		got[body]++
	}
	if got["b"] == 0 {
		t.Fatalf("recovered backend got no traffic: %v", got)
	}
}

func TestAllBackendsDownIs503(t *testing.T) {
	pool := newPool("p", "http", lb.NewRoundRobin, "127.0.0.1:1")
	pool.Backends[0].SetHealthy(false)
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	decodeError(t, resp)
}

// retryPool is [dead, alive] in strict round-robin with retries on, and no ejection.
func retryPool(t *testing.T, alive *named, retries int, budget *lb.RetryBudget) *lb.Pool {
	pool := newPool("p", "http", lb.NewRoundRobin, deadAddr(t), alive.addr())
	pool.Retries, pool.Budget = retries, budget
	return pool
}

func TestRetriesIdempotentRequestsThatNeverReachedABackend(t *testing.T) {
	alive := namedBackends(t, "alive")[0]
	pool := retryPool(t, alive, 1, lb.NewRetryBudget(1, 100))
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })
	for i := 0; i < 10; i++ {
		if code, body := do(t, "GET", base+"/", nil); code != 200 || body != "alive" {
			t.Fatalf("GET %d: %d %q, want retry onto the live backend", i, code, body)
		}
	}
}

func TestNoRetryForNonIdempotentRequests(t *testing.T) {
	alive := namedBackends(t, "alive")[0]
	pool := retryPool(t, alive, 1, lb.NewRetryBudget(1, 100))
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })
	failed := 0
	for i := 0; i < 10; i++ {
		if code, _ := do(t, "POST", base+"/", nil); code == http.StatusBadGateway {
			failed++
		}
	}
	// Strict round-robin: every second POST is sent to the dead backend and must not be retried.
	if failed != 5 || alive.hits.Load() != 5 {
		t.Fatalf("POST: %d failed, alive got %d; want 5 and 5 (no retries)", failed, alive.hits.Load())
	}
}

// A backend that accepts the request and then drops the connection may have
// acted on it, so even a GET must not be retried.
func TestNoRetryOnceRequestReachedBackend(t *testing.T) {
	var dropped atomic.Int64
	dropper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dropped.Add(1)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer dropper.Close()
	alive := namedBackends(t, "alive")[0]
	pool := newPool("p", "http", lb.NewRoundRobin, strings.TrimPrefix(dropper.URL, "http://"), alive.addr())
	pool.Retries, pool.Budget = 1, lb.NewRetryBudget(1, 100)
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })
	failed := 0
	for i := 0; i < 6; i++ {
		if code, _ := do(t, "GET", base+"/", nil); code == http.StatusBadGateway {
			failed++
		}
	}
	if failed != 3 || alive.hits.Load() != 3 || dropped.Load() < 3 {
		t.Fatalf("failed=%d alive=%d dropped=%d; want 3/3/3 (no retry after headers were sent)", failed, alive.hits.Load(), dropped.Load())
	}
}

// With every backend dead, the budget caps the total extra attempts.
func TestRetryBudgetCapsAmplification(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	pool := newPool("p", "http", lb.NewRoundRobin, deadAddr(t), deadAddr(t))
	pool.Retries, pool.Budget = 1, lb.NewRetryBudget(0, 3) // 3 retries up front, none earned
	base, _ := startProxy(t, "", func(o *proxy.Options) {
		o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool})
		o.Logger = slog.New(slog.NewJSONHandler(lockedWriter{&mu, &logs}, nil))
	})
	for i := 0; i < 20; i++ {
		if code, _ := do(t, "GET", base+"/", nil); code != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	total := 0
	sc := bufio.NewScanner(&logs)
	for sc.Scan() {
		var e struct {
			Msg      string `json:"msg"`
			Attempts int    `json:"attempts"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Msg == "request" {
			total += e.Attempts
		}
	}
	if total != 23 {
		t.Fatalf("20 requests made %d attempts, want 23 (20 + a budget of 3)", total)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// An "echo" upgrade stands in for WebSocket: the handshake mechanics are identical.
func TestUpgradeTunnel(t *testing.T) {
	be := backend(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "echo") || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
			http.Error(w, "upgrade headers missing upstream", http.StatusBadRequest)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(conn, brw) // echo everything back
	})
	base, _ := startProxy(t, be.URL, func(o *proxy.Options) { o.Limits.WriteTimeout = 100 * time.Millisecond })
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "echo" || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("handshake: %d %v", resp.StatusCode, resp.Header)
	}
	for _, msg := range []string{"ping", "pong after idle"} {
		if msg != "ping" {
			time.Sleep(300 * time.Millisecond) // longer than WriteTimeout: the tunnel must survive idling
		}
		_, _ = io.WriteString(conn, msg)
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != msg {
			t.Fatalf("echo %q: got %q, %v", msg, buf, err)
		}
	}
}

func TestHTTP2ToTLSUpstream(t *testing.T) {
	be := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	be.EnableHTTP2 = true
	be.StartTLS()
	defer be.Close()
	// The pool's own transport trusts the test CA (what -upstream-ca does); the
	// proxy's default transport would reject this certificate.
	tr := proxy.NewTransport(proxy.DefaultLimits)
	tr.TLSClientConfig = be.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	pool := newPool("tls", "https", lb.NewRoundRobin, strings.TrimPrefix(be.URL, "https://"))
	pool.Transport = tr
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })
	if code, body := do(t, "GET", base+"/", nil); code != 200 || body != "HTTP/2.0" {
		t.Fatalf("upstream saw %q (status %d), want HTTP/2.0", body, code)
	}
}
