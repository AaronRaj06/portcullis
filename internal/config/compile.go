package config

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/AaronRaj06/portcullis/internal/health"
	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/policy"
	"github.com/AaronRaj06/portcullis/internal/proxy"
	"github.com/AaronRaj06/portcullis/internal/ratelimit"
)

// Compiled is a validated config turned into live objects, not yet published.
type Compiled struct {
	Snapshot   *proxy.Snapshot
	Health     []PoolHealth               // active checks to run while this snapshot is current
	Transports map[string]*http.Transport // pool-specific transports by settings key
}

// PoolHealth pairs a pool with its active health-check settings.
type PoolHealth struct {
	Pool   *lb.Pool
	Config health.Config
}

// Compile validates c and builds a snapshot. Backends that also existed in prev
// (same pool name and address) inherit their health and ejection state.
// Pool transports whose settings are unchanged are taken from reuse, so their
// upstream connections survive the reload.
func Compile(c *Config, l proxy.Limits, prev *proxy.Snapshot, reuse map[string]*http.Transport) (*Compiled, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	files := readFiles(c) // read once: the hash and the transports see the same bytes
	old := map[string]*lb.Backend{}
	if prev != nil {
		for _, p := range prev.AllPools() {
			for _, b := range p.Backends {
				old[p.Name+"|"+b.Addr] = b
			}
		}
	}

	out := &Compiled{Transports: map[string]*http.Transport{}}
	pools := map[string]*lb.Pool{}
	for _, pc := range c.Pools {
		if pc.Status != 0 {
			pools[pc.Name] = &lb.Pool{Name: pc.Name, Status: pc.Status}
			continue
		}
		p := &lb.Pool{Name: pc.Name, Scheme: pc.Scheme, Panic: *pc.Panic}
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
		if key := transportKey(pc, files); key != "" {
			t := out.Transports[key]
			if t == nil {
				t = reuse[key]
			}
			if t == nil {
				var err error
				if t, err = transport(pc, l, files); err != nil {
					return nil, fmt.Errorf("pool %q: %w", pc.Name, err)
				}
			}
			p.Transport = t
			out.Transports[key] = t
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

	progs, limiters, err := compilePolicies(c, pools, prev)
	if err != nil {
		return nil, err
	}

	byPort := map[int][]proxy.Route{}
	for _, rc := range c.Routes {
		r := proxy.Route{Name: rc.Name, Hosts: rc.Hosts, Path: rc.Path, Exact: rc.PathType == "Exact", Methods: rc.Methods,
			Policies: progs[rc.Name]}
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
		if e := rc.RequestHeaders; e != nil {
			r.Edit = &proxy.HeaderEdit{Set: canonical(e.Set), Add: canonical(e.Add)}
			for _, k := range e.Remove {
				r.Edit.Remove = append(r.Edit.Remove, http.CanonicalHeaderKey(k))
			}
		}
		if d := rc.Redirect; d != nil {
			r.Redirect = &proxy.Redirect{Scheme: d.Scheme, Host: d.Host, Port: d.Port, Status: d.Status}
		}
		byPort[rc.Port] = append(byPort[rc.Port], r)
	}
	newRouter := func(port int) (*proxy.Router, error) {
		if len(byPort[port]) == 0 {
			return &proxy.Router{}, nil // a listener without routes answers 404
		}
		r, err := proxy.NewRouter(byPort[port])
		if err != nil {
			return nil, &ValidationError{Problems: []string{"routes: " + err.Error()}}
		}
		return r, nil
	}
	router, err := newRouter(0)
	if err != nil {
		return nil, err
	}
	ports := map[int]*proxy.Port{}
	for _, lc := range c.Listeners {
		p := &proxy.Port{}
		if p.Router, err = newRouter(lc.Port); err != nil {
			return nil, err
		}
		for _, cc := range lc.Certificates {
			cert, err := tls.X509KeyPair([]byte(cc.Cert), []byte(cc.Key))
			if err != nil { // validated above
				return nil, fmt.Errorf("listener %d: %w", lc.Port, err)
			}
			p.Certs = append(p.Certs, proxy.Cert{Host: strings.ToLower(cc.Host), Cert: &cert})
		}
		ports[lc.Port] = p
	}
	trusted := make([]netip.Prefix, 0, len(c.TrustedProxies))
	for _, s := range c.TrustedProxies {
		trusted = append(trusted, netip.MustParsePrefix(s).Masked()) // validated above
	}
	out.Snapshot = &proxy.Snapshot{Router: router, Ports: ports, TrustedProxies: trusted, Pools: pools, Limiters: limiters,
		Version: c.Version, Hash: c.hashWith(files)}
	return out, nil
}

// compilePolicies compiles every policy and returns them per route name, in
// config order. Rate limiters whose call site is unchanged (same policy name,
// position, limit and window) are taken from prev, so a reload does not reset
// anyone's counters.
func compilePolicies(c *Config, pools map[string]*lb.Pool, prev *proxy.Snapshot) (map[string][]*policy.Program, map[string]*ratelimit.Limiter, error) {
	names := make(map[string]bool, len(pools))
	for n := range pools {
		names[n] = true
	}
	limiters := map[string]*ratelimit.Limiter{}
	lim := func(site string, limit int, window time.Duration) *ratelimit.Limiter {
		l := limiters[site]
		if l == nil && prev != nil {
			l = prev.Limiters[site]
		}
		if l == nil {
			l = ratelimit.New(limit, window, 0)
		}
		limiters[site] = l
		return l
	}
	byRoute := map[string][]*policy.Program{}
	for i := range c.Policies {
		pc := &c.Policies[i]
		prog, err := compilePolicy(pc, c.Instances, names, lim)
		if err != nil { // Validate already compiled it, so this cannot happen
			return nil, nil, fmt.Errorf("policies[%d]: %w", i, err)
		}
		targets := pc.Routes
		if len(targets) == 0 {
			for _, r := range c.Routes {
				targets = append(targets, r.Name)
			}
		}
		for _, r := range targets {
			byRoute[r] = append(byRoute[r], prog)
		}
	}
	return byRoute, limiters, nil
}

func canonical(hs []HeaderMatch) []proxy.HeaderMatch {
	out := make([]proxy.HeaderMatch, 0, len(hs))
	for _, h := range hs {
		out = append(out, proxy.HeaderMatch{Name: http.CanonicalHeaderKey(h.Name), Value: h.Value})
	}
	return out
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

// transportKey identifies the settings a pool-specific transport depends on;
// "" means the pool uses the shared default transport.
func transportKey(pc Pool, files map[string][]byte) string {
	switch {
	case pc.Scheme == "h2c":
		return "h2c"
	case pc.TLS != nil:
		sum := sha256.Sum256(files[pc.TLS.CAFile])
		return fmt.Sprintf("tls|%s|%s|%x", pc.TLS.ServerName, pc.TLS.CAFile, sum)
	}
	return ""
}

// transport builds a pool-specific transport (see transportKey).
func transport(pc Pool, l proxy.Limits, files map[string][]byte) (*http.Transport, error) {
	if pc.Scheme == "h2c" {
		return proxy.NewH2CTransport(l), nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: pc.TLS.ServerName}
	if pc.TLS.CAFile != "" {
		pemData := files[pc.TLS.CAFile]
		if pemData == nil {
			return nil, fmt.Errorf("tls.caFile %s: unreadable", pc.TLS.CAFile)
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
