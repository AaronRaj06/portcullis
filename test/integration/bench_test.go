package integration

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// BenchmarkEndToEnd sends real HTTP over loopback: client → proxy (real
// server, real transport) → backend. allocs/op includes the client and the
// backend too, so it shows each optimisation at its true share of a request.
func BenchmarkEndToEnd(b *testing.B) {
	body := strings.Repeat("s", 128)
	be := backend(&testing.T{}, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
	defer be.Close()
	h, err := proxy.New(proxy.Options{
		Router:    mustRouterB(b, proxy.Route{Name: "r", Pool: newPool("p", "http", lb.NewRoundRobin, strings.TrimPrefix(be.URL, "http://"))}),
		Transport: proxy.NewTransport(proxy.DefaultLimits), Limits: proxy.DefaultLimits,
	})
	if err != nil {
		b.Fatal(err)
	}
	srv := http.Server{Handler: h}
	ln := listen(b)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	url := "http://" + ln.Addr().String() + "/small"
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(url)
			if err != nil {
				b.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

func mustRouterB(b *testing.B, routes ...proxy.Route) *proxy.Router {
	rt, err := proxy.NewRouter(routes)
	if err != nil {
		b.Fatal(err)
	}
	return rt
}

func listen(b *testing.B) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	return ln
}
