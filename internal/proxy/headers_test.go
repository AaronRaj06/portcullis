package proxy

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestRemoveHopByHop(t *testing.T) {
	h := http.Header{
		"Connection":          {"X-Drop, keep-alive", "X-Also"},
		"X-Drop":              {"1"},
		"X-Also":              {"1"},
		"Keep-Alive":          {"timeout=5"},
		"Proxy-Authorization": {"secret"},
		"Transfer-Encoding":   {"chunked"},
		"Upgrade":             {"websocket"},
		"Te":                  {"trailers"},
		"X-Keep":              {"yes"},
	}
	removeHopByHop(h)
	if len(h) != 1 || h.Get("X-Keep") != "yes" {
		t.Fatalf("after removeHopByHop: %v, want only X-Keep", h)
	}
}

func TestHasToken(t *testing.T) {
	for v, want := range map[string]bool{"trailers": true, "gzip, Trailers": true, "gzip": false, "": false} {
		if got := hasToken(http.Header{"Te": {v}}, "Te", "trailers"); got != want {
			t.Errorf("hasToken(%q) = %v, want %v", v, got, want)
		}
	}
	up := http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"websocket"}}
	if upgradeType(up) != "websocket" || upgradeType(http.Header{"Upgrade": {"websocket"}}) != "" {
		t.Error("upgradeType must require both Connection: upgrade and Upgrade")
	}
}

func TestClientAddr(t *testing.T) {
	for in, want := range map[string]string{
		"10.1.2.3:5000":          "10.1.2.3",
		"[::1]:80":               "::1",
		"[::ffff:10.0.0.1]:8080": "10.0.0.1", // IPv4-mapped is unmapped so v4 CIDRs match
		"garbage":                "invalid IP",
	} {
		if got := clientAddr(in).String(); got != want {
			t.Errorf("clientAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsTrusted(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if !isTrusted(netip.MustParseAddr("10.9.9.9"), trusted) {
		t.Error("10.9.9.9 should be trusted")
	}
	if isTrusted(netip.MustParseAddr("11.0.0.1"), trusted) || isTrusted(netip.Addr{}, trusted) {
		t.Error("outside address or invalid address must not be trusted")
	}
}

func TestRequestID(t *testing.T) {
	if got := requestID("abc-123", true); got != "abc-123" {
		t.Errorf("trusted valid ID replaced: %q", got)
	}
	for _, tc := range []struct {
		in      string
		trusted bool
	}{
		{"abc-123", false},            // untrusted client
		{"bad id\nX-Inject: 1", true}, // log injection attempt
		{"", true},
	} {
		got := requestID(tc.in, tc.trusted)
		if got == tc.in || len(got) != 32 {
			t.Errorf("requestID(%q, %v) = %q, want fresh 32-char ID", tc.in, tc.trusted, got)
		}
	}
}
