package proxy

import (
	"io"
	"net/http"
	"strings"
)

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
// Hijacked connections are not tracked by http.Server.Shutdown, so they do
// not hold up a drain. They are closed when the process exits.
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
