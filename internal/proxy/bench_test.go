package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/metrics"
)

// staticTransport answers every request in memory, so the benchmark measures
// only the proxy core: routing, picking, header work, copying.
type staticTransport struct{}

func (staticTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, ContentLength: 2,
		Header: http.Header{"Content-Type": {"text/plain"}, "Content-Length": {"2"}},
		Body:   io.NopCloser(strings.NewReader("ok")),
	}, nil
}

// BenchmarkHandler measures the proxy core per request (in-memory transport,
// so only our code and net/http's server-side types are counted). "metrics"
// adds Prometheus, and "metrics+retries" is the default config, where GETs are
// retry-eligible.
func BenchmarkHandler(b *testing.B) {
	for _, v := range []struct {
		name             string
		metrics, retries bool
	}{{"bare", false, false}, {"metrics", true, false}, {"metrics+retries", true, true}} {
		withMetrics := v.metrics
		b.Run(v.name, func(b *testing.B) {
			bs := []*lb.Backend{lb.NewBackend("10.0.0.1:80", 1), lb.NewBackend("10.0.0.2:80", 1)}
			pool := &lb.Pool{Name: "p", Scheme: "http", Backends: bs, Balancer: lb.NewRoundRobin(bs)}
			if v.retries { // the config default: 2 retries, 20% budget
				pool.Retries, pool.Budget = 2, lb.NewRetryBudget(0.2, 10)
			}
			rt, err := NewRouter([]Route{{Name: "r", Pool: pool}})
			if err != nil {
				b.Fatal(err)
			}
			o := Options{Router: rt, Transport: staticTransport{}, Limits: DefaultLimits}
			if withMetrics {
				o.Metrics = metrics.New(prometheus.NewRegistry())
			}
			h, err := New(o)
			if err != nil {
				b.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://app.example.com/api/users?id=7", nil)
			req.Header.Set("User-Agent", "bench")
			req.Header.Set("Accept", "*/*")
			w := &discardWriter{h: http.Header{}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				clear(w.h)
				h.ServeHTTP(w, req)
			}
		})
	}
}

// discardWriter is a reusable ResponseWriter, so the benchmark does not count
// httptest.ResponseRecorder's own allocations (as the 43 allocs/op baseline
// did). Like net/http's real writer it supports deadlines; without them every
// SetWriteDeadline would allocate an ErrNotSupported error.
type discardWriter struct{ h http.Header }

func (w *discardWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *discardWriter) SetReadDeadline(time.Time) error  { return nil }

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(int)             {}
func (w *discardWriter) Flush()                      {}
