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
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/AaronRaj06/portcullis/internal"
	"github.com/AaronRaj06/portcullis/internal/admin"
	"github.com/AaronRaj06/portcullis/internal/config"
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
//
// Traffic config (routes, pools) comes from -config (YAML/JSON, hot-reloadable
// via SIGHUP, POST /reload or PUT /config) or, for quick use, from -upstream and
// friends, which build an equivalent one-route config. Everything else here is
// process-level and needs a restart.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("portcullis", flag.ContinueOnError)
	l := proxy.DefaultLimits
	var (
		configFile = fs.String("config", "", "traffic config file (YAML or JSON); reloaded on SIGHUP")
		// -tls-cert/-tls-key are also re-read on SIGHUP (certificate rotation).
		listen      = fs.String("listen", ":8080", "proxy listen address")
		adminListen = fs.String("admin-listen", "127.0.0.1:9901", "admin listen address")
		tokenFile   = fs.String("admin-token-file", "", "file holding the admin bearer token (default: no auth; keep the admin port on localhost)")
		pprofOn     = fs.Bool("pprof", false, "serve /debug/pprof/ on the admin port")
		certFile    = fs.String("tls-cert", "", "TLS certificate file (enables HTTPS with -tls-key)")
		keyFile     = fs.String("tls-key", "", "TLS private key file")
		drain       = fs.Duration("drain-timeout", 30*time.Second, "how long shutdown waits for in-flight requests")
		logLevel    = fs.String("log-level", "info", "debug, info, warn or error")
		showVersion = fs.Bool("version", false, "print version and exit")

		// Shorthand for a one-route config when -config is not given.
		upstream   = fs.String("upstream", "", "comma-separated backends, same scheme (http, https or h2c), optional =weight: http://10.0.0.1:80=3,http://10.0.0.2:80")
		upstreamCA = fs.String("upstream-ca", "", "PEM file of CAs trusted for https upstreams (default: system roots)")
		algo       = fs.String("lb", "round_robin", "round_robin, weighted_round_robin, least_conn, p2c or hash")
		hashKey    = fs.String("hash-key", "", "for -lb hash: ip, path, header:NAME or cookie:NAME (default ip)")
		healthPath = fs.String("health-path", "/healthz", "active health check path (empty = off)")
		healthIntv = fs.Duration("health-interval", 5*time.Second, "time between health checks")
		ejectAfter = fs.Int("eject-after", 5, "consecutive gateway failures before a backend is ejected (0 = off)")
		ejectFor   = fs.Duration("eject-for", 30*time.Second, "base ejection time, multiplied by consecutive ejections")
		maxEject   = fs.Duration("max-eject-for", 5*time.Minute, "cap on the ejection time (0 = none)")
		panicMode  = fs.Bool("panic", true, "if no backend is available, send to all anyway instead of returning 503 (ADR 0009)")
		retries    = fs.Int("retries", 2, "extra attempts for idempotent requests that never reached a backend")
		budget     = fs.Float64("retry-budget", 0.2, "retries allowed as a fraction of requests")
		trusted    = fs.String("trusted-proxies", "", "comma-separated CIDRs whose X-Forwarded-* headers are trusted")
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

	if (*configFile == "") == (*upstream == "") {
		return errors.New("give exactly one of -config or -upstream")
	}
	var tlsCfg *tls.Config
	var certs *proxy.CertReloader
	if *certFile != "" || *keyFile != "" {
		var err error
		if certs, err = proxy.NewCertReloader(*certFile, *keyFile); err != nil {
			return err
		}
		tlsCfg = proxy.TLSConfigFrom(certs)
	}
	token := ""
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("-admin-token-file: %w", err)
		}
		if token, err = admin.TokenFromFile(data); err != nil {
			return err
		}
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg)
	h, err := proxy.New(proxy.Options{Transport: proxy.NewTransport(l), Limits: l, Logger: logger, Metrics: m})
	if err != nil {
		return err
	}
	metrics.RegisterBackendHealth(reg, func() []*lb.Pool {
		if s := h.Current(); s != nil {
			return s.Router.Pools()
		}
		return nil
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mgr := config.NewManager(ctx, h, l, *configFile, logger, m)
	if *configFile != "" {
		if _, err := mgr.Reload(); err != nil {
			return fmt.Errorf("-config: %w", err)
		}
	} else {
		c, err := flagsConfig(*upstream, *upstreamCA, *algo, *hashKey, *healthPath, *healthIntv,
			*ejectAfter, *ejectFor, *maxEject, *panicMode, *retries, *budget, *trusted)
		if err != nil {
			return err
		}
		if _, err := mgr.ApplyConfig(c, "flags"); err != nil {
			return err
		}
	}

	adm := &admin.Server{Manager: mgr, Gatherer: reg, Token: token, Pprof: *pprofOn}
	adminSrv := &http.Server{Handler: adm.Handler(), ReadHeaderTimeout: 5 * time.Second}

	pl, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	al, err := net.Listen("tcp", *adminListen)
	if err != nil {
		_ = pl.Close()
		return err
	}
	st := mgr.Status()
	logger.Info("listening", "proxy", pl.Addr().String(), "admin", al.Addr().String(), "tls", tlsCfg != nil,
		"config", st.Source, "config_hash", st.Hash, "admin_auth", token != "", "version", internal.Version)

	// SIGHUP re-reads the TLS certificate and the config file, whichever are in
	// use. A bad file is logged and ignored: the current one keeps serving.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if certs != nil {
					if err := certs.Reload(); err != nil {
						logger.Warn("SIGHUP certificate reload failed; previous certificate keeps serving", "error", err.Error())
					} else {
						logger.Info("certificate reloaded", "cert", *certFile)
					}
				}
				if *configFile != "" {
					if _, err := mgr.Reload(); err != nil {
						logger.Warn("SIGHUP reload failed; previous config keeps serving", "error", err.Error())
					}
				}
			}
		}
	}()

	// The admin server outlives the proxy drain, so /readyz can report
	// "draining" while in-flight requests finish.
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	adminCtx, stopAdmin := context.WithCancel(context.Background())
	defer stopProxy()
	defer stopAdmin()
	proxyErr, adminErr := make(chan error, 1), make(chan error, 1)
	go func() { proxyErr <- proxy.Serve(proxyCtx, proxy.NewServer(h, l, tlsCfg), pl, *drain) }()
	go func() { adminErr <- proxy.Serve(adminCtx, adminSrv, al, *drain) }()

	var errs []error
	select {
	case <-ctx.Done(): // signal: fail readiness first, then drain the proxy
		adm.SetDraining()
		stopProxy()
		errs = append(errs, <-proxyErr)
	case err := <-proxyErr: // the proxy server died
		errs = append(errs, err)
	case err := <-adminErr: // the admin server died: stop the proxy too
		stopProxy()
		errs = append(errs, err, <-proxyErr)
		adminErr = nil
	}
	cancel() // stops health checks and the SIGHUP loop
	if adminErr != nil {
		stopAdmin()
		errs = append(errs, <-adminErr)
	}
	err = errors.Join(errs...)
	logger.Info("stopped", "error", fmt.Sprint(err))
	return err
}

// flagsConfig turns the shorthand flags into a one-pool, one-route config,
// so flags and files share one validated path.
func flagsConfig(upstreams, ca, algo, hashKey, healthPath string, healthIntv time.Duration,
	ejectAfter int, ejectFor, maxEject time.Duration, panicMode bool, retries int, budget float64, trusted string) (*config.Config, error) {
	p := config.Pool{
		Name: "default", LB: algo, HashKey: hashKey, Panic: &panicMode,
		Ejection: &config.Ejection{After: ejectAfter, For: config.Duration(ejectFor), Max: config.Duration(maxEject)},
		Retries:  &config.Retries{Attempts: retries, Budget: budget, Burst: 10},
	}
	if ca != "" {
		p.TLS = &config.UpstreamTLS{CAFile: ca}
	}
	if healthPath != "" {
		p.HealthCheck = &config.HealthCheck{Path: healthPath, Interval: config.Duration(healthIntv)}
	}
	for _, s := range strings.Split(upstreams, ",") {
		s = strings.TrimSpace(s)
		weight := 1
		if i := strings.LastIndexByte(s, '='); i >= 0 {
			w, err := strconv.Atoi(s[i+1:])
			if err != nil || w < 1 {
				return nil, fmt.Errorf("-upstream %q: weight must be a number from 1 to 1000", s)
			}
			s, weight = s[:i], w
		}
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("-upstream: %w", err)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("-upstream %q: a path is not supported", s)
		}
		if p.Scheme != "" && p.Scheme != u.Scheme {
			return nil, errors.New("-upstream: all backends must use the same scheme")
		}
		p.Scheme = u.Scheme
		p.Backends = append(p.Backends, config.Backend{Address: u.Host, Weight: weight})
	}
	c := &config.Config{Pools: []config.Pool{p}, Routes: []config.Route{{Name: "default", Backends: []config.PoolRef{{Pool: "default"}}}}}
	for _, f := range strings.Split(trusted, ",") {
		if f = strings.TrimSpace(f); f != "" {
			c.TrustedProxies = append(c.TrustedProxies, f)
		}
	}
	return c, nil
}
