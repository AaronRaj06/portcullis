package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/metrics"
)

// statusClientClosed is NGINX's convention for "the client went away first".
// It is only recorded in logs and metrics; nothing is sent to the client.
const statusClientClosed = 499

// Options configures a Handler. Router and TrustedProxies, if set, become the
// initial snapshot; otherwise the handler answers 503 until Swap is called.
type Options struct {
	Router         *Router
	Transport      http.RoundTripper // usually NewTransport(Limits)
	TrustedProxies []netip.Prefix    // peers whose X-Forwarded-* and X-Request-Id we believe
	Limits         Limits
	Logger         *slog.Logger     // nil: discard
	Metrics        *metrics.Metrics // nil: disabled
}

// Handler is the reverse proxy. It is safe for concurrent use and holds no locks
// on the request path.
type Handler struct {
	o       Options
	snap    atomic.Pointer[Snapshot]
	tunnels tunnelSet // upgraded connections, so Drain can wait for them
}

// Snapshot is one immutable, validated routing configuration. The handler loads
// it once per request, so a request that started on an old snapshot finishes on
// it even if a reload swaps in a new one meanwhile. Nothing in a published
// Snapshot may be modified.
type Snapshot struct {
	Router         *Router
	TrustedProxies []netip.Prefix
	Version        string // set by whoever pushed it (e.g. the controller); may be empty
	Hash           string // sha256 of the canonical config
	Generation     uint64 // increments on every successful swap in this process
}

// Swap publishes s for new requests and returns the previous snapshot.
func (h *Handler) Swap(s *Snapshot) *Snapshot { return h.snap.Swap(s) }

// Current returns the snapshot new requests will use (nil before the first Swap).
func (h *Handler) Current() *Snapshot { return h.snap.Load() }

// New validates o and returns a Handler.
func New(o Options) (*Handler, error) {
	if o.Transport == nil {
		return nil, errors.New("proxy: Transport is required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	h := &Handler{o: o}
	if o.Router != nil {
		h.snap.Store(&Snapshot{Router: o.Router, TrustedProxies: o.TrustedProxies, Generation: 1})
	}
	return h, nil
}

// reqState is what the access log and metrics record about one request.
type reqState struct {
	route, pool, backend, errKind string
	status, attempts              int
	panicked                      bool
	written                       int64
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	snap := h.snap.Load() // one load: this request sees one consistent config
	var trustedNets []netip.Prefix
	if snap != nil {
		trustedNets = snap.TrustedProxies
	}
	peer := clientAddr(r.RemoteAddr)
	trusted := isTrusted(peer, trustedNets)
	// The original client: the peer itself, or behind trusted proxies the first
	// untrusted address in X-Forwarded-For. Used for hashing and logs.
	client := realClient(r.Header, peer, trustedNets).String()
	rid := requestID(r.Header.Get(HeaderRequestID), trusted)
	rc := http.NewResponseController(w)

	var st reqState
	if h.o.Metrics != nil {
		h.o.Metrics.Start()
	}
	// Runs even when we abort with http.ErrAbortHandler.
	defer func() {
		d := time.Since(start)
		if h.o.Metrics != nil {
			h.o.Metrics.Done(st.route, r.Method, st.status, st.errKind, d)
		}
		h.o.Logger.LogAttrs(context.Background(), slog.LevelInfo, "request",
			slog.String("request_id", rid), slog.String("client", client), slog.String("peer", peer.String()),
			slog.String("method", r.Method), slog.String("host", r.Host), slog.String("path", r.URL.Path),
			slog.String("route", st.route), slog.String("pool", st.pool), slog.String("backend", st.backend),
			slog.Int("attempts", st.attempts), slog.Bool("panic", st.panicked),
			slog.Int("status", st.status), slog.Int64("bytes", st.written), slog.Duration("duration", d),
			slog.String("error", st.errKind))
	}()

	if limit := h.o.Limits.MaxBodyBytes; limit > 0 && r.ContentLength > limit {
		st.status, st.errKind = http.StatusRequestEntityTooLarge, "body_too_large"
		h.writeError(w, rc, st.status, rid)
		return
	}
	if snap == nil {
		st.status, st.errKind = http.StatusServiceUnavailable, "not_ready"
		h.writeError(w, rc, st.status, rid)
		return
	}
	route := snap.Router.Match(r)
	if route == nil {
		st.status, st.errKind = http.StatusNotFound, "no_route"
		h.writeError(w, rc, st.status, rid)
		return
	}
	st.route = route.Name
	pool := route.PickPool()
	st.pool = pool.Name
	if pool.Budget != nil {
		pool.Budget.Deposit()
	}

	upgrade := upgradeType(r.Header)
	tmpl, body := h.outbound(w, r, rc, peer, trusted, rid, upgrade)
	transport := pool.Transport
	if transport == nil {
		transport = h.o.Transport
	}
	// Only requests that may be retried pay for the write tracing.
	retryable := pool.Retries > 0 && pool.Budget != nil && idempotent(r.Method)

	var triedBuf [4]*lb.Backend
	tried := triedBuf[:0]
	for {
		b, panicked, err := pool.Pick(r, client, tried)
		if panicked {
			st.panicked = true
			if h.o.Metrics != nil {
				h.o.Metrics.Panic(pool.Name)
			}
		}
		if err != nil {
			if st.attempts == 0 { // otherwise keep the previous attempt's status
				st.status, st.errKind = http.StatusServiceUnavailable, "no_backend"
			}
			h.writeError(w, rc, st.status, rid)
			return
		}
		st.backend = b.Addr
		st.attempts++
		out, wrote := forBackend(tmpl, pool.Scheme, b, retryable)

		b.Acquire()
		resp, err := transport.RoundTrip(out)
		if err == nil {
			defer b.Release()
			h.report(pool, b, gatewayFailure(resp.StatusCode), metrics.StatusClass(resp.StatusCode))
			if resp.StatusCode == http.StatusSwitchingProtocols {
				st.status, st.errKind = h.serveUpgrade(w, rc, r, resp, upgrade, rid)
				return
			}
			h.serveResponse(w, rc, r, resp, rid, &st)
			return
		}
		b.Release()

		st.status, st.errKind = classify(r.Context(), body, err)
		upstreamFault := st.status == http.StatusBadGateway || st.status == http.StatusGatewayTimeout
		if upstreamFault {
			h.report(pool, b, true, st.errKind)
		}
		// Retry only if: the method is idempotent, nothing reached the backend (no
		// headers written, no body read), attempts remain, and the budget allows it.
		if upstreamFault && retryable && !wrote.Load() && body.untouched() &&
			len(tried) < pool.Retries && pool.Budget.Withdraw() {
			tried = append(tried, b)
			if h.o.Metrics != nil {
				h.o.Metrics.Retry(pool.Name)
			}
			continue
		}
		if st.status != statusClientClosed {
			h.writeError(w, rc, st.status, rid)
		}
		return
	}
}

// report feeds passive health and metrics.
func (h *Handler) report(pool *lb.Pool, b *lb.Backend, failed bool, result string) {
	ejected := pool.Report(b, failed)
	if h.o.Metrics != nil {
		h.o.Metrics.Attempt(pool.Name, b.Addr, result)
		if ejected {
			h.o.Metrics.Ejected(pool.Name, b.Addr)
		}
	}
	if ejected {
		h.o.Logger.Warn("backend ejected", "pool", pool.Name, "backend", b.Addr, "for", pool.EjectFor.String())
	}
}

// gatewayFailure is a response that says the backend (or something behind it)
// is unwell, as opposed to an application error like 500.
func gatewayFailure(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// idempotent methods may be sent twice without changing the outcome (RFC 9110 §9.2.2).
func idempotent(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

func (h *Handler) serveResponse(w http.ResponseWriter, rc *http.ResponseController, r *http.Request, resp *http.Response, rid string, st *reqState) {
	defer resp.Body.Close()
	st.status = resp.StatusCode
	removeHopByHop(resp.Header)
	hdr := w.Header()
	for k, vv := range resp.Header {
		hdr[k] = vv
	}
	hdr.Add("Via", via(resp.ProtoMajor, resp.ProtoMinor))
	hdr.Set(HeaderRequestID, rid)
	h.extendWrite(rc)
	w.WriteHeader(resp.StatusCode)

	var err error
	st.written, err = h.copyBody(w, rc, resp)
	if err != nil {
		if r.Context().Err() != nil {
			st.errKind = "client_closed"
			return
		}
		// Headers are already sent, so we cannot change the status. Abort the
		// connection so the client sees a truncated response, not a complete-looking one.
		st.errKind = "body_copy"
		panic(http.ErrAbortHandler)
	}
	for k, vv := range resp.Trailer {
		hdr[http.TrailerPrefix+k] = vv
	}
}

// outbound builds the request template shared by all attempts. It carries the
// client's context, so a client disconnect cancels the upstream request too.
func (h *Handler) outbound(w http.ResponseWriter, r *http.Request, rc *http.ResponseController,
	client netip.Addr, trusted bool, rid, upgrade string) (*http.Request, *clientBody) {
	hdr := r.Header.Clone()
	te := hasToken(hdr, "Te", "trailers")
	removeHopByHop(hdr)
	if te {
		hdr.Set("Te", "trailers")
	}
	if upgrade != "" { // Connection and Upgrade are hop-by-hop, but an upgrade must be re-requested on the next hop
		hdr.Set("Connection", "Upgrade")
		hdr.Set("Upgrade", upgrade)
	}
	setForwarded(hdr, r, client, trusted)
	hdr.Set(HeaderRequestID, rid)
	if _, ok := hdr["User-Agent"]; !ok {
		hdr["User-Agent"] = []string{""} // stop net/http adding "Go-http-client/1.1"
	}

	out := &http.Request{
		Method:        r.Method,
		URL:           &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery},
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

// forBackend points a copy of tmpl at backend b. If track is set, wrote becomes
// true once request headers have been written to the backend; after that, a
// retry could make the backend process the request twice.
func forBackend(tmpl *http.Request, scheme string, b *lb.Backend, track bool) (*http.Request, *atomic.Bool) {
	wrote := new(atomic.Bool)
	ctx := tmpl.Context()
	if track {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	}
	out := tmpl.WithContext(ctx)
	u := *tmpl.URL
	u.Scheme, u.Host = scheme, b.Addr
	out.URL = &u
	return out, wrote
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
//   - remembers the first read error so classify can blame the client, not the upstream,
//   - ignores Close, so a failed attempt that never read the body leaves it
//     usable for a retry. net/http closes the real body after the handler returns.
//
// The Transport reads it from its own goroutine, hence the mutex and atomic.
type clientBody struct {
	r    io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	want int64 // Content-Length, or -1
	got  atomic.Int64

	mu  sync.Mutex
	err error
}

func (b *clientBody) Read(p []byte) (int, error) {
	if b.idle > 0 {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	}
	n, err := b.r.Read(p)
	got := b.got.Add(int64(n))
	// Clear only on success. After an error the expired deadline must stay, or
	// the server's drain of the unread body would block on a stalled client.
	if b.idle > 0 && (err == io.EOF || (err == nil && b.want > 0 && got >= b.want)) {
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

func (b *clientBody) Close() error { return nil }

func (b *clientBody) failure() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// untouched reports whether no body bytes were consumed, so the body can be resent.
func (b *clientBody) untouched() bool { return b == nil || (b.got.Load() == 0 && b.failure() == nil) }
