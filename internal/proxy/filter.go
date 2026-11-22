package proxy

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// HeaderEdit changes the upstream request's headers: set, then add, then remove.
// Names are canonical (config.Compile canonicalises them).
type HeaderEdit struct {
	Set, Add []HeaderMatch
	Remove   []string
}

func (e *HeaderEdit) apply(h http.Header) {
	for _, f := range e.Set {
		h[f.Name] = []string{f.Value}
	}
	for _, f := range e.Add {
		h[f.Name] = append(slices.Clip(h[f.Name]), f.Value) // value slices are shared with the inbound request
	}
	for _, k := range e.Remove {
		if k == "User-Agent" {
			h[k] = emptyValue // deleting it would make net/http send its own
			continue
		}
		delete(h, k)
	}
}

// Redirect answers a route without an upstream.
type Redirect struct {
	Scheme string
	Host   string // "": the request's host
	Port   int
	Status int
}

func (d *Redirect) location(reqHost string, u *url.URL) string {
	host := d.Host
	if host == "" {
		host = normalizeHost(reqHost)
	}
	switch {
	case (d.Scheme == "http" && d.Port != 80) || (d.Scheme == "https" && d.Port != 443):
		host = net.JoinHostPort(host, strconv.Itoa(d.Port))
	case strings.Contains(host, ":"):
		host = "[" + host + "]"
	}
	out := url.URL{Scheme: d.Scheme, Host: host, Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	return out.String()
}
