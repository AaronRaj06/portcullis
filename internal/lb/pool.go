package lb

import (
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrNoBackend means every backend is down, ejected or already tried.
var ErrNoBackend = errors.New("lb: no available backend")

// Pool is a named set of backends with one balancing algorithm and its
// failure-handling policy.
type Pool struct {
	Name     string
	Scheme   string // "http" or "https"
	Backends []*Backend
	Balancer Balancer

	// Passive ejection: after EjectAfter consecutive gateway failures, a backend
	// gets no traffic for EjectFor, then is re-admitted. EjectAfter 0 disables it.
	EjectAfter int
	EjectFor   time.Duration

	// Retries is the number of extra attempts for retryable failures, limited by Budget.
	Retries int
	Budget  *RetryBudget
}

// Pick chooses a backend, skipping tried.
func (p *Pool) Pick(r *http.Request, tried []*Backend) (*Backend, error) {
	if b := p.Balancer.Pick(r, tried, time.Now().UnixNano()); b != nil {
		return b, nil
	}
	return nil, ErrNoBackend
}

// Report records an attempt's outcome. failed means a gateway failure (connect
// error, timeout, or a 502/503/504 from the backend). It reports whether this
// call ejected the backend.
func (p *Pool) Report(b *Backend, failed bool) (ejected bool) {
	if p.EjectAfter <= 0 {
		return false
	}
	if !failed {
		if b.fails.Load() != 0 {
			b.fails.Store(0)
		}
		return false
	}
	if int(b.fails.Add(1)) < p.EjectAfter {
		return false
	}
	b.fails.Store(0)
	b.ejectedUntil.Store(time.Now().Add(p.EjectFor).UnixNano())
	return true
}

// RetryBudget caps retries at a fraction of traffic, so retries cannot turn a
// partial outage into a full one. Each request deposits `ratio` of a token;
// each retry spends one whole token. The balance is capped at `burst` tokens.
// It is lock-free: one CAS per request.
type RetryBudget struct {
	ratio   int64 // milli-tokens deposited per request
	cap     int64 // milli-tokens
	balance atomic.Int64
}

// NewRetryBudget allows retries of up to ratio (for example 0.2 = 20%) of requests,
// with burst tokens available up front. The balance can always hold at least one
// token; otherwise burst 0 would forbid retries entirely.
func NewRetryBudget(ratio float64, burst int) *RetryBudget {
	b := &RetryBudget{ratio: int64(ratio * 1000), cap: int64(max(burst, 1)) * 1000}
	b.balance.Store(int64(max(burst, 0)) * 1000)
	return b
}

// Deposit is called once per request.
func (b *RetryBudget) Deposit() {
	for {
		cur := b.balance.Load()
		next := min(cur+b.ratio, b.cap)
		if next == cur || b.balance.CompareAndSwap(cur, next) {
			return
		}
	}
}

// Withdraw takes one token for a retry, or reports false if the budget is spent.
func (b *RetryBudget) Withdraw() bool {
	for {
		cur := b.balance.Load()
		if cur < 1000 {
			return false
		}
		if b.balance.CompareAndSwap(cur, cur-1000) {
			return true
		}
	}
}
