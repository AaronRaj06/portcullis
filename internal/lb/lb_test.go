package lb

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func backends(weights ...int) []*Backend {
	bs := make([]*Backend, len(weights))
	for i, w := range weights {
		bs[i] = NewBackend(fmt.Sprintf("10.0.0.%d:80", i+1), w)
	}
	return bs
}

func req(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r
}

// counts runs n picks and returns hits per backend.
func counts(t *testing.T, bal Balancer, n int, mk func(i int) *http.Request) map[*Backend]int {
	t.Helper()
	now := time.Now().UnixNano()
	c := map[*Backend]int{}
	for i := 0; i < n; i++ {
		b := bal.Pick(mk(i), nil, now)
		if b == nil {
			t.Fatal("Pick returned nil with healthy backends")
		}
		c[b]++
	}
	return c
}

// within checks got is within tol (fraction) of want.
func within(t *testing.T, name string, got, want int, tol float64) {
	t.Helper()
	if math.Abs(float64(got-want)) > tol*float64(want) {
		t.Errorf("%s: got %d, want %d ±%.0f%%", name, got, want, tol*100)
	}
}

func TestSmoothWeightedSequence(t *testing.T) {
	bs := backends(5, 1, 1)
	seq := smoothSequence(bs, []int{5, 1, 1})
	got := ""
	for _, b := range seq {
		got += string(rune('a' + (b.Addr[7] - '1')))
	}
	if got != "aabacaa" { // the published NGINX example for weights 5,1,1
		t.Fatalf("sequence = %q, want aabacaa", got)
	}
	if n := len(smoothSequence(backends(4, 2), []int{4, 2})); n != 3 {
		t.Errorf("weights 4:2 cycle length %d, want 3 (divided by GCD)", n)
	}
}

func TestDistribution(t *testing.T) {
	const n = 60000
	anyReq := func(int) *http.Request { return req("/") }

	t.Run("round robin is exact", func(t *testing.T) {
		bs := backends(5, 1, 1) // weights ignored
		c := counts(t, NewRoundRobin(bs), n, anyReq)
		for _, b := range bs {
			if c[b] != n/3 {
				t.Errorf("%s got %d, want exactly %d", b.Addr, c[b], n/3)
			}
		}
	})
	t.Run("weighted round robin follows weights exactly", func(t *testing.T) {
		bs := backends(3, 2, 1)
		c := counts(t, NewWeightedRoundRobin(bs), n, anyReq)
		for i, want := range []int{n / 2, n / 3, n / 6} {
			if c[bs[i]] != want {
				t.Errorf("%s got %d, want %d", bs[i].Addr, c[bs[i]], want)
			}
		}
	})
	t.Run("p2c is roughly even when idle", func(t *testing.T) {
		bs := backends(1, 1, 1, 1)
		c := counts(t, NewPowerOfTwoChoices(bs), n, anyReq)
		for _, b := range bs {
			within(t, b.Addr, c[b], n/4, 0.05)
		}
	})
	t.Run("consistent hash spreads distinct keys", func(t *testing.T) {
		// Ring balance improves as about 1/sqrt(vnodes). Measured worst-case arc
		// deviation for 4 backends: 160 vnodes 16%, 640 vnodes 4.7%.
		bs := backends(1, 1, 1, 1)
		bal := NewConsistentHash(bs, PathKey, DefaultVnodes)
		c := counts(t, bal, n, func(i int) *http.Request { return req(fmt.Sprintf("/user/%d", i)) })
		for _, b := range bs {
			within(t, b.Addr, c[b], n/4, 0.10)
		}
	})
	t.Run("consistent hash respects weights", func(t *testing.T) {
		bs := backends(3, 1)
		c := counts(t, NewConsistentHash(bs, PathKey, 160), n, func(i int) *http.Request { return req(fmt.Sprintf("/k/%d", i)) })
		within(t, bs[0].Addr, c[bs[0]], n*3/4, 0.1)
	})
}

func TestLeastConnectionsPrefersIdle(t *testing.T) {
	bs := backends(1, 1, 1)
	bs[0].inflight.Store(5)
	bs[1].inflight.Store(1)
	bs[2].inflight.Store(3)
	bal := NewLeastConnections(bs)
	for i := 0; i < 10; i++ {
		if b := bal.Pick(req("/"), nil, 0); b != bs[1] {
			t.Fatalf("picked %s, want the backend with 1 in flight", b.Addr)
		}
	}
	// Ties rotate instead of always hitting the first backend.
	for _, b := range bs {
		b.inflight.Store(0)
	}
	c := counts(t, bal, 300, func(int) *http.Request { return req("/") })
	for _, b := range bs {
		if c[b] != 100 {
			t.Errorf("tie-break: %s got %d, want 100", b.Addr, c[b])
		}
	}
}

func TestP2CAvoidsLoadedBackend(t *testing.T) {
	bs := backends(1, 1, 1, 1)
	bs[0].inflight.Store(100)
	c := counts(t, NewPowerOfTwoChoices(bs), 10000, func(int) *http.Request { return req("/") })
	if c[bs[0]] != 0 {
		t.Errorf("heavily loaded backend picked %d times; P2C should always prefer the other sample", c[bs[0]])
	}
}

func TestConsistentHashStability(t *testing.T) {
	bs := backends(1, 1, 1, 1, 1)
	key := HeaderKey("X-User")
	before := NewConsistentHash(bs, key, 160)
	after := NewConsistentHash(bs[:4], key, 160) // remove one backend
	const n = 20000
	moved, ownedByRemoved := 0, 0
	for i := 0; i < n; i++ {
		r := req("/")
		r.Header.Set("X-User", fmt.Sprintf("u%d", i))
		a, b := before.Pick(r, nil, 0), after.Pick(r, nil, 0)
		if a == bs[4] {
			ownedByRemoved++
		} else if a != b {
			moved++ // a key whose backend still exists must not move
		}
		if a != before.Pick(r, nil, 0) {
			t.Fatal("same key mapped to different backends")
		}
	}
	if moved != 0 {
		t.Errorf("%d keys moved between surviving backends, want 0", moved)
	}
	within(t, "keys owned by removed backend", ownedByRemoved, n/5, 0.2)
}

func TestCookieAndIPKeys(t *testing.T) {
	r := req("/")
	r.AddCookie(&http.Cookie{Name: "sid", Value: "abc"})
	r.RemoteAddr = "192.0.2.7:5555"
	if CookieKey("sid")(r) != "abc" || CookieKey("nope")(r) != "" || ClientIPKey(r) != "192.0.2.7" {
		t.Fatal("key extraction wrong")
	}
	// Missing key falls back to client IP, so it is stable per client.
	bal := NewConsistentHash(backends(1, 1, 1), HeaderKey("X-Missing"), 50)
	first := bal.Pick(r, nil, 0)
	for i := 0; i < 20; i++ {
		if bal.Pick(r, nil, 0) != first {
			t.Fatal("fallback key not stable")
		}
	}
}

// Every algorithm must skip unavailable and already-tried backends, and return
// nil when nothing is left.
func TestAllAlgorithmsSkipUnavailable(t *testing.T) {
	algos := map[string]func([]*Backend) Balancer{
		"rr":         NewRoundRobin,
		"wrr":        NewWeightedRoundRobin,
		"least_conn": NewLeastConnections,
		"p2c":        NewPowerOfTwoChoices,
		"hash":       func(bs []*Backend) Balancer { return NewConsistentHash(bs, PathKey, 50) },
	}
	for name, mk := range algos {
		t.Run(name, func(t *testing.T) {
			bs := backends(1, 2, 1)
			bal := mk(bs)
			now := time.Now().UnixNano()
			bs[0].SetHealthy(false)
			bs[1].ejectedUntil.Store(now + int64(time.Hour))
			for i := 0; i < 200; i++ {
				if b := bal.Pick(req(fmt.Sprintf("/%d", i)), nil, now); b != bs[2] {
					t.Fatalf("picked %v, want only the available backend", b)
				}
			}
			if b := bal.Pick(req("/"), []*Backend{bs[2]}, now); b != nil {
				t.Fatalf("picked %s although it was already tried", b.Addr)
			}
			if bal.Pick(req("/"), nil, now+int64(2*time.Hour)) == nil {
				t.Fatal("ejected backend not re-admitted after its ejection expired")
			}
		})
	}
}

func TestRoundRobinSpreadsDeadBackendsShare(t *testing.T) {
	bs := backends(1, 1, 1)
	bs[2].SetHealthy(false)
	c := counts(t, NewRoundRobin(bs), 30000, func(int) *http.Request { return req("/") })
	within(t, bs[0].Addr, c[bs[0]], 15000, 0.05)
	within(t, bs[1].Addr, c[bs[1]], 15000, 0.05)
}

func TestPassiveEjectionAndReadmission(t *testing.T) {
	bs := backends(1, 1)
	p := &Pool{Backends: bs, Balancer: NewRoundRobin(bs), EjectAfter: 3, EjectFor: 50 * time.Millisecond}
	b := bs[0]
	p.Report(b, true)
	p.Report(b, true)
	p.Report(b, false) // a success resets the streak
	p.Report(b, true)
	if p.Report(b, true) || b.Ejected(time.Now().UnixNano()) {
		t.Fatal("ejected after non-consecutive failures")
	}
	if !p.Report(b, true) {
		t.Fatal("not ejected after 3 consecutive failures")
	}
	for i := 0; i < 20; i++ {
		if got, _ := p.Pick(req("/"), nil); got == b {
			t.Fatal("ejected backend received traffic")
		}
	}
	time.Sleep(60 * time.Millisecond)
	seen := false
	for i := 0; i < 4; i++ {
		if got, _ := p.Pick(req("/"), nil); got == b {
			seen = true
		}
	}
	if !seen {
		t.Fatal("backend not re-admitted after EjectFor")
	}
}

func TestPoolNoBackend(t *testing.T) {
	bs := backends(1)
	bs[0].SetHealthy(false)
	p := &Pool{Backends: bs, Balancer: NewRoundRobin(bs)}
	if _, err := p.Pick(req("/"), nil); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("err = %v, want ErrNoBackend", err)
	}
}

func TestRetryBudget(t *testing.T) {
	// withdrawals counts how many retries the budget grants right now.
	withdrawals := func(b *RetryBudget) int {
		n := 0
		for b.Withdraw() {
			n++
		}
		return n
	}
	b := NewRetryBudget(0.2, 2)
	if n := withdrawals(b); n != 2 {
		t.Fatalf("burst of 2 granted %d", n)
	}
	for i := 0; i < 10; i++ {
		b.Deposit()
	}
	if n := withdrawals(b); n != 2 {
		t.Fatalf("10 requests at 20%% granted %d retries, want 2", n)
	}
	for i := 0; i < 1000; i++ {
		b.Deposit()
	}
	if got := b.balance.Load(); got != 2000 {
		t.Fatalf("balance %d exceeds cap of 2 tokens", got)
	}
}

// Under concurrency the budget must never hand out more retries than it earned.
func TestRetryBudgetConcurrent(t *testing.T) {
	b := NewRetryBudget(0.1, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := 0
			for i := 0; i < 10000; i++ {
				b.Deposit()
				if b.Withdraw() {
					mine++
				}
			}
			mu.Lock()
			granted += mine
			mu.Unlock()
		}()
	}
	wg.Wait()
	// Safety: never more than earned (80,000 requests × 10%). There is no exact
	// lower bound: deposits that arrive while the balance is at its cap are
	// dropped by design. Accrual is checked single-threaded in TestRetryBudget.
	if granted > 8000 || granted == 0 {
		t.Fatalf("granted %d retries, want 1..8000", granted)
	}
}

func TestSetHealthyReportsChange(t *testing.T) {
	b := NewBackend("x:1", 0)
	if b.Weight != 1 {
		t.Errorf("weight 0 not clamped to 1")
	}
	if b.SetHealthy(true) {
		t.Error("up→up reported as change")
	}
	if !b.SetHealthy(false) || b.Healthy() {
		t.Error("up→down not reported")
	}
	if !b.SetHealthy(true) || !b.Healthy() {
		t.Error("down→up not reported")
	}
}

func BenchmarkPick(b *testing.B) {
	bs := backends(1, 2, 3, 1, 2, 3, 1, 2)
	r := req("/some/path")
	for name, bal := range map[string]Balancer{
		"rr": NewRoundRobin(bs), "wrr": NewWeightedRoundRobin(bs), "least_conn": NewLeastConnections(bs),
		"p2c": NewPowerOfTwoChoices(bs), "hash": NewConsistentHash(bs, PathKey, 160),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			now := time.Now().UnixNano()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					bal.Pick(r, nil, now)
				}
			})
		})
	}
}
