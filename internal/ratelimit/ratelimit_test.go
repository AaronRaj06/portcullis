package ratelimit

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func fixed(l *Limiter) *clock        { c := &clock{t: time.Unix(1000, 0)}; l.now = c.now; return c }
func allowN(l *Limiter, k string, n int) (ok int) {
	for range n {
		if l.Allow(k) {
			ok++
		}
	}
	return ok
}

func TestBurstThenRefill(t *testing.T) {
	l := New(10, time.Second, 0)
	c := fixed(l)
	if got := allowN(l, "a", 15); got != 10 {
		t.Fatalf("burst: allowed %d, want 10", got)
	}
	c.add(500 * time.Millisecond) // half a window refills 5 tokens
	if got := allowN(l, "a", 10); got != 5 {
		t.Fatalf("after 500ms: allowed %d, want 5", got)
	}
	c.add(time.Hour) // refill is capped at the limit
	if got := allowN(l, "a", 20); got != 10 {
		t.Fatalf("after idle: allowed %d, want 10", got)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, time.Minute, 0)
	fixed(l)
	if !l.Allow("a") || l.Allow("a") || !l.Allow("b") {
		t.Fatal("each key needs its own bucket")
	}
}

func TestMemoryIsBounded(t *testing.T) {
	l := New(1, time.Minute, 640) // 10 keys per shard
	c := fixed(l)
	for i := range 100_000 {
		l.Allow(strconv.Itoa(i))
		if i%1000 == 0 {
			c.add(time.Millisecond)
		}
	}
	if n := l.Len(); n > 640 {
		t.Fatalf("holding %d keys, limit 640", n)
	}
	// Idle keys are dropped first, and dropping them is invisible: a new key starts full.
	c.add(2 * time.Minute)
	l.Allow("fresh")
	if !l.Allow("other") {
		t.Fatal("new key after eviction should be allowed")
	}
}

// TestConcurrentExactCount: under contention, a key never gets more than its tokens.
func TestConcurrentExactCount(t *testing.T) {
	l := New(1000, time.Hour, 0) // refill is negligible during the test
	var ok atomic.Int64
	var wg sync.WaitGroup
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				if l.Allow("shared") {
					ok.Add(1)
				}
				l.Allow("k" + strconv.Itoa(g*1000+i)) // also churn other shards
			}
		}()
	}
	wg.Wait()
	if got := ok.Load(); got < 1000 || got > 1001 { // at most a fraction of a token refills
		t.Fatalf("allowed %d for a 1000-token bucket", got)
	}
}

func BenchmarkAllow(b *testing.B) {
	l := New(1<<30, time.Second, 0)
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = "10.0.0." + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.Allow(keys[i&1023])
			i++
		}
	})
}
