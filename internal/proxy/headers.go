package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"strconv"
	"strings"
)

// HeaderRequestID carries the per-request ID to the upstream and back to the client.
const HeaderRequestID = "X-Request-Id"

// hopHeaders apply to one connection only and must not be forwarded (RFC 9110 §7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// forwardedHeaders are only believed when they come from a trusted proxy.
var forwardedHeaders = []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded"}

// removeHopByHop deletes the standard hop-by-hop headers plus any header the
// sender listed in Connection.
func removeHopByHop(h http.Header) {
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			if name = textproto.TrimString(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopHeaders {
		delete(h, k)
	}
}

// hasToken reports whether a comma-separated header (Connection, TE) contains
// token, case-insensitively.
func hasToken(h http.Header, name, token string) bool {
	for _, v := range h[name] {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(textproto.TrimString(tok), token) {
				return true
			}
		}
	}
	return false
}

// clientAddr returns the peer IP of the TCP connection, unmapped from IPv4-in-IPv6.
func clientAddr(remoteAddr string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// realClient finds the original client. If the TCP peer is a trusted proxy,
// walk X-Forwarded-For from the right (the end our own proxies appended),
// skipping trusted hops. The first untrusted address is the client. Entries
// further left could be forged by that client, so they are never believed. If
// every hop is trusted, the leftmost is used. An unparseable entry stops the
// walk at the last address we could trust.
func realClient(h http.Header, peer netip.Addr, trusted []netip.Prefix) netip.Addr {
	if !isTrusted(peer, trusted) {
		return peer
	}
	client := peer
	vals := h["X-Forwarded-For"]
	for i := len(vals) - 1; i >= 0; i-- {
		hops := strings.Split(vals[i], ",")
		for j := len(hops) - 1; j >= 0; j-- {
			a, err := netip.ParseAddr(textproto.TrimString(hops[j]))
			if err != nil {
				return client
			}
			client = a.Unmap()
			if !isTrusted(client, trusted) {
				return client
			}
		}
	}
	return client
}

// setForwarded writes X-Forwarded-*, Via and the request ID on the outbound headers.
// Headers the client sent are kept only if the client is a trusted proxy;
// otherwise a client could spoof its IP to the backend.
func setForwarded(hdr http.Header, r *http.Request, client netip.Addr, trusted bool) {
	if !trusted {
		for _, k := range forwardedHeaders {
			delete(hdr, k)
		}
	}
	xff := client.String()
	if !client.IsValid() {
		xff = r.RemoteAddr
	}
	if prior := hdr["X-Forwarded-For"]; len(prior) > 0 {
		xff = strings.Join(prior, ", ") + ", " + xff
	}
	hdr.Set("X-Forwarded-For", xff)
	if hdr.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}
		hdr.Set("X-Forwarded-Proto", proto)
	}
	if hdr.Get("X-Forwarded-Host") == "" {
		hdr.Set("X-Forwarded-Host", r.Host)
	}
	hdr.Add("Via", via(r.ProtoMajor, r.ProtoMinor))
}

func via(major, minor int) string {
	return strconv.Itoa(major) + "." + strconv.Itoa(minor) + " portcullis"
}

// requestID keeps a trusted caller's ID if it is well-formed, else makes a new one.
func requestID(in string, trusted bool) string {
	if trusted && validRequestID(in) {
		return in
	}
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails since Go 1.24
	return hex.EncodeToString(b[:])
}

// validRequestID limits IDs to short, log-safe characters (no spaces, quotes or newlines).
func validRequestID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}
