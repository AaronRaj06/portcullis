// Command portcullis is the data plane: an L7 reverse proxy.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AaronRaj06/portcullis/internal"
	"github.com/AaronRaj06/portcullis/internal/metrics"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "portcullis:", err)
		os.Exit(1)
	}
}

// run is main without the process exit, so tests can drive it.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("portcullis", flag.ContinueOnError)
	l := proxy.DefaultLimits
	var (
		listen      = fs.String("listen", ":8080", "proxy listen address")
		adminListen = fs.String("admin-listen", "127.0.0.1:9901", "admin listen address (/metrics, /healthz)")
		upstream    = fs.String("upstream", "", "upstream base URL, e.g. http://127.0.0.1:9000 (required)")
		trusted     = fs.String("trusted-proxies", "", "comma-separated CIDRs whose X-Forwarded-* headers are trusted")
		certFile    = fs.String("tls-cert", "", "TLS certificate file (enables HTTPS with -tls-key)")
		keyFile     = fs.String("tls-key", "", "TLS private key file")
		drain       = fs.Duration("drain-timeout", 30*time.Second, "how long shutdown waits for in-flight requests")
		logLevel    = fs.String("log-level", "info", "debug, info, warn or error")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	fs.DurationVar(&l.ReadHeaderTimeout, "read-header-timeout", l.ReadHeaderTimeout, "max time to read request headers")
	fs.DurationVar(&l.IdleTimeout, "idle-timeout", l.IdleTimeout, "max keep-alive idle time")
	fs.DurationVar(&l.WriteTimeout, "write-timeout", l.WriteTimeout, "max time one write to the client may block")
	fs.DurationVar(&l.BodyReadTimeout, "body-read-timeout", l.BodyReadTimeout, "max gap between reads of the request body")
	fs.DurationVar(&l.DialTimeout, "dial-timeout", l.DialTimeout, "upstream TCP connect timeout")
	fs.DurationVar(&l.ResponseHeaderTimeout, "response-header-timeout", l.ResponseHeaderTimeout, "max wait for upstream response headers")
	fs.IntVar(&l.MaxHeaderBytes, "max-header-bytes", l.MaxHeaderBytes, "max request header size")
	fs.Int64Var(&l.MaxBodyBytes, "max-body-bytes", l.MaxBodyBytes, "max request body size (0 = unlimited)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		_, err := fmt.Fprintln(stdout, "portcullis", internal.Version)
		return err
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: lvl}))

	if *upstream == "" {
		return errors.New("-upstream is required")
	}
	up, err := url.Parse(*upstream)
	if err != nil {
		return fmt.Errorf("-upstream: %w", err)
	}
	prefixes, err := parsePrefixes(*trusted)
	if err != nil {
		return err
	}
	var tlsCfg *tls.Config
	if *certFile != "" || *keyFile != "" {
		if tlsCfg, err = proxy.TLSConfig(*certFile, *keyFile); err != nil {
			return err
		}
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	h, err := proxy.New(proxy.Options{
		Upstream: up, Transport: proxy.NewTransport(l), TrustedProxies: prefixes,
		Limits: l, Logger: logger, Metrics: metrics.New(reg),
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	admin := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	pl, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	al, err := net.Listen("tcp", *adminListen)
	if err != nil {
		_ = pl.Close()
		return err
	}
	logger.Info("listening", "proxy", pl.Addr().String(), "admin", al.Addr().String(),
		"upstream", up.String(), "tls", tlsCfg != nil, "version", internal.Version)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- proxy.Serve(ctx, proxy.NewServer(h, l, tlsCfg), pl, *drain) }()
	go func() { errc <- proxy.Serve(ctx, admin, al, *drain) }()
	err = <-errc
	cancel() // one server stopping (error or signal) stops the other
	if err2 := <-errc; err == nil {
		err = err2
	}
	logger.Info("stopped", "error", fmt.Sprint(err))
	return err
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("-trusted-proxies: %w", err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
