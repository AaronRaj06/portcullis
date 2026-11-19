// Package proxy is the L7 forwarding core: a hand-written reverse proxy built on
// net/http and http.Transport (httputil.ReverseProxy is used only as a benchmark baseline).
//
// It strips hop-by-hop headers, sets X-Forwarded-* and Via (trusting client-sent
// values only from configured proxies), streams bodies in both directions,
// propagates client cancellation upstream, and maps failures to 502/503/504 with a request ID.
package proxy
