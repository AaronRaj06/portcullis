package config

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AaronRaj06/portcullis/internal/health"
	"github.com/AaronRaj06/portcullis/internal/policy"
	"github.com/AaronRaj06/portcullis/internal/ratelimit"
)

// ValidationError lists every problem found, each prefixed with its field path
// (pools[0].backends[1].address: ...), so one round trip reports all of them.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid config:\n  " + strings.Join(e.Problems, "\n  ")
}

type checker struct{ problems []string }

func (c *checker) add(path, format string, a ...any) {
	c.problems = append(c.problems, path+": "+fmt.Sprintf(format, a...))
}

func fillHealth(hc *HealthCheck) {
	d := health.DefaultConfig
	if hc.Path == "" {
		hc.Path = d.Path
	}
	if hc.Interval == 0 {
		hc.Interval = Duration(d.Interval)
	}
	if hc.Timeout == 0 {
		hc.Timeout = Duration(d.Timeout)
	}
	if hc.Healthy == 0 {
		hc.Healthy = d.Healthy
	}
	if hc.Unhealthy == 0 {
		hc.Unhealthy = d.Unhealthy
	}
}

var (
	validSchemes = map[string]bool{"http": true, "https": true, "h2c": true}
	validLBs     = map[string]bool{"round_robin": true, "weighted_round_robin": true, "least_conn": true, "p2c": true, "hash": true}
)

// Validate applies defaults to c and checks it. It returns a *ValidationError
// listing every problem, or nil.
func (c *Config) Validate() error {
	c.applyDefaults()
	var v checker
	for i, cidr := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			v.add(fmt.Sprintf("trustedProxies[%d]", i), "%q is not a CIDR", cidr)
		}
	}
	if len(c.Pools) == 0 {
		v.add("pools", "at least one pool is required")
	}
	pools := map[string]bool{}
	for i := range c.Pools {
		p := &c.Pools[i]
		path := fmt.Sprintf("pools[%d]", i)
		switch {
		case p.Name == "":
			v.add(path+".name", "required")
		case pools[p.Name]:
			v.add(path+".name", "duplicate pool %q", p.Name)
		}
		pools[p.Name] = true
		if !validSchemes[p.Scheme] {
			v.add(path+".scheme", "%q is not http, https or h2c", p.Scheme)
		}
		if !validLBs[p.LB] {
			v.add(path+".lb", "%q is not round_robin, weighted_round_robin, least_conn, p2c or hash", p.LB)
		}
		if p.LB == "hash" && !validHashKey(p.HashKey) {
			v.add(path+".hashKey", "%q is not ip, path, header:NAME or cookie:NAME", p.HashKey)
		}
		if p.LB != "hash" && p.HashKey != "" {
			v.add(path+".hashKey", "only used with lb: hash")
		}
		if p.Status != 0 {
			if p.Status < 400 || p.Status > 599 || len(p.Backends) > 0 {
				v.add(path+".status", "must be 400..599, on a pool without backends")
			}
		} else if len(p.Backends) == 0 {
			v.add(path+".backends", "at least one backend is required")
		}
		seen := map[string]bool{}
		for j, b := range p.Backends {
			bp := fmt.Sprintf("%s.backends[%d]", path, j)
			if err := validAddress(b.Address); err != nil {
				v.add(bp+".address", "%v", err)
			}
			if seen[b.Address] {
				v.add(bp+".address", "duplicate backend %q", b.Address)
			}
			seen[b.Address] = true
			if b.Weight < 1 || b.Weight > 1000 {
				v.add(bp+".weight", "%d is outside 1..1000", b.Weight)
			}
		}
		if hc := p.HealthCheck; hc != nil {
			hp := path + ".healthCheck"
			if !strings.HasPrefix(hc.Path, "/") {
				v.add(hp+".path", "%q must start with /", hc.Path)
			}
			if hc.Interval < Duration(10*time.Millisecond) {
				v.add(hp+".interval", "must be at least 10ms")
			}
			if hc.Timeout <= 0 || hc.Timeout > hc.Interval {
				v.add(hp+".timeout", "must be positive and no longer than the interval")
			}
			if hc.Healthy < 1 || hc.Unhealthy < 1 {
				v.add(hp, "healthy and unhealthy thresholds must be at least 1")
			}
			if hc.Jitter < 0 || hc.Jitter > 1 {
				v.add(hp+".jitter", "%v is outside 0..1", hc.Jitter)
			}
		}
		if e := p.Ejection; e.After < 0 || e.For < 0 || e.Max < 0 || (e.Max > 0 && e.Max < e.For) {
			v.add(path+".ejection", "after/for/max must be non-negative and max >= for")
		}
		if r := p.Retries; r.Attempts < 0 || r.Attempts > 5 || r.Budget < 0 || r.Budget > 1 || r.Burst < 0 {
			v.add(path+".retries", "attempts must be 0..5, budget 0..1, burst >= 0")
		}
		if t := p.TLS; t != nil {
			if p.Scheme != "https" {
				v.add(path+".tls", "only valid with scheme: https")
			}
			if t.CAFile != "" {
				if _, err := os.Stat(t.CAFile); err != nil {
					v.add(path+".tls.caFile", "%v", err)
				}
			}
		}
	}

	if len(c.Routes) == 0 {
		v.add("routes", "at least one route is required")
	}
	ports := v.listeners(c.Listeners)
	routes := map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		path := fmt.Sprintf("routes[%d]", i)
		switch {
		case r.Name == "":
			v.add(path+".name", "required")
		case routes[r.Name]:
			v.add(path+".name", "duplicate route %q", r.Name)
		}
		routes[r.Name] = true
		if !strings.HasPrefix(r.Path, "/") {
			v.add(path+".path", "%q must start with /", r.Path)
		}
		if r.PathType != "Prefix" && r.PathType != "Exact" {
			v.add(path+".pathType", "%q is not Prefix or Exact", r.PathType)
		}
		for j, h := range r.Hosts {
			if strings.Contains(strings.TrimPrefix(h, "*."), "*") || h == "" || strings.ContainsAny(h, " /:") {
				v.add(fmt.Sprintf("%s.hosts[%d]", path, j), "%q is not a hostname or *.wildcard", h)
			}
		}
		for j, h := range r.Headers {
			if h.Name == "" {
				v.add(fmt.Sprintf("%s.headers[%d].name", path, j), "required")
			}
		}
		if r.Port != 0 && !ports[r.Port] {
			v.add(path+".port", "no listener has port %d", r.Port)
		}
		if e := r.RequestHeaders; e != nil {
			if err := e.Check(); err != nil {
				v.add(path+".requestHeaders", "%v", err)
			}
		}
		if d := r.Redirect; d != nil && ((d.Scheme != "http" && d.Scheme != "https") || d.Port < 1 || d.Port > 65535 ||
			!slices.Contains(redirectStatuses, d.Status) || strings.ContainsAny(d.Host, " /?#@\\\r\n")) {
			v.add(path+".redirect", "needs scheme http or https, a port, a plain host and a 301, 302, 303, 307 or 308 status")
		}
		if len(r.Backends) == 0 {
			v.add(path+".backends", "at least one pool reference is required")
		}
		total := 0
		for j, ref := range r.Backends {
			rp := fmt.Sprintf("%s.backends[%d]", path, j)
			if !pools[ref.Pool] {
				v.add(rp+".pool", "unknown pool %q", ref.Pool)
			}
			if *ref.Weight < 0 {
				v.add(rp+".weight", "must not be negative")
			}
			total += *ref.Weight
		}
		if len(r.Backends) > 0 && total == 0 {
			v.add(path+".backends", "all weights are 0")
		}
	}
	v.policies(c, pools, routes)
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

// MaxPolicies bounds the policies that run on one route, and so on one
// request. The total is bounded only by MaxSize, so one tenant's policies
// cannot use up another's allowance.
const MaxPolicies = 64

var redirectStatuses = []int{301, 302, 303, 307, 308}

func (c *checker) listeners(ls []Listener) map[int]bool {
	ports := map[int]bool{}
	for i, l := range ls {
		path := fmt.Sprintf("listeners[%d]", i)
		if l.Port < 1 || l.Port > 65535 || ports[l.Port] {
			c.add(path+".port", "%d is not a unique TCP port", l.Port)
		}
		ports[l.Port] = true
		for j, cert := range l.Certificates {
			if _, err := tls.X509KeyPair([]byte(cert.Cert), []byte(cert.Key)); err != nil {
				c.add(fmt.Sprintf("%s.certificates[%d]", path, j), "%v", err)
			}
		}
	}
	return ports
}

// Check reports an edit the proxy must not make: an invalid header, or one
// that belongs to the connection or the message framing.
func (e *HeaderEdit) Check() error {
	name := func(n string) error {
		if n == "" || strings.Trim(n, tokenChars) != "" {
			return fmt.Errorf("%q is not a header name", n)
		}
		switch http.CanonicalHeaderKey(n) {
		case "Host", "Content-Length", "Connection", "Proxy-Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
			return fmt.Errorf("header %q cannot be modified", n)
		}
		return nil
	}
	for _, h := range slices.Concat(e.Set, e.Add) {
		if err := name(h.Name); err != nil {
			return err
		}
		if strings.ContainsFunc(h.Value, func(r rune) bool { return r == 0x7f || (r < 0x20 && r != '\t') }) {
			return fmt.Errorf("header %q: the value has control characters", h.Name)
		}
	}
	for _, n := range e.Remove {
		if err := name(n); err != nil {
			return err
		}
	}
	return nil
}

const tokenChars = //nolint:gosec // RFC 9110 token characters, not a credential
"!#$%&'*+-.^_`|~0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func (c *checker) policies(cfg *Config, pools, routes map[string]bool) {
	if cfg.Instances < 1 || cfg.Instances > 1000 {
		c.add("instances", "%d is outside 1..1000", cfg.Instances)
	}
	perRoute, everywhere := map[string]int{}, 0
	for _, p := range cfg.Policies {
		if len(p.Routes) == 0 {
			everywhere++
		}
		for _, r := range p.Routes {
			perRoute[r]++
		}
	}
	for i, r := range cfg.Routes {
		if n := everywhere + perRoute[r.Name]; n > MaxPolicies {
			c.add(fmt.Sprintf("routes[%d]", i), "%d policies, limit is %d per route", n, MaxPolicies)
		}
	}
	names := map[string]bool{}
	for i := range cfg.Policies {
		pc := &cfg.Policies[i]
		path := fmt.Sprintf("policies[%d]", i)
		for j, r := range pc.Routes {
			if !routes[r] {
				c.add(fmt.Sprintf("%s.routes[%d]", path, j), "unknown route %q", r)
			}
		}
		if pc.OnError != "fail_closed" && pc.OnError != "fail_open" {
			c.add(path+".onError", "%q is not fail_closed or fail_open", pc.OnError)
		}
		if pc.MaxSteps < 0 || pc.MaxSteps > 1_000_000 {
			c.add(path+".maxSteps", "%d is outside 0..1000000", pc.MaxSteps)
		}
		if t := time.Duration(pc.Timeout); t != 0 && (t < 10*time.Microsecond || t > 100*time.Millisecond) {
			c.add(path+".timeout", "%v is outside 10µs..100ms", t)
		}
		// Rate limiters are not needed to check a script; Compile builds the real ones.
		prog, err := compilePolicy(pc, cfg.Instances, pools, func(string, int, time.Duration) *ratelimit.Limiter { return nil })
		if err != nil {
			var list policy.ErrorList
			if errors.As(err, &list) {
				for _, e := range list {
					c.add(path+".script", "%s", e.Error())
				}
			} else {
				c.add(path+".script", "%v", err)
			}
			continue
		}
		if names[prog.Name] {
			c.add(path+".script", "duplicate policy name %q", prog.Name)
		}
		names[prog.Name] = true
	}
}

func compilePolicy(pc *Policy, instances int, pools map[string]bool, lim func(string, int, time.Duration) *ratelimit.Limiter) (*policy.Program, error) {
	return policy.Compile(pc.Script, policy.Options{
		Name: pc.ID, MaxSteps: pc.MaxSteps, Timeout: time.Duration(pc.Timeout), FailOpen: pc.OnError == "fail_open",
		Share: instances, Pools: pools, Limiter: lim,
	})
}

func validHashKey(k string) bool {
	kind, name, _ := strings.Cut(k, ":")
	switch kind {
	case "ip", "path":
		return name == ""
	case "header", "cookie":
		return name != ""
	}
	return false
}

func validAddress(a string) error {
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		return fmt.Errorf("%q: want host:port", a)
	}
	if host == "" {
		return errors.New("host is empty")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not 1..65535", port)
	}
	return nil
}
