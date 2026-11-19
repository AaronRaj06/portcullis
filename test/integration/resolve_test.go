package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

func TestCanarySplitAcrossPools(t *testing.T) {
	ns := namedBackends(t, "stable", "canary")
	stable := newPool("stable", "http", lb.NewRoundRobin, ns[0].addr())
	canary := newPool("canary", "http", lb.NewRoundRobin, ns[1].addr())
	base, _ := startProxy(t, "", func(o *proxy.Options) {
		o.Router = mustRouter(t, proxy.Route{Name: "app", Split: []proxy.WeightedPool{{Pool: stable, Weight: 90}, {Pool: canary, Weight: 10}}})
	})
	for i := 0; i < 200; i++ {
		do(t, "GET", base+"/", nil)
	}
	if ns[0].hits.Load() != 180 || ns[1].hits.Load() != 20 {
		t.Fatalf("split %d/%d, want exactly 180/20", ns[0].hits.Load(), ns[1].hits.Load())
	}
}

func TestZeroWeightPoolGetsNoTraffic(t *testing.T) {
	ns := namedBackends(t, "live", "parked")
	base, _ := startProxy(t, "", func(o *proxy.Options) {
		o.Router = mustRouter(t, proxy.Route{Name: "app", Split: []proxy.WeightedPool{
			{Pool: newPool("live", "http", lb.NewRoundRobin, ns[0].addr()), Weight: 1},
			{Pool: newPool("parked", "http", lb.NewRoundRobin, ns[1].addr()), Weight: 0},
		}})
	})
	for i := 0; i < 20; i++ {
		do(t, "GET", base+"/", nil)
	}
	if ns[1].hits.Load() != 0 {
		t.Fatalf("weight-0 pool got %d requests", ns[1].hits.Load())
	}
}

// Behind a trusted load balancer every request comes from the same peer IP.
// Hashing on the peer would pin all users to one backend; we must hash on the
// real client from X-Forwarded-For.
func TestHashOnRealClientBehindTrustedProxy(t *testing.T) {
	ns := namedBackends(t, "a", "b", "c")
	base, _ := startProxy(t, "", func(o *proxy.Options) {
		o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: newPool("p", "http",
			func(bs []*lb.Backend) lb.Balancer { return lb.NewConsistentHash(bs, lb.ClientIPKey, lb.DefaultVnodes) },
			addrs(ns)...)})
	})
	owners := map[string]bool{}
	for c := 0; c < 30; c++ {
		xff := map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", c)}
		_, first := do(t, "GET", base+"/", xff)
		owners[first] = true
		if _, again := do(t, "GET", base+"/", xff); again != first {
			t.Fatalf("client %d moved backends", c)
		}
	}
	if len(owners) != 3 {
		t.Fatalf("30 distinct clients behind one proxy landed on %d backends, want 3", len(owners))
	}
}

func TestPanicModeServesWhenHealthChecksAreWrong(t *testing.T) {
	ns := namedBackends(t, "a")
	pool := newPool("p", "http", lb.NewRoundRobin, ns[0].addr())
	pool.Backends[0].SetHealthy(false) // health check says down; the app is actually fine
	pool.Panic = true
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })
	if code, body := do(t, "GET", base+"/", nil); code != 200 || body != "a" {
		t.Fatalf("panic mode: %d %q, want 200 from the only backend", code, body)
	}
}

func TestH2CUpstream(t *testing.T) {
	be := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	be.Config.Protocols = new(http.Protocols)
	be.Config.Protocols.SetUnencryptedHTTP2(true)
	be.Start()
	defer be.Close()
	pool := newPool("grpcish", "http", lb.NewRoundRobin, strings.TrimPrefix(be.URL, "http://"))
	pool.Transport = proxy.NewH2CTransport(proxy.DefaultLimits)
	base, _ := startProxy(t, "", func(o *proxy.Options) { o.Router = mustRouter(t, proxy.Route{Name: "r", Pool: pool}) })
	if code, body := do(t, "GET", base+"/", nil); code != 200 || body != "HTTP/2.0" {
		t.Fatalf("upstream saw %q (status %d), want HTTP/2.0 over cleartext", body, code)
	}
}

// echoUpgradeBackend speaks a toy "echo" upgrade protocol.
func echoUpgradeBackend(t *testing.T) *httptest.Server {
	return backend(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(conn, brw)
	})
}

// startDrainable runs a proxy whose shutdown uses the given drain timeout.
func startDrainable(t *testing.T, upstream string, drain time.Duration) (addr string, shutdown func() error) {
	t.Helper()
	pool := newPool("p", "http", lb.NewRoundRobin, strings.TrimPrefix(upstream, "http://"))
	h, err := proxy.New(proxy.Options{Router: mustRouter(t, proxy.Route{Name: "r", Pool: pool}), Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- proxy.Serve(ctx, proxy.NewServer(h, proxy.DefaultLimits, nil), ln, drain) }()
	return ln.Addr().String(), func() error { cancel(); return <-errc }
}

func openTunnel(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	buf := make([]byte, 512)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 101") {
		t.Fatalf("handshake failed: %q %v", buf[:n], err)
	}
	return conn
}

func TestShutdownWaitsForTunnels(t *testing.T) {
	addr, shutdown := startDrainable(t, echoUpgradeBackend(t).URL, 5*time.Second)
	conn := openTunnel(t, addr)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- shutdown() }()

	time.Sleep(200 * time.Millisecond)
	_, _ = io.WriteString(conn, "still here") // the tunnel keeps working during the drain
	buf := make([]byte, 10)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "still here" {
		t.Fatalf("tunnel broken during drain: %q %v", buf, err)
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown finished (%v) while a tunnel was still open", err)
	default:
	}
	_ = conn.Close() // client hangs up: the drain can finish early
	if err := wait(t, done, 2*time.Second, "shutdown after tunnel closed"); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("drain took %s; it should end as soon as the tunnel closes", el)
	}
}

func TestShutdownClosesTunnelsAtDeadline(t *testing.T) {
	addr, shutdown := startDrainable(t, echoUpgradeBackend(t).URL, 300*time.Millisecond)
	conn := openTunnel(t, addr)
	defer conn.Close()
	start := time.Now()
	if err := shutdown(); err == nil {
		t.Error("shutdown with a tunnel still open past the deadline should report an incomplete drain")
	}
	if el := time.Since(start); el < 250*time.Millisecond || el > 2*time.Second {
		t.Errorf("shutdown took %s, want about the 300ms drain timeout", el)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("tunnel still open after the drain deadline")
	} else if ne := net.Error(nil); errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("tunnel not closed by the proxy (read timed out)")
	}
}
