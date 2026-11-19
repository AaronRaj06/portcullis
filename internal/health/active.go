// Package health runs active HTTP health checks against backends.
// (Passive ejection lives in lb.Pool.Report, since it reacts to live traffic.)
package health

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/AaronRaj06/portcullis/internal/lb"
)

// Config controls active checks. A backend flips to unhealthy after Unhealthy
// consecutive failed checks, and back after Healthy consecutive passes. The
// thresholds stop a single dropped packet from flapping it.
type Config struct {
	Path      string        // e.g. "/healthz"
	Interval  time.Duration // time between checks
	Timeout   time.Duration // per check
	Healthy   int           // consecutive passes to mark up
	Unhealthy int           // consecutive failures to mark down
	Jitter    float64       // 0..1: each interval varies by ±Jitter so checks from many proxies don't line up
}

// DefaultConfig is a reasonable starting point.
var DefaultConfig = Config{Path: "/healthz", Interval: 5 * time.Second, Timeout: 2 * time.Second, Healthy: 2, Unhealthy: 3, Jitter: 0.1}

// OnChange is called when a backend's verdict flips.
type OnChange func(pool string, b *lb.Backend, healthy bool)

// Run checks every backend of pool until ctx is cancelled: one goroutine per
// backend. Backends start healthy (optimistic), so a fresh proxy serves traffic
// at once instead of returning 503 until the first round of checks completes.
func Run(ctx context.Context, client *http.Client, pool *lb.Pool, cfg Config, onChange OnChange, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for _, b := range pool.Backends {
		go checkLoop(ctx, client, pool, b, cfg, onChange, log)
	}
}

func checkLoop(ctx context.Context, client *http.Client, pool *lb.Pool, b *lb.Backend, cfg Config, onChange OnChange, log *slog.Logger) {
	url := pool.Scheme + "://" + b.Addr + cfg.Path
	passes, fails := 0, 0
	// A random first delay spreads the checks of many backends over the interval.
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(cfg.Interval) + 1)))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if probe(ctx, client, url, cfg.Timeout) {
			passes, fails = passes+1, 0
		} else {
			passes, fails = 0, fails+1
		}
		var verdict, decided bool
		switch {
		case passes >= cfg.Healthy:
			verdict, decided = true, true
		case fails >= cfg.Unhealthy:
			verdict, decided = false, true
		}
		if decided && b.SetHealthy(verdict) {
			log.Info("backend health changed", "pool", pool.Name, "backend", b.Addr, "healthy", verdict)
			if onChange != nil {
				onChange(pool.Name, b, verdict)
			}
		}
		timer.Reset(jittered(cfg.Interval, cfg.Jitter))
	}
}

func jittered(d time.Duration, j float64) time.Duration {
	if j <= 0 {
		return d
	}
	return time.Duration(float64(d) * (1 + j*(2*rand.Float64()-1)))
}

// probe passes on any 2xx or 3xx response within timeout.
func probe(ctx context.Context, client *http.Client, url string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "portcullis-health")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain so the connection is reused
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}
