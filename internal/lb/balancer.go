package lb

import (
	"math/rand/v2"
	"net/http"
	"sync/atomic"
)

// Balancer chooses a backend for a request.
type Balancer interface {
	// Pick returns an available backend that is not in tried, or nil if there is none.
	// now is the current time in unix nanoseconds (passed in so tests control time).
	Pick(r *http.Request, tried []*Backend, now int64) *Backend
}

// schedule implements round-robin and smooth weighted round-robin lock-free:
// the order is precomputed once, and requests walk it with an atomic counter.
type schedule struct {
	seq  []*Backend
	next atomic.Uint64
}

// NewRoundRobin cycles through backends in order, ignoring weights.
func NewRoundRobin(bs []*Backend) Balancer {
	w := make([]int, len(bs))
	for i := range w {
		w[i] = 1
	}
	return &schedule{seq: smoothSequence(bs, w)}
}

// NewWeightedRoundRobin sends traffic in proportion to Backend.Weight, interleaved
// (A A B A for 3:1 becomes A B A A ...), not in bursts.
func NewWeightedRoundRobin(bs []*Backend) Balancer {
	w := make([]int, len(bs))
	for i, b := range bs {
		w[i] = b.Weight
	}
	return &schedule{seq: smoothSequence(bs, w)}
}

// smoothSequence is NGINX's smooth weighted round-robin, run once to produce
// one full cycle. Weights are divided by their GCD to keep the cycle short.
func smoothSequence(bs []*Backend, w []int) []*Backend {
	if len(bs) == 0 {
		return nil
	}
	g := 0
	for _, x := range w {
		g = gcd(g, x)
	}
	total := 0
	for i := range w {
		w[i] /= g
		total += w[i]
	}
	cur := make([]int, len(bs))
	seq := make([]*Backend, 0, total)
	for len(seq) < total {
		best := 0
		for i := range bs {
			cur[i] += w[i]
			if cur[i] > cur[best] {
				best = i
			}
		}
		cur[best] -= total
		seq = append(seq, bs[best])
	}
	return seq
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (s *schedule) Pick(_ *http.Request, tried []*Backend, now int64) *Backend {
	n := uint64(len(s.seq))
	if n == 0 {
		return nil
	}
	if b := s.seq[(s.next.Add(1)-1)%n]; usable(b, now, tried) {
		return b
	}
	// The scheduled backend is unusable. Sample random slots so its share is spread
	// in proportion to the others' weights. A linear scan would hand it all to the
	// next backend in the cycle. Fall back to a scan if sampling keeps missing.
	for i := uint64(0); i < n; i++ {
		if b := s.seq[rand.Uint64N(n)]; usable(b, now, tried) {
			return b
		}
	}
	for _, b := range s.seq {
		if usable(b, now, tried) {
			return b
		}
	}
	return nil
}

// leastConn picks the usable backend with the fewest in-flight requests.
// Ties are broken by rotating the scan start. Weights are ignored.
type leastConn struct {
	bs   []*Backend
	next atomic.Uint64
}

// NewLeastConnections returns a least-connections balancer.
func NewLeastConnections(bs []*Backend) Balancer { return &leastConn{bs: bs} }

func (l *leastConn) Pick(_ *http.Request, tried []*Backend, now int64) *Backend {
	n := uint64(len(l.bs))
	if n == 0 {
		return nil
	}
	start := l.next.Add(1)
	var best *Backend
	for i := uint64(0); i < n; i++ {
		b := l.bs[(start+i)%n]
		if usable(b, now, tried) && (best == nil || b.InFlight() < best.InFlight()) {
			best = b
		}
	}
	return best
}

// p2c samples two random backends and keeps the less loaded one. It is O(1),
// nearly as good as least-connections, and avoids the herd effect where every
// proxy sends to the same "least loaded" backend at once. Weights are ignored.
type p2c struct {
	bs       []*Backend
	fallback leastConn
}

// NewPowerOfTwoChoices returns a P2C balancer.
func NewPowerOfTwoChoices(bs []*Backend) Balancer { return &p2c{bs: bs, fallback: leastConn{bs: bs}} }

func (p *p2c) Pick(r *http.Request, tried []*Backend, now int64) *Backend {
	n := len(p.bs)
	if n < 2 {
		return p.fallback.Pick(r, tried, now)
	}
	i := rand.IntN(n)
	j := rand.IntN(n - 1)
	if j >= i {
		j++ // two distinct indexes
	}
	a, b := p.bs[i], p.bs[j]
	au, bu := usable(a, now, tried), usable(b, now, tried)
	switch {
	case au && bu:
		if b.InFlight() < a.InFlight() {
			return b
		}
		return a
	case au:
		return a
	case bu:
		return b
	}
	return p.fallback.Pick(r, tried, now) // both samples unusable: scan
}
