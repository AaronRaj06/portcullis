// Command rproxy is the benchmark baseline: the standard library's
// httputil.ReverseProxy, using the same Transport settings as Portcullis so
// the comparison measures the proxy logic, not connection-pool tuning.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/AaronRaj06/portcullis/internal/proxy"
)

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	upstream := flag.String("upstream", "http://backend:9000", "upstream URL")
	flag.Parse()
	u, err := url.Parse(*upstream)
	if err != nil {
		log.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = proxy.NewTransport(proxy.DefaultLimits)
	srv := &http.Server{Addr: *listen, Handler: rp, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
