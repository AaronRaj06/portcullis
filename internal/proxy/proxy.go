package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AaronRaj06/portcullis/internal/lb"
	"github.com/AaronRaj06/portcullis/internal/metrics"
	"github.com/AaronRaj06/portcullis/internal/policy"
	"github.com/AaronRaj06/portcullis/internal/ratelimit"
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
	Router         *Router       // the -listen address
	Ports          map[int]*Port // every other listener
	TrustedProxies []netip.Prefix
	// Pools holds every configured pool by name, for route_to. A pool may be
	// reachable only through a policy, so it is not necessarily in Router.
	Pools map[string]*lb.Pool
	// Limiters holds rate_limit state by call site, so a reload that keeps a
	// call site unchanged keeps its counters (config.Compile hands them over).
	Limiters   map[string]*ratelimit.Limiter
	Version    string // set by whoever pushed it (e.g. the controller); may be empty
	Hash       string // sha256 of the canonical config
	Generation uint64 // increments on every successful swap in this process
}

// AllPools returns every pool: those used by routes, plus any only reachable via route_to.
func (s *Snapshot) AllPools() []*lb.Pool {
	ps := s.Router.Pools() // the other ports' pools are all in s.Pools
	for _, p := range s.Pools {
		if !slices.Contains(ps, p) {
			ps = append(slices.Clip(ps), p)
		}
	}
	return ps
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
	tags                          []string          // from policy tag(...)
	progs                         []*policy.Program // the route's policies, for the response phase
	penv                          *policy.Env       // nil when the route has no policies
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.serve(w, r, 0) }

// serve handles a request that arrived on port (0: the -listen address).
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, port int) {
	start := time.Now()
	snap := h.snap.Load() // one load: this request sees one consistent config
	var trustedNets []netip.Prefix
	if snap != nil {
		trustedNets = snap.TrustedProxies
	}
	peer := clientAddr(r.RemoteAddr)
	peerStr := r.RemoteAddr
	if peer.IsValid() {
		peerStr = peer.String()
	}
	trusted := isTrusted(peer, trustedNets)
	// The original client: the peer itself, or behind trusted proxies the first
	// untrusted address in X-Forwarded-For. Used for hashing and logs.
	client := peerStr
	clientIP := realClient(r.Header, peer, trustedNets)
	if clientIP != peer {
		client = clientIP.String()
	}
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
		if !h.o.Logger.Enabled(context.Background(), slog.LevelInfo) {
			return // building the attributes allocates; skip it when access logs are off
		}
		h.o.Logger.LogAttrs(context.Background(), slog.LevelInfo, "request",
			slog.String("request_id", rid), slog.String("client", client), slog.String("peer", peerStr),
			slog.String("method", r.Method), slog.String("host", r.Host), slog.String("path", r.URL.Path),
			slog.String("route", st.route), slog.String("pool", st.pool), slog.String("backend", st.backend),
			slog.Int("attempts", st.attempts), slog.Bool("panic", st.panicked),
			slog.Int("status", st.status), slog.Int64("bytes", st.written), slog.Duration("duration", d),
			slog.String("error", st.errKind), slog.Any("tags", st.tags))
	}()

	if limit := h.o.Limits.MaxBodyBytes; limit > 0 && r.ContentLength > limit {
		st.status, st.errKind = http.StatusRequestEntityTooLarge, "body_too_large"
		h.writeError(w, rc, st.status, rid)
		return
	}
	extended := isExtendedConnect(r) // WebSocket over HTTP/2 (RFC 8441)
	if r.Method == http.MethodConnect && !extended {
		// Plain CONNECT asks for a raw TCP tunnel, which a reverse proxy does not
		// offer. Checked before routing: a CONNECT has no path to route on.
		st.status, st.errKind = http.StatusMethodNotAllowed, "connect_not_allowed"
		h.writeError(w, rc, st.status, rid)
		return
	}
	if snap == nil {
		st.status, st.errKind = http.StatusServiceUnavailable, "not_ready"
		h.writeError(w, rc, st.status, rid)
		return
	}
	r = normalizePath(r)
	router := snap.Router
	if port != 0 {
		router = nil // never fall back: that would serve another Gateway's routes
		if p := snap.Ports[port]; p != nil {
			router = p.Router
		}
	}
	var route *Route
	if router != nil {
		route = router.Match(r)
	}
	if route == nil {
		st.status, st.errKind = http.StatusNotFound, "no_route"
		h.writeError(w, rc, st.status, rid)
		return
	}
	st.route = route.Name

	upgrade := upgradeType(r.Header)
	if extended {
		upgrade = "websocket"
	}
	tmpl, body, ridVal := h.outbound(w, r, rc, peerStr, trusted, rid, upgrade)

	// Policies see the request as the upstream will (the outbound headers),
	// and may answer it, rewrite it or pick its pool (docs/POLICY_LANGUAGE.md).
	var pool *lb.Pool
	if len(route.Policies) > 0 {
		penv := envPool.Get().(*policy.Env)
		defer func() { penv.Reset(); envPool.Put(penv) }() // runs before the access-log defer; nothing reads penv there
		penv.Method, penv.Host, penv.Path, penv.RawQuery = r.Method, normalizeHost(r.Host), tmpl.URL.Path, tmpl.URL.RawQuery
		penv.ClientIP, penv.Header, penv.Now = clientIP, tmpl.Header, start.Unix()
		st.progs, st.penv = route.Policies, penv
		if h.requestPolicies(w, rc, route.Policies, penv, rid, &st) {
			return
		}
		if penv.Path != tmpl.URL.Path {
			tmpl.URL.Path, tmpl.URL.RawPath = penv.Path, ""
		}
		if penv.RouteTo != "" {
			pool = snap.Pools[penv.RouteTo] // validated at load: the pool exists
		}
	}
	if route.Redirect != nil {
		st.status, st.errKind = route.Redirect.Status, "redirect"
		loc := route.Redirect.location(r.Host, tmpl.URL)
		if !h.responsePhase(&st, st.status, w.Header(), rid) {
			clear(w.Header())
			st.status, st.errKind = http.StatusInternalServerError, "policy_error"
			h.writeError(w, rc, st.status, rid)
			return
		}
		h.extendWrite(rc)
		w.Header().Set("Location", loc)
		w.Header().Set(HeaderRequestID, rid)
		w.WriteHeader(st.status)
		return
	}
	if route.Edit != nil {
		route.Edit.apply(tmpl.Header)
	}
	if pool == nil {
		pool = route.PickPool()
	}
	st.pool = pool.Name
	if pool.Status != 0 {
		st.status, st.errKind = pool.Status, "fixed_status"
		h.upstreamError(w, rc, rid, &st)
		return
	}
	if pool.Budget != nil {
		pool.Budget.Deposit()
	}
	var wsKey string
	if extended {
		wsKey = toHTTP1WebSocket(tmpl)
		body = nil // the client's stream body is the tunnel, read after the handshake
	}
	transport := pool.Transport
	if transport == nil {
		transport = h.o.Transport
	}
	// Only requests that may be retried pay for the write tracing.
	retryable := pool.Retries > 0 && pool.Budget != nil && idempotent(r.Method)
	out := tmpl

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
			h.upstreamError(w, rc, rid, &st)
			return
		}
		st.backend = b.Addr
		st.attempts++
		if st.attempts > 1 {
			out = retryCopy(tmpl) // rare: only after a failed dial
		}
		out.URL.Scheme, out.URL.Host = pool.Scheme, b.Addr

		b.Acquire()
		resp, err := transport.RoundTrip(out)
		if err == nil {
			defer b.Release()
			h.report(pool, b, gatewayFailure(resp.StatusCode), metrics.StatusClass(resp.StatusCode))
			if resp.StatusCode == http.StatusSwitchingProtocols {
				if extended {
					st.status, st.errKind = h.serveExtendedConnect(w, rc, r, resp, wsKey, rid, &st)
				} else {
					st.status, st.errKind = h.serveUpgrade(w, rc, r, resp, upgrade, rid, &st)
				}
				return
			}
			h.serveResponse(w, rc, r, resp, ridVal, &st)
			return
		}
		b.Release()

		st.status, st.errKind = classify(r.Context(), body, err)
		upstreamFault := st.status == http.StatusBadGateway || st.status == http.StatusGatewayTimeout
		if upstreamFault {
			h.report(pool, b, true, st.errKind)
		}
		// Retry only if: the method is idempotent, nothing reached the backend
		// (the TCP connect itself failed, and no body was read), attempts remain,
		// and the budget allows it (ADR 0005).
		if upstreamFault && retryable && dialFailed(err) && body.untouched() &&
			len(tried) < pool.Retries && pool.Budget.Withdraw() {
			tried = append(tried, b)
			if h.o.Metrics != nil {
				h.o.Metrics.Retry(pool.Name)
			}
			continue
		}
		if st.status != statusClientClosed {
			h.upstreamError(w, rc, rid, &st)
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

func (h *Handler) serveResponse(w http.ResponseWriter, rc *http.ResponseController, r *http.Request, resp *http.Response,
	rid []string, st *reqState) {
	defer resp.Body.Close()
	st.status = resp.StatusCode
	removeHopByHop(resp.Header)
	hdr := w.Header()
	for k, vv := range resp.Header {
		hdr[k] = vv
	}
	addVia(hdr, resp.ProtoMajor, resp.ProtoMinor)
	hdr[HeaderRequestID] = rid // the same read-only slice sent upstream
	if !h.responsePhase(st, resp.StatusCode, hdr, rid[0]) {
		clear(hdr) // fail closed: none of the upstream response reaches the client
		st.status, st.errKind = http.StatusInternalServerError, "policy_error"
		h.writeError(w, rc, st.status, rid[0])
		return
	}
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
// It also returns the request ID as a header value, shared with the response.
func (h *Handler) outbound(w http.ResponseWriter, r *http.Request, rc *http.ResponseController,
	peer string, trusted bool, rid, upgrade string) (*http.Request, *clientBody, []string) {
	hdr := forwardableHeaders(r.Header)
	if hasToken(r.Header, "Te", "trailers") {
		hdr.Set("Te", "trailers")
	}
	if upgrade != "" { // Connection and Upgrade are hop-by-hop, but an upgrade must be re-requested on the next hop
		hdr.Set("Connection", "Upgrade")
		hdr.Set("Upgrade", upgrade)
	}
	dyn := make([]string, 3) // XFF, X-Forwarded-Host, request ID: one allocation
	setForwarded(hdr, r, peer, trusted, dyn)
	dyn[2] = rid
	ridVal := dyn[2:3:3]
	hdr[HeaderRequestID] = ridVal
	if _, ok := hdr["User-Agent"]; !ok {
		hdr["User-Agent"] = emptyValue // stop net/http adding "Go-http-client/1.1"
	}

	// One shallow copy of the inbound request carries its context (so a client
	// disconnect cancels the upstream call) without a second WithContext copy.
	// Then every server-side field a client request must not have is reset.
	out := new(http.Request)
	*out = *r
	out.URL = &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	out.Header = hdr
	out.Body, out.GetBody = nil, nil
	out.RequestURI, out.RemoteAddr, out.TLS = "", "", nil
	out.Close, out.TransferEncoding, out.Trailer = false, nil, nil
	out.Form, out.PostForm, out.MultipartForm, out.Response = nil, nil, nil, nil
	var body *clientBody
	if r.ContentLength != 0 { // > 0 known length, -1 chunked
		body = &clientBody{r: r.Body, rc: rc, idle: h.o.Limits.BodyReadTimeout, want: r.ContentLength}
		if limit := h.o.Limits.MaxBodyBytes; limit > 0 {
			body.r = http.MaxBytesReader(w, r.Body, limit)
		}
		out.Body = body
	}
	return out, body, ridVal
}

// dialFailed reports a failure to connect at all. Then nothing was sent, so
// even a request the backend must not see twice cannot have been seen once.
// Failures on an already open connection are excluded: the request may have
// been written. (net/http itself retries idempotent requests on a reused
// connection that dies before anything was written.)
func dialFailed(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// retryCopy gives a retry its own request and URL, so nothing the Transport
// may still hold from the failed attempt is mutated.
func retryCopy(tmpl *http.Request) *http.Request {
	out := new(http.Request)
	*out = *tmpl
	u := *tmpl.URL
	out.URL = &u
	return out
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
	h.writeErrorMsg(w, rc, status, rid, http.StatusText(status))
}

func (h *Handler) writeErrorMsg(w http.ResponseWriter, rc *http.ResponseController, status int, rid, msg string) {
	h.extendWrite(rc)
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set(HeaderRequestID, rid)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}{msg, rid})
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
