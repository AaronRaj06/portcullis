// Package config defines the dynamic (hot-reloadable) configuration: routes,
// pools, backends and their policies. It parses YAML or JSON strictly,
// validates with field-path error messages, and compiles a config into an
// immutable proxy.Snapshot.
//
// Process-level settings (listen addresses, TLS certificate, server timeouts)
// are flags, read once at start-up, like Envoy's bootstrap vs dynamic config.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the dynamic configuration. The same schema is accepted as YAML
// (files) and JSON (PUT /config from the controller).
type Config struct {
	Version        string   `json:"version,omitempty"`        // opaque label from the author or controller
	TrustedProxies []string `json:"trustedProxies,omitempty"` // CIDRs
	Pools          []Pool   `json:"pools"`
	Routes         []Route  `json:"routes"`
	Policies       []Policy `json:"policies,omitempty"`
	// Listeners are extra ports, opened and closed on reload. A route with a
	// port is served only there.
	Listeners []Listener `json:"listeners,omitempty"`
	// Instances is how many proxy replicas share the traffic. Each divides
	// every rate_limit by it, so together they approximate the global limit.
	Instances int `json:"instances,omitempty"` // default 1
}

// Policy attaches a PolicyScript program (docs/POLICY_LANGUAGE.md) to routes.
type Policy struct {
	// ID, if set, replaces the script's policy name as the identity: unique in
	// the config, and the key for rate-limit state. The controller sets it to
	// namespace/name, so tenants cannot collide.
	ID       string   `json:"id,omitempty"`
	Script   string   `json:"script"`
	Routes   []string `json:"routes,omitempty"`   // omitted: every route
	OnError  string   `json:"onError,omitempty"`  // fail_closed (default) or fail_open
	MaxSteps int      `json:"maxSteps,omitempty"` // 0: policy.DefaultMaxSteps
	Timeout  Duration `json:"timeout,omitempty"`  // 0: policy.DefaultTimeout
}

// Pool is a named group of backends.
type Pool struct {
	Name        string       `json:"name"`
	Scheme      string       `json:"scheme,omitempty"`  // http (default), https, h2c
	LB          string       `json:"lb,omitempty"`      // round_robin (default), weighted_round_robin, least_conn, p2c, hash
	HashKey     string       `json:"hashKey,omitempty"` // for lb=hash: ip (default), path, header:NAME, cookie:NAME
	Backends    []Backend    `json:"backends"`
	Status      int          `json:"status,omitempty"`      // instead of backends: always answer this code
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"` // omitted: no active checks
	Ejection    *Ejection    `json:"ejection,omitempty"`
	Retries     *Retries     `json:"retries,omitempty"`
	Panic       *bool        `json:"panic,omitempty"` // omitted: true (ADR 0009)
	TLS         *UpstreamTLS `json:"tls,omitempty"`
}

// Backend is one upstream address.
type Backend struct {
	Address string `json:"address"`          // host:port
	Weight  int    `json:"weight,omitempty"` // 1..1000, default 1
}

// HealthCheck configures active checks. Zero fields take health.DefaultConfig values.
type HealthCheck struct {
	Path      string   `json:"path,omitempty"`
	Interval  Duration `json:"interval,omitempty"`
	Timeout   Duration `json:"timeout,omitempty"`
	Healthy   int      `json:"healthy,omitempty"`
	Unhealthy int      `json:"unhealthy,omitempty"`
	Jitter    float64  `json:"jitter,omitempty"`
}

// Ejection configures passive ejection. Omitted: after 5 failures, 30s base, 5m cap.
type Ejection struct {
	After int      `json:"after"` // 0 disables
	For   Duration `json:"for,omitempty"`
	Max   Duration `json:"max,omitempty"`
}

// Retries configures retries. Omitted: 2 attempts, 20% budget, burst 10.
type Retries struct {
	Attempts int     `json:"attempts"`
	Budget   float64 `json:"budget,omitempty"`
	Burst    int     `json:"burst,omitempty"`
}

// UpstreamTLS configures TLS to https backends.
type UpstreamTLS struct {
	CAFile     string `json:"caFile,omitempty"`     // PEM; default: system roots
	ServerName string `json:"serverName,omitempty"` // SNI and verification name; default: the backend host
}

// Route sends matching requests to one or more pools.
type Route struct {
	Name     string        `json:"name"`
	Hosts    []string      `json:"hosts,omitempty"`
	Path     string        `json:"path,omitempty"`     // default "/"
	PathType string        `json:"pathType,omitempty"` // Prefix (default) or Exact
	Methods  []string      `json:"methods,omitempty"`
	Headers  []HeaderMatch `json:"headers,omitempty"`
	Backends []PoolRef     `json:"backends"` // one entry, or a weighted split

	Port           int         `json:"port,omitempty"` // a listeners[].port; omitted: the -listen address
	RequestHeaders *HeaderEdit `json:"requestHeaders,omitempty"`
	Redirect       *Redirect   `json:"redirect,omitempty"`
}

// HeaderEdit changes headers on the way upstream: set, then add, then remove.
type HeaderEdit struct {
	Set    []HeaderMatch `json:"set,omitempty"`
	Add    []HeaderMatch `json:"add,omitempty"`
	Remove []string      `json:"remove,omitempty"`
}

// Redirect answers with a Location built from these fields and the request's path and query.
type Redirect struct {
	Scheme string `json:"scheme"`         // http or https
	Host   string `json:"host,omitempty"` // omitted: the request's
	Port   int    `json:"port"`           // left out of Location when it is the scheme's default
	Status int    `json:"status"`         // 301, 302, 303, 307 or 308
}

// Listener is an extra port: HTTP, or HTTPS if it has certificates.
type Listener struct {
	Port         int           `json:"port"`
	Certificates []Certificate `json:"certificates,omitempty"`
}

// Certificate is a PEM pair and the server name it is served for.
type Certificate struct {
	Host string `json:"host,omitempty"` // omitted: any name
	Cert string `json:"cert"`
	Key  string `json:"key"` // secret: see Redacted
}

// Redacted returns a copy of c that is safe to show: no private keys.
func (c *Config) Redacted() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Listeners = slices.Clone(c.Listeners)
	for i := range out.Listeners {
		l := &out.Listeners[i]
		l.Certificates = slices.Clone(l.Certificates)
		for j := range l.Certificates {
			l.Certificates[j].Key = "REDACTED"
		}
	}
	return &out
}

// HeaderMatch is an exact header match.
type HeaderMatch struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PoolRef names a pool and its share of the route's traffic.
type PoolRef struct {
	Pool   string `json:"pool"`
	Weight *int   `json:"weight,omitempty"` // default 1; 0 sends nothing
}

// Duration is a time.Duration written as "5s" in YAML and JSON.
type Duration time.Duration

// MarshalJSON writes the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON accepts a Go duration string such as "250ms" or "1m30s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\", got %s", b)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MaxSize bounds a config document (files and PUT bodies).
const MaxSize = 16 << 20

// Parse decodes YAML or JSON strictly: unknown fields are errors, so a typo
// like "pathtype" is reported instead of silently ignored.
func Parse(raw []byte) (*Config, error) {
	if len(raw) > MaxSize {
		return nil, fmt.Errorf("config is %d bytes, limit is %d", len(raw), MaxSize)
	}
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &c, nil
}

// Hash is the sha256 of the canonical JSON form (after defaults) plus the
// contents of every file the config references (CA bundles). So two configs
// that mean the same thing hash the same whatever their formatting, and
// replacing a CA file's contents counts as a change.
func (c *Config) Hash() string { return c.hashWith(readFiles(c)) }

func (c *Config) hashWith(files map[string][]byte) string {
	b, err := json.Marshal(c)
	if err != nil { // cannot happen: every field is JSON-safe
		panic(err)
	}
	h := sha256.New()
	h.Write(b)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sum := sha256.Sum256(files[p])
		fmt.Fprintf(h, "\x00%s\x00%x", p, sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readFiles reads every referenced file once. Unreadable files map to nil;
// Validate reports them.
func readFiles(c *Config) map[string][]byte {
	files := map[string][]byte{}
	for _, p := range c.Pools {
		if p.TLS != nil && p.TLS.CAFile != "" {
			b, _ := os.ReadFile(p.TLS.CAFile)
			files[p.TLS.CAFile] = b
		}
	}
	return files
}

// applyDefaults fills omitted optional fields in place, so validation,
// compilation and hashing all see the same explicit values.
func (c *Config) applyDefaults() {
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Scheme == "" {
			p.Scheme = "http"
		}
		if p.LB == "" {
			p.LB = "round_robin"
		}
		if p.LB == "hash" && p.HashKey == "" {
			p.HashKey = "ip"
		}
		for j := range p.Backends {
			if p.Backends[j].Weight == 0 {
				p.Backends[j].Weight = 1
			}
		}
		if p.Ejection == nil {
			p.Ejection = &Ejection{After: 5, For: Duration(30 * time.Second), Max: Duration(5 * time.Minute)}
		} else if p.Ejection.For == 0 {
			p.Ejection.For = Duration(30 * time.Second)
		}
		if p.Retries == nil {
			p.Retries = &Retries{Attempts: 2, Budget: 0.2, Burst: 10}
		}
		if p.Panic == nil {
			on := true // serve from all backends rather than fail when every one looks down (ADR 0009)
			p.Panic = &on
		}
		if hc := p.HealthCheck; hc != nil {
			fillHealth(hc)
		}
	}
	if c.Instances == 0 {
		c.Instances = 1
	}
	for i := range c.Policies {
		if c.Policies[i].OnError == "" {
			c.Policies[i].OnError = "fail_closed"
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Path == "" {
			r.Path = "/"
		}
		if r.PathType == "" {
			r.PathType = "Prefix"
		}
		for j := range r.Methods {
			r.Methods[j] = strings.ToUpper(r.Methods[j])
		}
		for j := range r.Backends {
			if r.Backends[j].Weight == nil {
				one := 1
				r.Backends[j].Weight = &one
			}
		}
	}
}
