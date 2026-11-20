package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for Sec-WebSocket-Accept; it is not used for security
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tunnelSet tracks upgraded connections. http.Server.Shutdown forgets a
// connection once it is hijacked, so without this a drain would not wait for
// WebSockets and would leave them open. It is only touched on the upgrade
// path, so its mutex is never on the normal request path.
type tunnelSet struct {
	mu    sync.Mutex
	conns map[io.Closer]struct{}
}

func (t *tunnelSet) add(cs ...io.Closer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conns == nil {
		t.conns = map[io.Closer]struct{}{}
	}
	for _, c := range cs {
		t.conns[c] = struct{}{}
	}
}

func (t *tunnelSet) remove(cs ...io.Closer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range cs {
		delete(t.conns, c)
	}
}

func (t *tunnelSet) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

func (t *tunnelSet) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for c := range t.conns {
		_ = c.Close()
	}
}

// Drain waits for upgraded connections (WebSockets) to finish until ctx is done,
// then closes the rest. proxy.Serve calls it after http.Server.Shutdown. It polls,
// like Shutdown does, because a tunnel may still open mid-drain (a handshake
// already in flight), which rules out a WaitGroup.
func (h *Handler) Drain(ctx context.Context) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for h.tunnels.len() > 0 {
		select {
		case <-ctx.Done():
			h.tunnels.closeAll()
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}

// upgradeType returns the protocol a request or response asks to switch to
// ("websocket"), or "" if it is not an upgrade.
func upgradeType(h http.Header) string {
	if !hasToken(h, "Connection", "upgrade") {
		return ""
	}
	return h.Get("Upgrade")
}

// serveUpgrade handles a 101 Switching Protocols response, for example a
// WebSocket handshake. It takes over (hijacks) the client connection and copies
// bytes in both directions until either side closes.
//
// Tunnels are tracked so a graceful shutdown waits for them (Handler.Drain).
func (h *Handler) serveUpgrade(w http.ResponseWriter, rc *http.ResponseController, r *http.Request,
	resp *http.Response, reqType, rid string) (int, string) {
	respType := upgradeType(resp.Header)
	if reqType == "" || !strings.EqualFold(respType, reqType) {
		_ = resp.Body.Close()
		h.writeError(w, rc, http.StatusBadGateway, rid)
		return http.StatusBadGateway, "upgrade_mismatch"
	}
	backConn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		h.writeError(w, rc, http.StatusBadGateway, rid)
		return http.StatusBadGateway, "upgrade_body"
	}
	defer backConn.Close()

	conn, brw, err := rc.Hijack()
	if err != nil { // e.g. HTTP/2 client: upgrades are an HTTP/1.1 feature
		h.writeError(w, rc, http.StatusBadGateway, rid)
		return http.StatusBadGateway, "upgrade_hijack"
	}
	defer conn.Close()
	h.tunnels.add(conn, backConn)
	defer h.tunnels.remove(conn, backConn)
	// No need to clear our per-write deadline: Hijack already resets the
	// connection's deadlines (net/http server.go hijackLocked), so a tunnel can
	// idle indefinitely. TestUpgradeTunnel idles past WriteTimeout to prove it.

	// Close the backend if the client request is cancelled (e.g. on server Close).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-r.Context().Done():
			_ = backConn.Close()
		case <-stop:
		}
	}()

	removeHopByHop(resp.Header)
	resp.Header.Set("Connection", "Upgrade")
	resp.Header.Set("Upgrade", respType)
	resp.Header.Add("Via", via(resp.ProtoMajor, resp.ProtoMinor))
	resp.Header.Set(HeaderRequestID, rid)
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		return http.StatusSwitchingProtocols, "upgrade_write"
	}
	if err := resp.Header.Write(brw); err != nil {
		return http.StatusSwitchingProtocols, "upgrade_write"
	}
	if _, err := brw.WriteString("\r\n"); err != nil || brw.Flush() != nil {
		return http.StatusSwitchingProtocols, "upgrade_write"
	}

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backConn, brw); done <- struct{}{} }() // brw may already hold client bytes
	go func() { _, _ = io.Copy(conn, backConn); done <- struct{}{} }()
	<-done // either side finished: the deferred Closes end the other copy
	return http.StatusSwitchingProtocols, ""
}

// isExtendedConnect reports an RFC 8441 WebSocket request: HTTP/2, method
// CONNECT, pseudo-header :protocol = websocket. Go's HTTP/2 server only
// accepts these when started with GODEBUG=http2xconnect=1 (golang/go#71128).
func isExtendedConnect(r *http.Request) bool {
	return r.ProtoMajor == 2 && r.Method == http.MethodConnect && strings.EqualFold(r.Header.Get(":protocol"), "websocket")
}

// toHTTP1WebSocket rewrites the outbound template from an RFC 8441 CONNECT
// into an RFC 6455 HTTP/1.1 upgrade, which is what backends speak. HTTP/2
// clients send no Sec-WebSocket-Key, so we make one and return it, to check the
// backend's answer.
func toHTTP1WebSocket(out *http.Request) string {
	out.Method = http.MethodGet
	out.Body, out.ContentLength = nil, 0
	delete(out.Header, ":protocol")
	var b [16]byte
	_, _ = rand.Read(b[:])
	key := base64.StdEncoding.EncodeToString(b[:])
	out.Header.Set("Sec-WebSocket-Key", key)
	if out.Header.Get("Sec-WebSocket-Version") == "" {
		out.Header.Set("Sec-WebSocket-Version", "13")
	}
	return key
}

// wsAccept is the Sec-WebSocket-Accept a backend must return for key (RFC 6455 §4.2.2).
func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // protocol-mandated
	return base64.StdEncoding.EncodeToString(sum[:])
}

// serveExtendedConnect completes an RFC 8441 WebSocket: the backend upgraded
// over HTTP/1.1, we answer the HTTP/2 client with 200 and then copy the
// client's request stream to the backend and the backend to the response
// stream, flushing every chunk.
func (h *Handler) serveExtendedConnect(w http.ResponseWriter, rc *http.ResponseController, r *http.Request,
	resp *http.Response, key, rid string) (int, string) {
	if !strings.EqualFold(upgradeType(resp.Header), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		_ = resp.Body.Close()
		h.writeError(w, rc, http.StatusBadGateway, rid)
		return http.StatusBadGateway, "upgrade_mismatch"
	}
	backConn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		h.writeError(w, rc, http.StatusBadGateway, rid)
		return http.StatusBadGateway, "upgrade_body"
	}
	defer backConn.Close()
	h.tunnels.add(backConn, r.Body)
	defer h.tunnels.remove(backConn, r.Body)

	removeHopByHop(resp.Header)
	delete(resp.Header, "Sec-Websocket-Accept") // an HTTP/1.1 detail; RFC 8441 responses carry no accept header
	hdr := w.Header()
	for k, vv := range resp.Header {
		hdr[k] = vv // e.g. Sec-WebSocket-Protocol, Sec-WebSocket-Extensions
	}
	hdr.Add("Via", via(resp.ProtoMajor, resp.ProtoMinor))
	hdr.Set(HeaderRequestID, rid)
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return http.StatusOK, "upgrade_write"
	}
	// No write deadline is ever set on this path (extendWrite runs only for
	// normal responses). On HTTP/2 a deadline resets the stream when it fires,
	// even while idle, so keep it that way. TestWebSocketOverHTTP2 idles past
	// WriteTimeout to prove it.

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backConn, r.Body); done <- struct{}{} }()
	go func() {
		bp := bufPool.Get().(*[]byte)
		defer bufPool.Put(bp)
		for {
			n, err := backConn.Read(*bp)
			if n > 0 {
				if _, werr := w.Write((*bp)[:n]); werr != nil || rc.Flush() != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	return http.StatusOK, ""
}
