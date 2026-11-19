package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/AaronRaj06/portcullis/internal/health"
	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// Compiled is a validated config turned into live objects, not yet published.
type Compiled struct {
	Snapshot   *proxy.Snapshot
	Health     []PoolHealth      // active checks to run while this snapshot is current
	Transports []*http.Transport // pool-specific transports, closed when superseded
}

// PoolHealth pairs a pool with its active health-check settings.
type PoolHealth struct {
	Pool   *lb.Pool
	Config health.Config
}

// Compile validates c and builds a snapshot. Backends that also existed in prev
// (same pool name and address) inherit their health and ejection state.
func Compile(c *Config, l proxy.Limits, prev *proxy.Snapshot) (*Compiled, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	old := map[string]*lb.Backend{}
	if prev != nil {
		for _, p := range prev.Router.Pools() {
			for _, b := range p.Backends {
				old[p.Name+"|"+b.Addr] = b
			}
		}
	}

	out := &Compiled{}
	pools := map[string]*lb.Pool{}
	for _, pc := range c.Pools {
		p := &lb.Pool{Name: pc.Name, Scheme: pc.Scheme, Panic: pc.Panic}
		for _, bc := range pc.Backends {
			b := lb.NewBackend(bc.Address, bc.Weight)
			if o := old[pc.Name+"|"+bc.Address]; o != nil {
				b.InheritState(o)
			}
			p.Backends = append(p.Backends, b)
		}
		var err error
		if p.Balancer, err = balancer(pc.LB, pc.HashKey, p.Backends); err != nil {
			return nil, fmt.Errorf("pool %q: %w", pc.Name, err)
		}
		p.EjectAfter, p.EjectFor, p.MaxEjectFor = pc.Ejection.After, time.Duration(pc.Ejection.For), time.Duration(pc.Ejection.Max)
		p.Retries = pc.Retries.Attempts
		p.Budget = lb.NewRetryBudget(pc.Retries.Budget, pc.Retries.Burst)
		if t, err := transport(pc, l); err != nil {
			return nil, fmt.Errorf("pool %q: %w", pc.Name, err)
		} else if t != nil {
			p.Transport = t
			out.Transports = append(out.Transports, t)
		}
		if pc.Scheme == "h2c" {
			p.Scheme = "http" // h2c is HTTP/2 over plain http URLs, via the pool's transport
		}
		if hc := pc.HealthCheck; hc != nil {
			out.Health = append(out.Health, PoolHealth{Pool: p, Config: health.Config{
				Path: hc.Path, Interval: time.Duration(hc.Interval), Timeout: time.Duration(hc.Timeout),
				Healthy: hc.Healthy, Unhealthy: hc.Unhealthy, Jitter: hc.Jitter,
			}})
		}
		pools[pc.Name] = p
	}

	routes := make([]proxy.Route, 0, len(c.Routes))
	for _, rc := range c.Routes {
		r := proxy.Route{Name: rc.Name, Hosts: rc.Hosts, Path: rc.Path, Exact: rc.PathType == "Exact", Methods: rc.Methods}
		for _, h := range rc.Headers {
			r.Headers = append(r.Headers, proxy.HeaderMatch{Name: h.Name, Value: h.Value})
		}
		if len(rc.Backends) == 1 {
			r.Pool = pools[rc.Backends[0].Pool]
		} else {
			for _, ref := range rc.Backends {
				r.Split = append(r.Split, proxy.WeightedPool{Pool: pools[ref.Pool], Weight: *ref.Weight})
			}
		}
		routes = append(routes, r)
	}
	router, err := proxy.NewRouter(routes)
	if err != nil {
		return nil, &ValidationError{Problems: []string{"routes: " + err.Error()}}
	}
	trusted := make([]netip.Prefix, 0, len(c.TrustedProxies))
	for _, s := range c.TrustedProxies {
		trusted = append(trusted, netip.MustParsePrefix(s).Masked()) // validated above
	}
	out.Snapshot = &proxy.Snapshot{Router: router, TrustedProxies: trusted, Version: c.Version, Hash: c.Hash()}
	return out, nil
}

func balancer(name, hashKey string, bs []*lb.Backend) (lb.Balancer, error) {
	switch name {
	case "round_robin":
		return lb.NewRoundRobin(bs), nil
	case "weighted_round_robin":
		return lb.NewWeightedRoundRobin(bs), nil
	case "least_conn":
		return lb.NewLeastConnections(bs), nil
	case "p2c":
		return lb.NewPowerOfTwoChoices(bs), nil
	case "hash":
		kind, arg, _ := strings.Cut(hashKey, ":")
		key := map[string]lb.KeyFunc{"ip": lb.ClientIPKey, "path": lb.PathKey}[kind]
		switch kind {
		case "header":
			key = lb.HeaderKey(arg)
		case "cookie":
			key = lb.CookieKey(arg)
		}
		return lb.NewConsistentHash(bs, key, lb.DefaultVnodes), nil
	}
	return nil, fmt.Errorf("unknown lb %q", name)
}

// transport returns a pool-specific transport, or nil to use the shared default.
func transport(pc Pool, l proxy.Limits) (*http.Transport, error) {
	if pc.Scheme == "h2c" {
		return proxy.NewH2CTransport(l), nil
	}
	if pc.TLS == nil {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: pc.TLS.ServerName}
	if pc.TLS.CAFile != "" {
		pemData, err := os.ReadFile(pc.TLS.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("tls.caFile %s: no PEM certificates found", pc.TLS.CAFile)
		}
	}
	t := proxy.NewTransport(l)
	t.TLSClientConfig = cfg
	return t, nil
}
