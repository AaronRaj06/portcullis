package proxy

import (
	"context"
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
