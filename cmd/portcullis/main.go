// Command portcullis is the data plane: an L7 reverse proxy and load balancer.
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
	"github.com/AaronRaj06/portcullis/internal/health"
	"github.com/AaronRaj06/portcullis/internal/lb"
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
// Flags configure one catch-all route to one pool; multi-route config files arrive in Phase 3.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("portcullis", flag.ContinueOnError)
	l := proxy.DefaultLimits
	hc := health.DefaultConfig
	var (
		listen      = fs.String("listen", ":8080", "proxy listen address")
		adminListen = fs.String("admin-listen", "127.0.0.1:9901", "admin listen address (/metrics, /healthz)")
		upstream    = fs.String("upstream", "", "comma-separated backend URLs, same scheme, e.g. http://10.0.0.1:80,http://10.0.0.2:80 (required)")
		algo        = fs.String("lb", "round_robin", "round_robin, least_conn, p2c or hash")
		hashKey     = fs.String("hash-key", "ip", "for -lb hash: ip, path, header:NAME or cookie:NAME")
		ejectAfter  = fs.Int("eject-after", 5, "consecutive gateway failures before a backend is ejected (0 = off)")
		ejectFor    = fs.Duration("eject-for", 30*time.Second, "how long an ejected backend gets no traffic")
		retries     = fs.Int("retries", 2, "extra attempts for idempotent requests that never reached a backend")
		budget      = fs.Float64("retry-budget", 0.2, "retries allowed as a fraction of requests")
		trusted     = fs.String("trusted-proxies", "", "comma-separated CIDRs whose X-Forwarded-* headers are trusted")
		certFile    = fs.String("tls-cert", "", "TLS certificate file (enables HTTPS with -tls-key)")
		keyFile     = fs.String("tls-key", "", "TLS private key file")
		drain       = fs.Duration("drain-timeout", 30*time.Second, "how long shutdown waits for in-flight requests")
		logLevel    = fs.String("log-level", "info", "debug, info, warn or error")
		showVersion = fs.Bool("version", false, "print version and exit")
	)
	fs.StringVar(&hc.Path, "health-path", hc.Path, "active health check path (empty = off)")
	fs.DurationVar(&hc.Interval, "health-interval", hc.Interval, "time between health checks")
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

	pool, err := buildPool(*upstream, *algo, *hashKey)
	if err != nil {
		return err
	}
	pool.EjectAfter, pool.EjectFor, pool.Retries = *ejectAfter, *ejectFor, *retries
	pool.Budget = lb.NewRetryBudget(*budget, 10)
	router, err := proxy.NewRouter([]proxy.Route{{Name: "default", Pool: pool}})
	if err != nil {
		return err
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
	metrics.RegisterBackendHealth(reg, router.Pools)
	h, err := proxy.New(proxy.Options{
		Router: router, Transport: proxy.NewTransport(l), TrustedProxies: prefixes,
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
		"backends", len(pool.Backends), "lb", *algo, "tls", tlsCfg != nil, "version", internal.Version)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if hc.Path != "" {
		checker := &http.Client{
			Transport:     proxy.NewTransport(l),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // a 3xx counts as healthy
		}
		health.Run(ctx, checker, pool, hc, nil, logger)
	}
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

func buildPool(upstreams, algo, hashKey string) (*lb.Pool, error) {
	if upstreams == "" {
		return nil, errors.New("-upstream is required")
	}
	pool := &lb.Pool{Name: "default"}
	for _, s := range strings.Split(upstreams, ",") {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("-upstream: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("-upstream %q: want scheme://host:port with no path", s)
		}
		if pool.Scheme != "" && pool.Scheme != u.Scheme {
			return nil, errors.New("-upstream: all backends must use the same scheme")
		}
		pool.Scheme = u.Scheme
		pool.Backends = append(pool.Backends, lb.NewBackend(u.Host, 1))
	}
	switch algo {
	case "round_robin":
		pool.Balancer = lb.NewRoundRobin(pool.Backends)
	case "least_conn":
		pool.Balancer = lb.NewLeastConnections(pool.Backends)
	case "p2c":
		pool.Balancer = lb.NewPowerOfTwoChoices(pool.Backends)
	case "hash":
		key, err := parseHashKey(hashKey)
		if err != nil {
			return nil, err
		}
		pool.Balancer = lb.NewConsistentHash(pool.Backends, key, lb.DefaultVnodes)
	default:
		return nil, fmt.Errorf("-lb %q: want round_robin, least_conn, p2c or hash", algo)
	}
	return pool, nil
}

func parseHashKey(s string) (lb.KeyFunc, error) {
	kind, name, _ := strings.Cut(s, ":")
	switch {
	case kind == "ip":
		return lb.ClientIPKey, nil
	case kind == "path":
		return lb.PathKey, nil
	case kind == "header" && name != "":
		return lb.HeaderKey(name), nil
	case kind == "cookie" && name != "":
		return lb.CookieKey(name), nil
	}
	return nil, fmt.Errorf("-hash-key %q: want ip, path, header:NAME or cookie:NAME", s)
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
