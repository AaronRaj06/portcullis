// Package metrics defines the Prometheus metrics exported by the data plane.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AaronRaj06/portcullis/internal/lb"
)

// Metrics holds the data-plane collectors. Label values come only from config
// (route, pool, backend names) or fixed sets (method, code, kind), so the number
// of time series is bounded.
type Metrics struct {
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	errors    *prometheus.CounterVec
	attempts  *prometheus.CounterVec
	retries   *prometheus.CounterVec
	ejections *prometheus.CounterVec
	panics    *prometheus.CounterVec
	reloads   *prometheus.CounterVec
	gen       prometheus.Gauge
	lastOK    prometheus.Gauge
	inFlight  prometheus.Gauge
}

// New creates the collectors and registers them on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_requests_total", Help: "Requests handled, by route, method and status code.",
		}, []string{"route", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "portcullis_request_duration_seconds", Help: "Time from request start to end of response body.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_proxy_errors_total", Help: "Requests that failed in the proxy, by kind.",
		}, []string{"kind"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_upstream_attempts_total", Help: "Attempts sent to a backend, by result (status class or error kind).",
		}, []string{"pool", "backend", "result"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_retries_total", Help: "Retries performed, by pool.",
		}, []string{"pool"}),
		ejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_backend_ejections_total", Help: "Passive ejections after consecutive gateway failures.",
		}, []string{"pool", "backend"}),
		panics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_panic_picks_total", Help: "Picks made in panic mode because no backend was available.",
		}, []string{"pool"}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_config_reloads_total", Help: "Config apply attempts, by result (success, unchanged, error).",
		}, []string{"result"}),
		gen: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "portcullis_config_generation", Help: "Generation of the config serving traffic; increments on every successful apply.",
		}),
		lastOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "portcullis_config_last_success_timestamp_seconds", Help: "Unix time of the last successful config apply.",
		}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "portcullis_requests_in_flight", Help: "Requests currently being served.",
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.errors, m.attempts, m.retries, m.ejections, m.panics, m.reloads, m.gen, m.lastOK, m.inFlight)
	return m
}

// Start marks a request as in flight.
func (m *Metrics) Start() { m.inFlight.Inc() }

// Done records a finished request. route is "" when nothing matched; errKind is "" on success.
func (m *Metrics) Done(route, method string, code int, errKind string, d time.Duration) {
	m.inFlight.Dec()
	if route == "" {
		route = "none"
	}
	m.requests.WithLabelValues(route, Method(method), strconv.Itoa(code)).Inc()
	m.duration.WithLabelValues(route).Observe(d.Seconds())
	if errKind != "" {
		m.errors.WithLabelValues(errKind).Inc()
	}
}

// Attempt records one try against a backend. result is a status class ("2xx") or an error kind.
func (m *Metrics) Attempt(pool, backend, result string) {
	m.attempts.WithLabelValues(pool, backend, result).Inc()
}

// Retry records a retry in pool.
func (m *Metrics) Retry(pool string) { m.retries.WithLabelValues(pool).Inc() }

// Ejected records a passive ejection.
func (m *Metrics) Ejected(pool, backend string) { m.ejections.WithLabelValues(pool, backend).Inc() }

// ConfigReload records one config apply attempt.
func (m *Metrics) ConfigReload(result string) {
	m.reloads.WithLabelValues(result).Inc()
	if result == "success" {
		m.lastOK.SetToCurrentTime()
	}
}

// ConfigGeneration records the generation now serving.
func (m *Metrics) ConfigGeneration(g uint64) { m.gen.Set(float64(g)) }

// Panic records a panic-mode pick.
func (m *Metrics) Panic(pool string) { m.panics.WithLabelValues(pool).Inc() }

// StatusClass maps 200 → "2xx" without allocating.
func StatusClass(code int) string {
	switch code / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	}
	return "5xx"
}

// Method maps any non-standard method to "OTHER" so a client cannot create
// unbounded label values (each distinct label set is a new time series in memory).
func Method(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m
	}
	return "OTHER"
}

// RegisterBackendHealth exports portcullis_backend_up{pool,backend}: 1 if the
// backend is healthy and not ejected. It is read from live state at scrape
// time, so it is never stale (ejections expire without any event), and it
// follows config reloads because pools() is called on every scrape.
func RegisterBackendHealth(reg prometheus.Registerer, pools func() []*lb.Pool) {
	reg.MustRegister(&backendCollector{
		desc:  prometheus.NewDesc("portcullis_backend_up", "1 if the backend may receive traffic (healthy and not ejected).", []string{"pool", "backend"}, nil),
		pools: pools,
	})
}

type backendCollector struct {
	desc  *prometheus.Desc
	pools func() []*lb.Pool
}

func (c *backendCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *backendCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now().UnixNano()
	for _, p := range c.pools() {
		for _, b := range p.Backends {
			v := 0.0
			if b.Available(now) {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, p.Name, b.Addr)
		}
	}
}
