// Package ratelimit provides a sharded, memory-bounded token-bucket rate limiter.
//
// A token bucket holds up to Limit tokens and refills Limit tokens per Window,
// continuously. Each request takes one token; with none left it is over the
// limit. So a client may burst up to Limit requests at once, and sustains
// Limit per Window on average.
package ratelimit

import (
	"hash/maphash"
	"sync"
	"time"
)

const shards = 64 // a power of two; spreads lock contention across cores

// DefaultMaxKeys bounds one Limiter's memory: about 100 bytes per key plus the key itself.
const DefaultMaxKeys = 100_000

// Limiter is a set of token buckets, one per key. It is safe for concurrent use.
type Limiter struct {
	limit    float64
	perNano  float64 // refill rate, tokens per nanosecond
	window   time.Duration
	maxShard int
	seed     maphash.Seed
	now      func() time.Time // replaced in tests
	shard    [shards]shard
}

type shard struct {
	mu sync.Mutex
	m  map[string]*bucket // pointers: an existing key updates in place, with no map write
}

type bucket struct {
	tokens float64
	last   int64 // unix nanoseconds of the last refill
}

// New returns a limiter allowing limit requests per window per key, holding at
// most maxKeys keys (<= 0 means DefaultMaxKeys).
func New(limit int, window time.Duration, maxKeys int) *Limiter {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	l := &Limiter{
		limit: float64(limit), perNano: float64(limit) / float64(window), window: window,
		maxShard: max(1, maxKeys/shards), seed: maphash.MakeSeed(), now: time.Now,
	}
	for i := range l.shard {
		l.shard[i].m = map[string]*bucket{}
	}
	return l
}

// Allow takes a token for key and reports whether one was available.
func (l *Limiter) Allow(key string) bool {
	s := &l.shard[maphash.String(l.seed, key)&(shards-1)]
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[key]
	if b == nil {
		b = l.insert(s, key)
	}
	return l.take(b)
}

// AllowBytes is Allow for a key in a byte slice. A key seen before costs no
// allocation (the map lookup does not copy key); only a new key is copied.
func (l *Limiter) AllowBytes(key []byte) bool {
	s := &l.shard[maphash.Bytes(l.seed, key)&(shards-1)]
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[string(key)]
	if b == nil {
		b = l.insert(s, string(key))
	}
	return l.take(b)
}

func (l *Limiter) insert(s *shard, key string) *bucket {
	now := l.now().UnixNano()
	if len(s.m) >= l.maxShard {
		l.evict(s, now)
	}
	b := &bucket{tokens: l.limit, last: now}
	s.m[key] = b
	return b
}

// take refills b for the time since its last use, then takes a token if there is one.
func (l *Limiter) take(b *bucket) bool {
	if now := l.now().UnixNano(); now > b.last {
		b.tokens = min(l.limit, b.tokens+float64(now-b.last)*l.perNano)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evict makes room in a full shard. First it drops keys idle long enough to be
// full again: forgetting them changes nothing, since a new key starts full.
// If that frees nothing (many active keys), it drops an arbitrary eighth.
// Forgetting an active key lets that key start over with a full bucket, a
// bounded leniency that is the price of bounded memory.
func (l *Limiter) evict(s *shard, now int64) {
	idle := int64(l.window)
	for k, b := range s.m {
		if now-b.last >= idle {
			delete(s.m, k)
		}
	}
	if len(s.m) < l.maxShard {
		return
	}
	n := max(1, len(s.m)/8)
	for k := range s.m { // Go randomises map iteration order
		delete(s.m, k)
		if n--; n == 0 {
			return
		}
	}
}

// Len returns the number of keys held (for tests and metrics).
func (l *Limiter) Len() int {
	n := 0
	for i := range l.shard {
		l.shard[i].mu.Lock()
		n += len(l.shard[i].m)
		l.shard[i].mu.Unlock()
	}
	return n
}
