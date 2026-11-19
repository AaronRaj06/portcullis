package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/AaronRaj06/portcullis/internal/lb"
)

// HeaderMatch requires a request header to equal Value exactly (name is case-insensitive).
type HeaderMatch struct{ Name, Value string }

// Route sends matching requests to Pool.
type Route struct {
	Name    string
	Hosts   []string // "app.example.com" or "*.example.com"; empty matches any host
	Path    string   // must start with "/"; "" means "/"
	Exact   bool     // exact path match instead of prefix
	Methods []string // empty matches any method
	Headers []HeaderMatch
	Pool    *lb.Pool
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
		r := routes[i] // copy: the router owns its routes
		if r.Name == "" || names[r.Name] {
			return nil, fmt.Errorf("route %d: name %q is empty or duplicated", i, r.Name)
		}
		names[r.Name] = true
		if r.Pool == nil || len(r.Pool.Backends) == 0 || r.Pool.Balancer == nil {
			return nil, fmt.Errorf("route %q: pool with at least one backend and a balancer is required", r.Name)
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
		if !seenPool[r.Pool] {
			seenPool[r.Pool] = true
			rt.pools = append(rt.pools, r.Pool)
		}
		rp := &r
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
