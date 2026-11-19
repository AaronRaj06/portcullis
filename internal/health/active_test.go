package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AaronRaj06/portcullis/internal/lb"
)

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestActiveCheckThresholds(t *testing.T) {
	var ok atomic.Bool
	ok.Store(true)
	var checks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("checked path %q", r.URL.Path)
		}
		checks.Add(1)
		if !ok.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	b := lb.NewBackend(strings.TrimPrefix(srv.URL, "http://"), 1)
	pool := &lb.Pool{Name: "p", Scheme: "http", Backends: []*lb.Backend{b}}
	var changes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := Config{Path: "/healthz", Interval: 10 * time.Millisecond, Timeout: time.Second, Healthy: 2, Unhealthy: 3, Jitter: 0.2}
	Run(ctx, srv.Client(), pool, cfg, func(string, *lb.Backend, bool) { changes.Add(1) }, nil)

	waitFor(t, time.Second, "first checks", func() bool { return checks.Load() >= 3 })
	if !b.Healthy() || changes.Load() != 0 {
		t.Fatal("healthy backend marked down")
	}

	ok.Store(false)
	start := checks.Load()
	waitFor(t, 2*time.Second, "backend marked down", func() bool { return !b.Healthy() })
	if n := checks.Load() - start; n < 3 {
		t.Errorf("marked down after %d failed checks, threshold is 3", n)
	}

	ok.Store(true)
	start = checks.Load()
	waitFor(t, 2*time.Second, "backend re-admitted", func() bool { return b.Healthy() })
	if n := checks.Load() - start; n < 2 {
		t.Errorf("marked up after %d passing checks, threshold is 2", n)
	}
	if changes.Load() != 2 {
		t.Errorf("OnChange called %d times, want 2 (down, up)", changes.Load())
	}
}

func TestUnreachableAndSlowBackendsFail(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer slow.Close()
	ctx := context.Background()
	if probe(ctx, http.DefaultClient, slow.URL+"/", 50*time.Millisecond) {
		t.Error("slow backend passed a 50ms check")
	}
	if probe(ctx, http.DefaultClient, "http://127.0.0.1:1/", time.Second) {
		t.Error("closed port passed")
	}
}

func TestJitterStaysInRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := jittered(100*time.Millisecond, 0.1)
		if d < 90*time.Millisecond || d > 110*time.Millisecond {
			t.Fatalf("jittered = %s, want 90-110ms", d)
		}
	}
	if jittered(time.Second, 0) != time.Second {
		t.Fatal("zero jitter changed the interval")
	}
}
