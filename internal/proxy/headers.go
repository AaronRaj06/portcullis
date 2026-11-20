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

// forwardableHeaders copies h without hop-by-hop headers, in one pass and one
// allocation (the map): value slices are shared with the inbound request,
// which is safe because nothing writes into them. This replaced Clone followed
// by removeHopByHop (Phase 4 round 2).
func forwardableHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h)+4) // +4: room for the forwarding headers
	listed := h["Connection"]
	for k, vv := range h {
		if isHopHeader(k) || (len(listed) > 0 && hasToken(h, "Connection", k)) {
			continue
		}
		out[k] = vv
	}
	return out
}

func isHopHeader(k string) bool {
	switch k {
	case "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
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
//
// peer is the TCP peer's address as a string. dyn is scratch space of length
// >= 2 for the per-request values, so they share one allocation instead of
// one []string each (a hot-path optimisation, Phase 4 round 1).
func setForwarded(hdr http.Header, r *http.Request, peer string, trusted bool, dyn []string) {
	if !trusted {
		for _, k := range forwardedHeaders {
			delete(hdr, k)
		}
	}
	xff := peer
	if prior := hdr["X-Forwarded-For"]; len(prior) > 0 {
		xff = strings.Join(prior, ", ") + ", " + xff
	}
	dyn[0] = xff
	hdr["X-Forwarded-For"] = dyn[0:1:1]
	if len(hdr["X-Forwarded-Proto"]) == 0 {
		if r.TLS != nil {
			hdr["X-Forwarded-Proto"] = protoHTTPS
		} else {
			hdr["X-Forwarded-Proto"] = protoHTTP
		}
	}
	if len(hdr["X-Forwarded-Host"]) == 0 {
		dyn[1] = r.Host
		hdr["X-Forwarded-Host"] = dyn[1:2:2]
	}
	addVia(hdr, r.ProtoMajor, r.ProtoMinor)
}

// Shared read-only header values. Each has cap == len, so an append by anyone
// copies instead of writing into the shared array. Nothing assigns into them.
var (
	protoHTTP  = []string{"http"}[:1:1]
	protoHTTPS = []string{"https"}[:1:1]
	emptyValue = []string{""}[:1:1]
	via10      = []string{"1.0 portcullis"}[:1:1]
	via11      = []string{"1.1 portcullis"}[:1:1]
	via20      = []string{"2.0 portcullis"}[:1:1]
)

func via(major, minor int) string {
	return strconv.Itoa(major) + "." + strconv.Itoa(minor) + " portcullis"
}

// addVia appends our Via entry, using a shared constant when it is the only one.
func addVia(h http.Header, major, minor int) {
	var v []string
	switch {
	case major == 1 && minor == 1:
		v = via11
	case major == 2:
		v = via20
	case major == 1 && minor == 0:
		v = via10
	default:
		v = []string{via(major, minor)}
	}
	if prior := h["Via"]; len(prior) > 0 {
		h["Via"] = append(prior[:len(prior):len(prior)], v[0])
		return
	}
	h["Via"] = v
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
