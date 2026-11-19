// Package lb holds upstream pools and the hand-written load-balancing algorithms:
// round-robin, smooth weighted round-robin, least-connections, power-of-two-choices
// and consistent hashing with virtual nodes.
//
// All per-backend state is atomic, so picking never takes a lock.
package lb

import (
	"sync/atomic"
)

// Backend is one upstream address and its live state.
type Backend struct {
	Addr   string // host:port
	Weight int    // >= 1; used by weighted round-robin and consistent hashing

	inflight     atomic.Int64
	down         atomic.Bool  // set by active health checks; zero value means up
	ejectedUntil atomic.Int64 // unix nanoseconds; set by passive ejection
	fails        atomic.Int32 // consecutive gateway failures
}

// NewBackend returns a healthy backend. Weights below 1 are treated as 1.
func NewBackend(addr string, weight int) *Backend {
	return &Backend{Addr: addr, Weight: max(weight, 1)}
}

// Available reports whether the backend may receive traffic at time now (unix ns).
func (b *Backend) Available(now int64) bool {
	return !b.down.Load() && now >= b.ejectedUntil.Load()
}

// SetHealthy records an active health-check verdict and reports whether it changed.
func (b *Backend) SetHealthy(ok bool) (changed bool) { return b.down.Swap(!ok) == ok }

// Healthy is the active health-check verdict (ignores passive ejection).
func (b *Backend) Healthy() bool { return !b.down.Load() }

// Ejected reports whether passive ejection currently excludes the backend.
func (b *Backend) Ejected(now int64) bool { return now < b.ejectedUntil.Load() }

// InFlight is the number of requests currently sent to this backend.
func (b *Backend) InFlight() int64 { return b.inflight.Load() }

// Acquire and Release bracket a request to this backend (least-conn and P2C read the count).
func (b *Backend) Acquire() { b.inflight.Add(1) }

// Release ends a request started with Acquire.
func (b *Backend) Release() { b.inflight.Add(-1) }

// usable is the common filter for every algorithm.
func usable(b *Backend, now int64, tried []*Backend) bool {
	if !b.Available(now) {
		return false
	}
	for _, t := range tried {
		if t == b {
			return false
		}
	}
	return true
}
