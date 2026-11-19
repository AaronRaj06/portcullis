package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AaronRaj06/portcullis/internal/admin"
	"github.com/AaronRaj06/portcullis/internal/config"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// variants are valid configs that differ in algorithm, backend set and
// split, so every reload really changes the routing.
func variants(a, b, c string) []string {
	pool := func(name, lb string, addrs ...string) string {
		var bs []string
		for _, x := range addrs {
			bs = append(bs, fmt.Sprintf("{address: %q}", x))
		}
		return fmt.Sprintf("{name: %s, lb: %s, backends: [%s]}", name, lb, strings.Join(bs, ", "))
	}
	return []string{
		"pools: [" + pool("p", "round_robin", a, b, c) + "]\nroutes: [{name: r, backends: [{pool: p}]}]",
		"pools: [" + pool("p", "least_conn", a, b) + "]\nroutes: [{name: r, backends: [{pool: p}]}]",
		"pools: [" + pool("p", "p2c", c) + "]\nroutes: [{name: r, backends: [{pool: p}]}]",
		"pools: [" + pool("stable", "round_robin", a) + ", " + pool("canary", "hash", b, c) + "]\n" +
			"routes: [{name: r, backends: [{pool: stable, weight: 90}, {pool: canary, weight: 10}]}]",
		"pools: [" + pool("p", "weighted_round_robin", b, a) + "]\nroutes: [{name: r, path: /, backends: [{pool: p}]}, {name: api, path: /api, backends: [{pool: p}]}]",
	}
}

// Phase 3 "done when": sustained load while reloading 100 times; failed requests
// are counted (target 0), and invalid configs never replace a good one.
func TestHotReloadUnderLoad(t *testing.T) {
	ns := namedBackends(t, "a", "b", "c")
	vs := variants(ns[0].addr(), ns[1].addr(), ns[2].addr())

	h, err := proxy.New(proxy.Options{Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	mgr := config.NewManager(context.Background(), h, proxy.DefaultLimits, "", nil, nil)
	if _, err := mgr.Apply([]byte(vs[0]), "test"); err != nil {
		t.Fatal(err)
	}
	adm := httptest.NewServer((&admin.Server{Manager: mgr, Gatherer: prometheus.NewRegistry()}).Handler())
	defer adm.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()
	base := srv.URL

	var total, failed atomic.Int64
	var firstFailure atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}, Timeout: 5 * time.Second}
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				path := "/"
				if i%2 == 1 {
					path = fmt.Sprintf("/api/%d", g)
				}
				resp, err := client.Get(base + path)
				total.Add(1)
				if err != nil {
					failed.Add(1)
					firstFailure.CompareAndSwap(nil, err.Error())
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					failed.Add(1)
					firstFailure.CompareAndSwap(nil, resp.Status)
				}
			}
		}(g)
	}

	applied, rejected := 0, 0
	for i := 0; i < 100; i++ {
		raw := vs[(i+1)%len(vs)]
		if i%10 == 9 { // every 10th push is broken
			raw = "pools: [{name: p, backends: [{address: no-port}]}]\nroutes: [{name: r, backends: [{pool: p}]}]"
		}
		before := h.Current()
		var ok bool
		if i%2 == 0 { // half through the manager directly (file/SIGHUP path) ...
			_, err := mgr.Apply([]byte(raw), "file")
			ok = err == nil
		} else { // ... half through the admin API (controller path)
			req, _ := http.NewRequest(http.MethodPut, adm.URL+"/config", strings.NewReader(raw))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			ok = resp.StatusCode == 200
		}
		if ok {
			applied++
		} else {
			rejected++
			if h.Current() != before {
				t.Fatalf("reload %d: rejected config replaced the serving snapshot", i)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	t.Logf("reloads: %d applied, %d rejected; requests: %d total, %d failed", applied, rejected, total.Load(), failed.Load())
	if applied != 90 || rejected != 10 {
		t.Fatalf("applied %d / rejected %d, want 90 / 10", applied, rejected)
	}
	if failed.Load() != 0 {
		t.Fatalf("%d of %d requests failed during reloads; first: %v", failed.Load(), total.Load(), firstFailure.Load())
	}
	if total.Load() < 1000 {
		t.Fatalf("only %d requests ran; the load was not sustained", total.Load())
	}
	if g := mgr.Status().Generation; g != 91 {
		t.Fatalf("generation %d, want 91 (1 initial + 90 applied)", g)
	}
}

// A request that started on the old snapshot finishes on it, even when the
// reload removes its backend.
func TestInFlightRequestFinishesOnOldSnapshot(t *testing.T) {
	started := make(chan struct{})
	slow := backend(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "old")
	})
	other := namedBackends(t, "new")[0]
	h, _ := proxy.New(proxy.Options{Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits})
	mgr := config.NewManager(context.Background(), h, proxy.DefaultLimits, "", nil, nil)
	cfg := func(addr string) []byte {
		return []byte(fmt.Sprintf("pools: [{name: p, backends: [{address: %q}]}]\nroutes: [{name: r, backends: [{pool: p}]}]", addr))
	}
	if _, err := mgr.Apply(cfg(strings.TrimPrefix(slow.URL, "http://")), "t"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res := make(chan string, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			res <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		res <- fmt.Sprintf("%d %s", resp.StatusCode, b)
	}()
	wait(t, started, 2*time.Second, "slow request to start")
	if _, err := mgr.Apply(cfg(other.addr()), "t"); err != nil {
		t.Fatal(err)
	}
	if _, body := do(t, "GET", srv.URL+"/", nil); body != "new" {
		t.Fatalf("new request served %q, want the new config", body)
	}
	if got := wait(t, res, 3*time.Second, "in-flight request"); got != "200 old" {
		t.Fatalf("in-flight request got %q, want 200 old", got)
	}
}
