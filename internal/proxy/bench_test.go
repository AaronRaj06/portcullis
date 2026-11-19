package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AaronRaj06/portcullis/internal/lb"
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

// BenchmarkHandler is the Phase 4 baseline for allocations per request.
func BenchmarkHandler(b *testing.B) {
	bs := []*lb.Backend{lb.NewBackend("10.0.0.1:80", 1), lb.NewBackend("10.0.0.2:80", 1)}
	pool := &lb.Pool{Name: "p", Scheme: "http", Backends: bs, Balancer: lb.NewRoundRobin(bs)}
	rt, err := NewRouter([]Route{{Name: "r", Pool: pool}})
	if err != nil {
		b.Fatal(err)
	}
	h, err := New(Options{Router: rt, Transport: staticTransport{}, Limits: DefaultLimits})
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/api/users?id=7", nil)
	req.Header.Set("User-Agent", "bench")
	req.Header.Set("Accept", "*/*")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}
