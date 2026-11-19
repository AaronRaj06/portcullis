package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/AaronRaj06/portcullis/internal/lb"
)

// HeaderMatch requires a request header to equal Value exactly (name is case-insensitive).
type HeaderMatch struct{ Name, Value string }

// WeightedPool is one target of a traffic split.
type WeightedPool struct {
	Pool   *lb.Pool
	Weight int // 0 sends no traffic (Gateway API semantics); the route needs at least one > 0
}

// Route sends matching requests to Pool, or splits them across Split by weight
// (canary: 90 stable / 10 canary). Set exactly one of Pool and Split.
type Route struct {
	Name    string
	Hosts   []string // "app.example.com" or "*.example.com"; empty matches any host
	Path    string   // must start with "/"; "" means "/"
	Exact   bool     // exact path match instead of prefix
	Methods []string // empty matches any method
	Headers []HeaderMatch
	Pool    *lb.Pool
	Split   []WeightedPool

	// order is one smooth-weighted cycle of Split, walked with an atomic counter:
	// exact proportions, interleaved, lock-free. A pool with no available
	// backend is not skipped: a dead canary fails its share (no silent failover).
	order []*lb.Pool
	next  atomic.Uint64
}

// PickPool returns the pool for the next request on this route.
func (rt *Route) PickPool() *lb.Pool {
	if len(rt.order) == 1 {
		return rt.order[0]
	}
	return rt.order[(rt.next.Add(1)-1)%uint64(len(rt.order))]
}

// Router picks the most specific route, following Gateway API precedence:
//  1. hostname: exact > longest wildcard suffix > any host;
//  2. then, within that hostname: exact path > longest prefix > has a method match >
//     more header matches > declaration order.
//
// If nothing matches under a more specific hostname, less specific ones are tried.
type Router struct {
	exact    map[string][]*Route
	wildcard []wildGroup // longest suffix first
	any      []*Route
	pools    []*lb.Pool
}

type wildGroup struct {
	suffix string // ".example.com"
	routes []*Route
}

// NewRouter validates routes and indexes them for matching.
func NewRouter(routes []Route) (*Router, error) {
	rt := &Router{exact: map[string][]*Route{}}
	wild := map[string][]*Route{}
	names := map[string]bool{}
	seenPool := map[*lb.Pool]bool{}
	for i := range routes {
		rp := &Route{ // copy: the router owns its routes
			Name: routes[i].Name, Hosts: routes[i].Hosts, Path: routes[i].Path, Exact: routes[i].Exact,
			Methods: routes[i].Methods, Headers: slices.Clone(routes[i].Headers),
			Pool: routes[i].Pool, Split: routes[i].Split,
		}
		r := rp
		if r.Name == "" || names[r.Name] {
			return nil, fmt.Errorf("route %d: name %q is empty or duplicated", i, r.Name)
		}
		names[r.Name] = true
		if (r.Pool == nil) == (len(r.Split) == 0) {
			return nil, fmt.Errorf("route %q: set exactly one of Pool and Split", r.Name)
		}
		split := r.Split
		if r.Pool != nil {
			split = []WeightedPool{{Pool: r.Pool, Weight: 1}}
		}
		weights := make([]int, len(split))
		for j, wp := range split {
			if wp.Pool == nil || len(wp.Pool.Backends) == 0 || wp.Pool.Balancer == nil {
				return nil, fmt.Errorf("route %q: every pool needs at least one backend and a balancer", r.Name)
			}
			if wp.Weight < 0 {
				return nil, fmt.Errorf("route %q: negative weight for pool %q", r.Name, wp.Pool.Name)
			}
			weights[j] = wp.Weight
			if !seenPool[wp.Pool] {
				seenPool[wp.Pool] = true
				rt.pools = append(rt.pools, wp.Pool)
			}
		}
		for _, j := range lb.SmoothOrder(weights) {
			r.order = append(r.order, split[j].Pool)
		}
		if len(r.order) == 0 {
			return nil, fmt.Errorf("route %q: all split weights are 0", r.Name)
		}
		if r.Path == "" {
			r.Path = "/"
		}
		if !strings.HasPrefix(r.Path, "/") {
			return nil, fmt.Errorf("route %q: path %q must start with /", r.Name, r.Path)
		}
		for j := range r.Headers {
			r.Headers[j].Name = http.CanonicalHeaderKey(r.Headers[j].Name)
		}
		if len(r.Hosts) == 0 {
			rt.any = append(rt.any, rp)
		}
		for _, h := range r.Hosts {
			h = strings.ToLower(strings.TrimSuffix(h, "."))
			switch {
			case strings.HasPrefix(h, "*."):
				wild[h[1:]] = append(wild[h[1:]], rp)
			case h == "" || strings.Contains(h, "*"):
				return nil, fmt.Errorf("route %q: invalid host %q (only a leading *. wildcard is allowed)", r.Name, h)
			default:
				rt.exact[h] = append(rt.exact[h], rp)
			}
		}
	}
	if len(routes) == 0 {
		return nil, errors.New("router: at least one route is required")
	}
	for s, rs := range wild {
		rt.wildcard = append(rt.wildcard, wildGroup{s, rs})
	}
	slices.SortFunc(rt.wildcard, func(a, b wildGroup) int {
		if d := len(b.suffix) - len(a.suffix); d != 0 {
			return d
		}
		return strings.Compare(a.suffix, b.suffix)
	})
	for _, rs := range rt.exact {
		sortBySpecificity(rs)
	}
	for _, g := range rt.wildcard {
		sortBySpecificity(g.routes)
	}
	sortBySpecificity(rt.any)
	return rt, nil
}

func sortBySpecificity(rs []*Route) {
	slices.SortStableFunc(rs, func(a, b *Route) int {
		switch {
		case a.Exact != b.Exact:
			if a.Exact {
				return -1
			}
			return 1
		case len(a.Path) != len(b.Path):
			return len(b.Path) - len(a.Path)
		case (len(a.Methods) > 0) != (len(b.Methods) > 0):
			if len(a.Methods) > 0 {
				return -1
			}
			return 1
		}
		return len(b.Headers) - len(a.Headers)
	})
}

// Pools returns every distinct pool referenced by a route.
func (rt *Router) Pools() []*lb.Pool { return rt.pools }

// Match returns the most specific matching route, or nil.
func (rt *Router) Match(r *http.Request) *Route {
	host := normalizeHost(r.Host)
	if rs, ok := rt.exact[host]; ok {
		if m := firstMatch(rs, r); m != nil {
			return m
		}
	}
	for _, g := range rt.wildcard {
		if len(host) > len(g.suffix) && strings.HasSuffix(host, g.suffix) {
			if m := firstMatch(g.routes, r); m != nil {
				return m
			}
		}
	}
	return firstMatch(rt.any, r)
}

func firstMatch(rs []*Route, r *http.Request) *Route {
	for _, rt := range rs {
		if rt.matches(r) {
			return rt
		}
	}
	return nil
}

func (rt *Route) matches(r *http.Request) bool {
	p := r.URL.Path
	if rt.Exact {
		if p != rt.Path {
			return false
		}
	} else if !pathPrefix(p, rt.Path) {
		return false
	}
	if len(rt.Methods) > 0 && !slices.Contains(rt.Methods, r.Method) {
		return false
	}
	for _, hm := range rt.Headers {
		if r.Header.Get(hm.Name) != hm.Value {
			return false
		}
	}
	return true
}

// pathPrefix matches whole path segments, as Gateway API PathPrefix does:
// "/api" matches "/api" and "/api/v1" but not "/apiv1".
func pathPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || strings.HasSuffix(prefix, "/") || path[len(prefix)] == '/'
}

// normalizeHost lowercases and strips any port, IPv6 brackets and trailing dot.
// (net.SplitHostPort allocates an error for the common no-port case.)
func normalizeHost(h string) string {
	if i := strings.LastIndexByte(h, ':'); i > strings.LastIndexByte(h, ']') {
		h = h[:i]
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
