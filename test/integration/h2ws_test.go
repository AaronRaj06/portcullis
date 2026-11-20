package integration

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // RFC 6455 handshake, not security
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// wsBackend is an HTTP/1.1 WebSocket endpoint that performs a real RFC 6455
// handshake (or a forged one, if lie is set) and then echoes bytes.
func wsBackend(t *testing.T, lie bool) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
			http.Error(w, "not a websocket handshake", http.StatusBadRequest)
			return
		}
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec
		accept := base64.StdEncoding.EncodeToString(sum[:])
		if lie {
			accept = "forged"
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(conn, brw)
	}))
	t.Cleanup(s.Close)
	return strings.TrimPrefix(s.URL, "http://")
}

// h2Stream is a minimal raw HTTP/2 client for one stream, built on x/net's
// Framer and HPACK encoder. Neither net/http's Transport nor x/net's (which now
// routes through net/http) lets a caller send the :protocol pseudo-header, so
// this speaks the frames a browser sends for an RFC 8441 WebSocket.
type h2Stream struct {
	conn    *tls.Conn
	fr      *http2.Framer
	wmu     sync.Mutex
	status  chan int
	data    chan []byte
	pending []byte
}

// dialH2 opens a TLS h2 connection to srv and sends a CONNECT on stream 1.
// protocol "" sends a plain CONNECT (no :scheme, :path or :protocol).
func dialH2(t *testing.T, srv *httptest.Server, protocol, path string) (*h2Stream, int) {
	t.Helper()
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.NextProtos = []string{"h2"}
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if conn.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("server did not negotiate h2")
	}
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	s := &h2Stream{conn: conn, fr: http2.NewFramer(conn, conn), status: make(chan int, 1), data: make(chan []byte, 64)}
	s.fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := s.fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	// The server must advertise RFC 8441 support before we may use :protocol.
	for {
		f, err := s.fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
			if v, _ := sf.Value(http2.SettingEnableConnectProtocol); v != 1 && protocol != "" {
				t.Fatal("server does not advertise SETTINGS_ENABLE_CONNECT_PROTOCOL")
			}
			if err := s.fr.WriteSettingsAck(); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	fields := [][2]string{{":method", "CONNECT"}, {":authority", srv.Listener.Addr().String()}}
	if protocol != "" {
		fields = append(fields, [2]string{":protocol", protocol}, [2]string{":scheme", "https"}, [2]string{":path", path},
			[2]string{"sec-websocket-version", "13"})
	}
	for _, f := range fields {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := s.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	go s.readLoop()
	select {
	case code := <-s.status:
		return s, code
	case <-time.After(3 * time.Second):
		t.Fatal("no response headers")
	}
	return nil, 0
}

func (s *h2Stream) readLoop() {
	defer close(s.data)
	for {
		f, err := s.fr.ReadFrame()
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *http2.MetaHeadersFrame:
			if f.StreamID == 1 {
				code, _ := strconv.Atoi(f.PseudoValue("status"))
				select {
				case s.status <- code:
				default:
				}
			}
		case *http2.DataFrame:
			if n := len(f.Data()); n > 0 {
				s.data <- append([]byte(nil), f.Data()...)
				s.wmu.Lock() // give the flow-control window back
				_ = s.fr.WriteWindowUpdate(0, uint32(n))
				_ = s.fr.WriteWindowUpdate(1, uint32(n))
				s.wmu.Unlock()
			}
		case *http2.SettingsFrame:
			if !f.IsAck() {
				s.wmu.Lock()
				_ = s.fr.WriteSettingsAck()
				s.wmu.Unlock()
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				s.wmu.Lock()
				_ = s.fr.WritePing(true, f.Data)
				s.wmu.Unlock()
			}
		case *http2.RSTStreamFrame, *http2.GoAwayFrame:
			return
		}
	}
}

func (s *h2Stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return len(p), s.fr.WriteData(1, false, p)
}

func (s *h2Stream) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		d, ok := <-s.data
		if !ok {
			return 0, io.EOF
		}
		s.pending = d
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// TestWebSocketOverHTTP2 needs GODEBUG=http2xconnect=1, which Go reads once at
// start-up, so the test re-runs itself in a subprocess with it set.
func TestWebSocketOverHTTP2(t *testing.T) {
	if !strings.Contains(os.Getenv("GODEBUG"), "http2xconnect=1") {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWebSocketOverHTTP2$", "-test.count=1", "-test.v") //nolint:gosec // re-running our own test binary
		cmd.Env = append(os.Environ(), "GODEBUG=http2xconnect=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestWebSocketOverHTTP2") {
			t.Fatalf("subprocess with GODEBUG=http2xconnect=1 failed: %v\n%s", err, out)
		}
		return
	}

	start := func(backendAddr string) *httptest.Server {
		limits := proxy.DefaultLimits
		limits.WriteTimeout = 200 * time.Millisecond
		h, err := proxy.New(proxy.Options{
			Router:    mustRouter(t, proxy.Route{Name: "ws", Pool: newPool("ws", "http", lb.NewRoundRobin, backendAddr)}),
			Transport: proxy.NewTransport(limits), Limits: limits,
		})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewUnstartedServer(h)
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv
	}
	t.Run("echo through the tunnel, surviving idle time", func(t *testing.T) {
		st, code := dialH2(t, start(wsBackend(t, false)), "websocket", "/chat")
		if code != 200 {
			t.Fatalf("handshake status %d, want 200", code)
		}
		for i, msg := range []string{"hello over h2", "still here after idling"} {
			if i > 0 {
				time.Sleep(500 * time.Millisecond) // > WriteTimeout: an idle tunnel must not be reset
			}
			if _, err := io.WriteString(st, msg); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(msg))
			_ = st.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.ReadFull(st, buf); err != nil || string(buf) != msg {
				t.Fatalf("echo %q: got %q, %v", msg, buf, err)
			}
		}
	})

	t.Run("forged Sec-WebSocket-Accept is rejected", func(t *testing.T) {
		if _, code := dialH2(t, start(wsBackend(t, true)), "websocket", "/chat"); code != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", code)
		}
	})

	t.Run("plain CONNECT is refused", func(t *testing.T) {
		if _, code := dialH2(t, start(wsBackend(t, false)), "", ""); code != http.StatusMethodNotAllowed {
			t.Fatalf("status %d, want 405", code)
		}
	})
}
