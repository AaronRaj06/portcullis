// Package metrics defines the Prometheus metrics exported by the data plane.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the data-plane collectors. Route and upstream labels arrive in Phase 2.
type Metrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	errors   *prometheus.CounterVec
	inFlight prometheus.Gauge
}

// New creates the collectors and registers them on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_requests_total", Help: "Requests handled, by method and status code.",
		}, []string{"method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "portcullis_request_duration_seconds", Help: "Time from request start to end of response body.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "portcullis_proxy_errors_total", Help: "Requests that failed in the proxy, by kind.",
		}, []string{"kind"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "portcullis_requests_in_flight", Help: "Requests currently being served.",
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.errors, m.inFlight)
	return m
}

// Start marks a request as in flight.
func (m *Metrics) Start() { m.inFlight.Inc() }

// Done records a finished request. errKind is "" on success.
func (m *Metrics) Done(method string, code int, errKind string, d time.Duration) {
	m.inFlight.Dec()
	method = Method(method)
	m.requests.WithLabelValues(method, strconv.Itoa(code)).Inc()
	m.duration.WithLabelValues(method).Observe(d.Seconds())
	if errKind != "" {
		m.errors.WithLabelValues(errKind).Inc()
	}
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
