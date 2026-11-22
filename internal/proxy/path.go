package proxy

import (
	"net/http"
	"path"
	"strings"
)

// normalizePath resolves dot segments and repeated slashes before routing, so
// routes, policies and the upstream all act on one path: "//admin" must not
// slip past a rule on "/admin". A path that is already clean costs nothing and
// keeps its original escaping.
func normalizePath(r *http.Request) *http.Request {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/") { // "*" (OPTIONS) and authority forms
		return r
	}
	c := path.Clean(p)
	if c == p {
		return r
	}
	if c != "/" && p[len(p)-1] == '/' {
		if c == p[:len(p)-1] {
			return r
		}
		c += "/"
	}
	out := new(http.Request)
	*out = *r
	u := *r.URL
	u.Path, u.RawPath = c, "" // the old escaping described the old path
	out.URL = &u
	return out
}
