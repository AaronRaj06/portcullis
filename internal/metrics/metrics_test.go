package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AaronRaj06/portcullis/internal/lb"
)

func TestMethodNormalisation(t *testing.T) {
	for in, want := range map[string]string{"GET": "GET", "PATCH": "PATCH", "get": "OTHER", "XYZZY": "OTHER", "": "OTHER"} {
		if got := Method(in); got != want {
			t.Errorf("Method(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusClass(t *testing.T) {
	for code, want := range map[int]string{101: "1xx", 200: "2xx", 304: "3xx", 404: "4xx", 502: "5xx"} {
		if got := StatusClass(code); got != want {
			t.Errorf("StatusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestBackendHealthCollector(t *testing.T) {
	up, down := lb.NewBackend("10.0.0.1:80", 1), lb.NewBackend("10.0.0.2:80", 1)
	down.SetHealthy(false)
	pool := &lb.Pool{Name: "web", Backends: []*lb.Backend{up, down}}
	reg := prometheus.NewRegistry()
	RegisterBackendHealth(reg, func() []*lb.Pool { return []*lb.Pool{pool} })
	want := `
# HELP portcullis_backend_up 1 if the backend may receive traffic (healthy and not ejected).
# TYPE portcullis_backend_up gauge
portcullis_backend_up{backend="10.0.0.1:80",pool="web"} 1
portcullis_backend_up{backend="10.0.0.2:80",pool="web"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "portcullis_backend_up"); err != nil {
		t.Fatal(err)
	}
}

func TestDoneAndUnmatchedRoute(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	m.Start()
	m.Done("", "BREW", 404, "no_route", time.Millisecond)
	if got := testutil.ToFloat64(m.requests.WithLabelValues("none", "OTHER", "404")); got != 1 {
		t.Fatalf("requests{none,OTHER,404} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.inFlight); got != 0 {
		t.Fatalf("in flight = %v after Done, want 0", got)
	}
}
