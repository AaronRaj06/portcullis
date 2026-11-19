// Package integration runs the real proxy server on loopback against real backends.
package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AaronRaj06/portcullis/internal/proxy"
)

// startProxy runs a proxy in front of upstream. mod may adjust options before start.
// The returned shutdown triggers a graceful drain and returns Serve's result.
func startProxy(t *testing.T, upstream string, mod func(*proxy.Options)) (base string, shutdown func() error) {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	o := proxy.Options{Upstream: u, Limits: proxy.DefaultLimits}
	if mod != nil {
		mod(&o)
	}
	tr := proxy.NewTransport(o.Limits)
	o.Transport = tr
	h, err := proxy.New(o)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- proxy.Serve(ctx, proxy.NewServer(h, o.Limits, nil), ln, 5*time.Second) }()
	var once sync.Once
	var serr error
	shutdown = func() error {
		once.Do(func() { cancel(); serr = <-errc; tr.CloseIdleConnections() })
		return serr
	}
	t.Cleanup(func() { _ = shutdown() })
	return "http://" + ln.Addr().String(), shutdown
}

func backend(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// wait fails the test if ch is not closed or sent to within d.
func wait[T any](t *testing.T, ch <-chan T, d time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for %s", d, what)
	}
	var zero T
	return zero
}

func TestHeaders(t *testing.T) {
	type seen struct {
		Header http.Header
		Host   string
	}
	be := backend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "X-Resp-Drop")
		w.Header().Set("X-Resp-Drop", "1")
		w.Header().Set("X-Resp-Keep", "1")
		_ = json.NewEncoder(w).Encode(seen{r.Header, r.Host})
	})

	send := func(t *testing.T, base string) (seen, http.Header) {
		req, _ := http.NewRequest(http.MethodGet, base+"/h", nil)
		req.Host = "app.example.com"
		req.Header.Set("Connection", "X-Drop")
		req.Header.Set("X-Drop", "1")
		req.Header.Set("Keep-Alive", "timeout=5")
		req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
		req.Header.Set("X-Forwarded-For", "1.2.3.4")
		req.Header.Set("X-Forwarded-Host", "spoofed.example")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Forwarded", "for=1.2.3.4")
		req.Header.Set("X-Request-Id", "client-id-1")
		req.Header.Set("X-Keep", "yes")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var s seen
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
		return s, resp.Header
	}

	t.Run("untrusted client", func(t *testing.T) {
		base, _ := startProxy(t, be.URL, nil)
		s, respHdr := send(t, base)
		h := s.Header
		for _, k := range []string{"X-Drop", "Keep-Alive", "Proxy-Authorization", "Forwarded"} {
			if v := h.Get(k); v != "" {
				t.Errorf("upstream got %s: %q, want it stripped", k, v)
			}
		}
		check := map[string]string{
			"X-Keep":            "yes",
			"X-Forwarded-For":   "127.0.0.1",       // spoofed 1.2.3.4 dropped
			"X-Forwarded-Host":  "app.example.com", // spoofed host dropped
			"X-Forwarded-Proto": "http",
			"Via":               "1.1 portcullis",
		}
		for k, want := range check {
			if got := h.Get(k); got != want {
				t.Errorf("upstream %s = %q, want %q", k, got, want)
			}
		}
		if s.Host != "app.example.com" {
			t.Errorf("upstream Host = %q, want original host", s.Host)
		}
		rid := h.Get("X-Request-Id")
		if rid == "client-id-1" || len(rid) != 32 {
			t.Errorf("untrusted client's request ID was used or ID malformed: %q", rid)
		}
		if got := respHdr.Get("X-Request-Id"); got != rid {
			t.Errorf("response request ID %q != upstream request ID %q", got, rid)
		}
		if respHdr.Get("X-Resp-Drop") != "" || respHdr.Get("X-Resp-Keep") != "1" {
			t.Errorf("response hop-by-hop handling wrong: %v", respHdr)
		}
		if !strings.Contains(strings.Join(respHdr.Values("Via"), ","), "1.1 portcullis") {
			t.Errorf("response Via = %q", respHdr.Values("Via"))
		}
	})

	t.Run("trusted proxy", func(t *testing.T) {
		base, _ := startProxy(t, be.URL, func(o *proxy.Options) {
			o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		})
		s, _ := send(t, base)
		check := map[string]string{
			"X-Forwarded-For":   "1.2.3.4, 127.0.0.1",
			"X-Forwarded-Host":  "spoofed.example",
			"X-Forwarded-Proto": "https",
			"X-Request-Id":      "client-id-1",
		}
		for k, want := range check {
			if got := s.Header.Get(k); got != want {
				t.Errorf("upstream %s = %q, want %q", k, got, want)
			}
		}
	})
}

func TestStreamingResponseIsFlushed(t *testing.T) {
	release := make(chan struct{})
	be := backend(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release // the second event only exists after the client saw the first
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	base, _ := startProxy(t, be.URL, nil)
	resp, err := http.Get(base + "/events") //nolint:bodyclose // closed by the defer below; read in a goroutine
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines <- sc.Text()
			}
		}
		close(lines)
	}()
	if got := wait(t, lines, 2*time.Second, "first event (proxy is buffering?)"); got != "data: one" {
		t.Fatalf("first event = %q", got)
	}
	close(release)
	if got := wait(t, lines, 2*time.Second, "second event"); got != "data: two" {
		t.Fatalf("second event = %q", got)
	}
}

func TestStreamingRequestBody(t *testing.T) {
	gotFirst := make(chan struct{})
	be := backend(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		close(gotFirst) // upstream has bytes while the client is still sending
		rest, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, string(buf)+string(rest))
	})
	base, _ := startProxy(t, be.URL, nil)
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, "part1")
		select {
		case <-gotFirst:
			_, _ = io.WriteString(pw, "part2")
			_ = pw.Close()
		case <-time.After(2 * time.Second):
			pw.CloseWithError(errors.New("upstream never saw the first chunk: request body is buffered"))
		}
	}()
	resp, err := http.Post(base+"/upload", "application/octet-stream", pr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "part1part2" {
		t.Fatalf("status %d body %q", resp.StatusCode, b)
	}
}

func TestClientCancelPropagatesUpstream(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	be := backend(t, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	base, _ := startProxy(t, be.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/slow", nil)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	wait(t, started, 2*time.Second, "upstream to receive request")
	cancel()
	wait(t, cancelled, 2*time.Second, "upstream to see cancellation")
}

func decodeError(t *testing.T, resp *http.Response) {
	t.Helper()
	var e struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if e.RequestID == "" || e.RequestID != resp.Header.Get("X-Request-Id") {
		t.Errorf("request_id %q, header %q: want equal and non-empty", e.RequestID, resp.Header.Get("X-Request-Id"))
	}
}

func TestUpstreamDownIs502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close() // nothing listens here now: connect is refused
	base, _ := startProxy(t, dead, nil)
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	decodeError(t, resp)
}

func TestSlowUpstreamIs504(t *testing.T) {
	be := backend(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	base, _ := startProxy(t, be.URL, func(o *proxy.Options) { o.Limits.ResponseHeaderTimeout = 100 * time.Millisecond })
	start := time.Now()
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("504 took %s, timeout not enforced", el)
	}
	decodeError(t, resp)
}

func TestBodyTooLargeIs413(t *testing.T) {
	be := backend(t, func(_ http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	base, _ := startProxy(t, be.URL, func(o *proxy.Options) { o.Limits.MaxBodyBytes = 10 })
	body := strings.Repeat("x", 100)
	t.Run("declared length", func(t *testing.T) {
		resp, err := http.Post(base+"/", "text/plain", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})
	t.Run("chunked", func(t *testing.T) {
		// io.MultiReader hides the length, so the client sends chunked.
		resp, err := http.Post(base+"/", "text/plain", io.MultiReader(strings.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})
}

// A client that stops sending its body mid-way is cut off with 408, and the
// upstream is not blamed (not 502/504).
func TestStalledClientBodyIs408(t *testing.T) {
	be := backend(t, func(_ http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	base, _ := startProxy(t, be.URL, func(o *proxy.Options) { o.Limits.BodyReadTimeout = 100 * time.Millisecond })
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 10\r\n\r\nab")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("no response to stalled body: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 408") {
		t.Fatalf("status line = %q, want 408", line)
	}
}

// Regression: once the body is fully read, the idle deadline must be cleared.
// Otherwise net/http's background read hits it while we wait for a slow
// upstream and cancels the request.
func TestSlowUpstreamAfterBodyIsNotCancelled(t *testing.T) {
	be := backend(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		time.Sleep(400 * time.Millisecond) // well past BodyReadTimeout
		_, _ = w.Write(b)
	})
	base, _ := startProxy(t, be.URL, func(o *proxy.Options) { o.Limits.BodyReadTimeout = 100 * time.Millisecond })
	for _, name := range []string{"declared length", "chunked"} {
		t.Run(name, func(t *testing.T) {
			var body io.Reader = strings.NewReader("hello")
			if name == "chunked" {
				body = io.MultiReader(body)
			}
			resp, err := http.Post(base+"/", "text/plain", body)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 || string(b) != "hello" {
				t.Fatalf("status %d body %q", resp.StatusCode, b)
			}
		})
	}
}

func TestSlowlorisHeadersAreCutOff(t *testing.T) {
	be := backend(t, func(http.ResponseWriter, *http.Request) {})
	base, _ := startProxy(t, be.URL, func(o *proxy.Options) { o.Limits.ReadHeaderTimeout = 100 * time.Millisecond })
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n") // never finish the header
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = io.ReadAll(conn) // returns when the server closes the connection
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("server kept a half-sent request open: no slowloris protection")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("connection closed after %s, want about 100ms", el)
	}
}

func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	started := make(chan struct{})
	be := backend(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})
	base, shutdown := startProxy(t, be.URL, nil)

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/")
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		res <- result{string(b), err}
	}()
	wait(t, started, 2*time.Second, "request to reach upstream")

	shutDone := make(chan error, 1)
	go func() { shutDone <- shutdown() }()

	// New connections are refused while the in-flight request finishes.
	time.Sleep(50 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), 200*time.Millisecond); err == nil {
		c.Close()
		t.Error("new connection accepted during shutdown")
	}
	r := wait(t, res, 3*time.Second, "in-flight request")
	if r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request: body %q err %v, want drained to completion", r.body, r.err)
	}
	if err := wait(t, shutDone, 3*time.Second, "shutdown"); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
