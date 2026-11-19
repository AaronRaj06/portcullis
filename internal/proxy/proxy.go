package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AaronRaj06/portcullis/internal/metrics"
)

// statusClientClosed is NGINX's convention for "the client went away first".
// It is only recorded in logs and metrics; nothing is sent to the client.
const statusClientClosed = 499

// Options configures a Handler.
type Options struct {
	Upstream       *url.URL          // absolute http(s) URL with no path (Phase 1: one upstream)
	Transport      http.RoundTripper // usually NewTransport(Limits)
	TrustedProxies []netip.Prefix    // peers whose X-Forwarded-* and X-Request-Id we believe
	Limits         Limits
	Logger         *slog.Logger     // nil: discard
	Metrics        *metrics.Metrics // nil: disabled
}

// Handler is the reverse proxy. It is safe for concurrent use and holds no locks
// on the request path.
type Handler struct {
	o Options
}

// New validates o and returns a Handler.
func New(o Options) (*Handler, error) {
	u := o.Upstream
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("proxy: upstream must be an absolute http or https URL")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("proxy: upstream %q: a path or query is not supported yet", u)
	}
	if o.Transport == nil {
		return nil, errors.New("proxy: Transport is required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{o: o}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	client := clientAddr(r.RemoteAddr)
	trusted := isTrusted(client, h.o.TrustedProxies)
	rid := requestID(r.Header.Get(HeaderRequestID), trusted)
	rc := http.NewResponseController(w)

	var (
		status  int
		written int64
		errKind string
	)
	if h.o.Metrics != nil {
		h.o.Metrics.Start()
	}
	// Runs even when we abort with http.ErrAbortHandler below.
	defer func() {
		d := time.Since(start)
		if h.o.Metrics != nil {
			h.o.Metrics.Done(r.Method, status, errKind, d)
		}
		h.o.Logger.LogAttrs(context.Background(), slog.LevelInfo, "request",
			slog.String("request_id", rid), slog.String("client", client.String()),
			slog.String("method", r.Method), slog.String("host", r.Host), slog.String("path", r.URL.Path),
			slog.Int("status", status), slog.Int64("bytes", written), slog.Duration("duration", d),
			slog.String("error", errKind))
	}()

	if limit := h.o.Limits.MaxBodyBytes; limit > 0 && r.ContentLength > limit {
		status, errKind = http.StatusRequestEntityTooLarge, "body_too_large"
		h.writeError(w, rc, status, rid)
		return
	}

	out, body := h.outbound(w, r, rc, client, trusted, rid)
	resp, err := h.o.Transport.RoundTrip(out)
	if err != nil {
		status, errKind = classify(r.Context(), body, err)
		if status != statusClientClosed {
			h.writeError(w, rc, status, rid)
		}
		return
	}
	defer resp.Body.Close()
	status = resp.StatusCode

	removeHopByHop(resp.Header)
	hdr := w.Header()
	for k, vv := range resp.Header {
		hdr[k] = vv
	}
	hdr.Add("Via", via(resp.ProtoMajor, resp.ProtoMinor))
	hdr.Set(HeaderRequestID, rid)
	h.extendWrite(rc)
	w.WriteHeader(resp.StatusCode)

	written, err = h.copyBody(w, rc, resp)
	if err != nil {
		if r.Context().Err() != nil {
			errKind = "client_closed"
			return
		}
		// Headers are already sent, so we cannot change the status. Abort the
		// connection so the client sees a truncated response, not a complete-looking one.
		errKind = "body_copy"
		panic(http.ErrAbortHandler)
	}
	for k, vv := range resp.Trailer {
		hdr[http.TrailerPrefix+k] = vv
	}
}

// outbound builds the upstream request. It shares the client's context, so a
// client disconnect cancels the upstream request too.
func (h *Handler) outbound(w http.ResponseWriter, r *http.Request, rc *http.ResponseController,
	client netip.Addr, trusted bool, rid string) (*http.Request, *clientBody) {
	hdr := r.Header.Clone()
	te := wantsTrailers(hdr)
	removeHopByHop(hdr)
	if te {
		hdr.Set("Te", "trailers")
	}
	setForwarded(hdr, r, client, trusted)
	hdr.Set(HeaderRequestID, rid)
	if _, ok := hdr["User-Agent"]; !ok {
		hdr["User-Agent"] = []string{""} // stop net/http adding "Go-http-client/1.1"
	}

	out := &http.Request{
		Method: r.Method,
		URL: &url.URL{
			Scheme: h.o.Upstream.Scheme, Host: h.o.Upstream.Host,
			Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery,
		},
		Header:        hdr,
		Host:          r.Host,
		ContentLength: r.ContentLength,
	}
	var body *clientBody
	if r.ContentLength != 0 { // > 0 known length, -1 chunked
		body = &clientBody{r: r.Body, rc: rc, idle: h.o.Limits.BodyReadTimeout, want: r.ContentLength}
		if limit := h.o.Limits.MaxBodyBytes; limit > 0 {
			body.r = http.MaxBytesReader(w, r.Body, limit)
		}
		out.Body = body
	}
	return out.WithContext(r.Context()), body
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// copyBody streams the upstream body to the client through a pooled buffer.
// Streaming responses (SSE, or no Content-Length) are flushed after every read,
// so events reach the client as soon as the upstream sends them.
func (h *Handler) copyBody(w http.ResponseWriter, rc *http.ResponseController, resp *http.Response) (int64, error) {
	flush := resp.ContentLength == -1 || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	var total int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			h.extendWrite(rc)
			m, werr := w.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
			if flush {
				if err := rc.Flush(); err != nil {
					return total, err
				}
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// extendWrite pushes the client write deadline forward before each write. The
// limit is on how long one write may block (a stalled reader), not on the total
// response time, so long-lived streams are not cut off.
func (h *Handler) extendWrite(rc *http.ResponseController) {
	if d := h.o.Limits.WriteTimeout; d > 0 {
		_ = rc.SetWriteDeadline(time.Now().Add(d))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, rc *http.ResponseController, status int, rid string) {
	h.extendWrite(rc)
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set(HeaderRequestID, rid)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}{http.StatusText(status), rid})
}

// classify maps a RoundTrip error to a status code and a metrics label. The
// order matters: a failure caused by the client must not be blamed on the upstream.
// A body read error comes first because net/http cancels the request context
// when a body read fails, so checking ctx first would hide it.
func classify(ctx context.Context, body *clientBody, err error) (int, string) {
	var ne net.Error
	if berr := body.failure(); berr != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(berr, &mbe):
			return http.StatusRequestEntityTooLarge, "body_too_large"
		case errors.As(berr, &ne) && ne.Timeout():
			return http.StatusRequestTimeout, "client_body_timeout"
		default:
			return http.StatusBadRequest, "client_body"
		}
	}
	if ctx.Err() != nil {
		return statusClientClosed, "client_closed"
	}
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return http.StatusGatewayTimeout, "upstream_timeout"
	}
	return http.StatusBadGateway, "upstream_error"
}

// clientBody wraps the client's request body. It
//   - applies an idle deadline to each Read (a client that stops sending is cut off),
//   - clears that deadline once the body is fully read (successfully), because net/http starts a
//     background read on the connection then, and a deadline firing there would
//     cancel the whole request while we wait for the upstream,
//   - remembers the first read error so classify can blame the client, not the upstream.
//
// The Transport reads it from its own goroutine, hence the mutex.
type clientBody struct {
	r    io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	want int64 // Content-Length, or -1
	got  int64

	mu  sync.Mutex
	err error
}

func (b *clientBody) Read(p []byte) (int, error) {
	if b.idle > 0 {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	}
	n, err := b.r.Read(p)
	b.got += int64(n)
	// Clear only on success. After an error the expired deadline must stay, or
	// Close (which drains the unread body) would block on a stalled client.
	if b.idle > 0 && (err == io.EOF || (err == nil && b.want > 0 && b.got >= b.want)) {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	if err != nil && err != io.EOF {
		b.mu.Lock()
		if b.err == nil {
			b.err = err
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *clientBody) Close() error { return b.r.Close() }

func (b *clientBody) failure() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}
