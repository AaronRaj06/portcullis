// Command backend is the benchmark upstream: fixed-size responses, fast.
//
//	GET /small    128-byte body
//	GET /large    256 KiB body
//	GET /healthz  200
//
// Every response carries X-Backend: <name>. For failure injection, SIGUSR1
// closes the listener and all connections (clients then get connection
// refused); SIGUSR2 starts listening again.
package main

import (
	"bytes"
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("listen", ":9000", "listen address")
	name := flag.String("name", "backend", "value of the X-Backend header")
	flag.Parse()

	small := bytes.Repeat([]byte("s"), 128)
	large := bytes.Repeat([]byte("L"), 256<<10)
	body := func(b []byte) http.HandlerFunc {
		n := strconv.Itoa(len(b))
		return func(w http.ResponseWriter, _ *http.Request) {
			h := w.Header()
			h["Content-Type"] = []string{"application/octet-stream"}
			h["Content-Length"] = []string{n}
			h["X-Backend"] = []string{*name}
			_, _ = w.Write(b)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /small", body(small))
	mux.Handle("GET /large", body(large))
	mux.Handle("GET /healthz", body([]byte("ok")))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGTERM, syscall.SIGINT)
	var srv *http.Server
	start := func() {
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			log.Fatal(err)
		}
		srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		log.Printf("%s listening on %s", *name, *addr)
	}
	start()
	for s := range sig {
		switch s {
		case syscall.SIGUSR1:
			if srv != nil {
				_ = srv.Close() // listener and every connection: like a crashed process
				srv = nil
				log.Printf("%s stopped (SIGUSR1)", *name)
			}
		case syscall.SIGUSR2:
			if srv == nil {
				start()
			}
		default:
			if srv != nil {
				_ = srv.Shutdown(context.Background())
			}
			return
		}
	}
}
