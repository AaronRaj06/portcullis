# Design

This file explains *how* Portcullis works; ADRs in `adr/` explain *why* a choice was made.

## Big picture

```
            Gateway API objects + PortcullisPolicy
                          │ watch
              ┌───────────▼───────────┐
              │ portcullis-controller │  (control plane)
              └───────────┬───────────┘
                          │ PUT /config (versioned snapshot)
 client ──HTTP──▶ ┌───────▼────────┐ ──▶ backend pods
                  │   portcullis   │  (data plane)
                  │ route → policy → LB → upstream
                  └────────────────┘
```

- **Data plane**: the process that actually handles user traffic. It must be fast and must never stop serving because of a bad config.
- **Control plane**: the process that decides what the data plane should do, and tells it. It can be slow; it must be correct.

## Request lifecycle

`internal/proxy.Handler.ServeHTTP`, in order:

1. **Identify the peer.** The client IP comes from the TCP connection (`RemoteAddr`), never from a header. If that IP is inside `TrustedProxies`, we also believe its `X-Forwarded-*` and `X-Request-Id`.
2. **Request ID.** Keep a trusted, well-formed ID (`[A-Za-z0-9._:-]{1,128}`); otherwise generate 128 random bits as hex. It is sent upstream, returned to the client, written to the access log, and included in every error body.
3. **Early size check.** If `Content-Length` > `MaxBodyBytes`, return 413 before contacting the upstream.
3a. **Route.** `Router.Match` picks the most specific route (see Routing below), or returns 404. The route names a pool; the pool's retry budget gets its per-request deposit.
4. **Build the outbound request.** Clone the headers, then:
   - remove hop-by-hop headers (the fixed list plus anything named in `Connection`), keeping `TE: trailers`;
   - set `X-Forwarded-For` (append if trusted, replace if not), `-Proto`, `-Host` and `Via`;
   - strip `Forwarded` from untrusted clients;
   - keep the original `Host`.

   The request body is wrapped (`clientBody`) to apply the body limit, the per-read idle deadline, and error capture. The outbound request shares the client's context, so a client disconnect cancels the upstream call.
5. **Pick and send.** The pool's balancer picks an available backend that hasn't been tried yet; if there is none, return 503. The request goes through our own `http.Transport` (`NewTransport`): dial timeout, response-header timeout, pooled keep-alive connections, HTTP/2 to TLS upstreams, no environment proxy, and compression left untouched. Every outcome is reported to the pool for passive ejection. If the attempt fails and ADR 0005's retry conditions hold, go back to Pick.
6. **Errors before headers** map to a status and a log/metric label (`classify`):

   | Cause | Status | Label |
   |---|---|---|
   | client body over limit | 413 | `body_too_large` |
   | client body stalled | 408 | `client_body_timeout` |
   | client body malformed | 400 | `client_body` |
   | client went away | 499 (logged only, nothing sent) | `client_closed` |
   | dial or header timeout | 504 | `upstream_timeout` |
   | anything else (refused, reset) | 502 | `upstream_error` |

   | no route matched | 404 | `no_route` |
   | no available backend | 503 | `no_backend` |

   Client causes are checked first so the upstream is never blamed for them. Error bodies are JSON: `{"error": "...", "request_id": "..."}`.
7. **Response.** Strip hop-by-hop headers, copy the rest, add `Via` and `X-Request-Id`, then stream the body through a pooled 32 KiB buffer. If the response has no `Content-Length` or is `text/event-stream`, flush after every read so events are not held in a buffer. Copy trailers at the end.
7a. **Upgrades (WebSocket).** If the client asked to upgrade (`Connection: upgrade` + `Upgrade: X`), those two headers are re-added after hop-by-hop stripping. If the backend answers 101 with the same protocol, we hijack the client connection, write the 101, and copy bytes both ways until either side closes. A mismatched protocol gives 502. Hijacking resets connection deadlines, so idle tunnels are not cut by `WriteTimeout`. Hijacked connections are not tracked by `Shutdown`.
8. **Errors after headers.** The status line is already sent, so panic with `http.ErrAbortHandler`. The connection is cut, and the client sees a truncated response rather than one that looks complete.
9. **Log and metrics** are recorded in a `defer`, so they run even on abort.

## Timeouts and limits

| Limit | Default | Protects against |
|---|---|---|
| `ReadHeaderTimeout` | 10s | slowloris (dribbling headers) |
| `IdleTimeout` | 120s | idle keep-alive connections piling up |
| `WriteTimeout` (per write) | 30s | clients that stop reading |
| `BodyReadTimeout` (per read) | 30s | clients that stop sending a body |
| `DialTimeout` | 5s | blackholed upstreams |
| `ResponseHeaderTimeout` | 30s | upstreams that accept but never answer |
| `MaxHeaderBytes` | 64 KiB | header memory exhaustion |
| `MaxBodyBytes` | 10 MiB | body abuse |

Why per-read/per-write instead of `http.Server.ReadTimeout`/`WriteTimeout`: see ADR 0004.

## Shutdown

`proxy.Serve` waits for the context (SIGINT or SIGTERM in `main`), then calls `Server.Shutdown`. That closes the listeners, so new connections are refused, and lets in-flight requests finish. After `-drain-timeout` it force-closes the remaining connections; that cancels their request contexts, which cancels the upstream calls.

## TLS

`proxy.TLSConfig` is the only place TLS policy lives:
- TLS 1.2 minimum.
- ECDHE + AEAD cipher suites for TLS 1.2 (the 1.3 suites are fixed by Go).
- `CurvePreferences` left nil on purpose. Go's default prefers the post-quantum hybrid **X25519MLKEM768**, and `TestTLSTermination` checks that a modern client actually negotiates it.

## Routing

`proxy.Router` follows Gateway API precedence:
1. **Hostname:** exact (case-insensitive, port and trailing dot ignored) > longest `*.suffix` wildcard (matches one or more labels, never the apex) > routes with no hostname.
2. **Within a hostname group,** sorted once at build time: exact path > longer prefix > has a method match > more header matches > declaration order.

Prefix matching works on whole path segments (`/api` matches `/api/x`, not `/apiv1`). If nothing matches under a specific hostname, less specific groups are tried. A match costs about 60 ns with 0 allocations (`BenchmarkRouterMatch`, 15 routes).

## Load balancing (`internal/lb`)

All per-backend state (`in flight`, `down`, `ejectedUntil`, `fails`) is atomic, so `Pick` takes no locks. `go test -bench Pick` reports 0 allocations per pick for every algorithm.

| Algorithm | How | Use when |
|---|---|---|
| round robin | precomputed cycle + atomic counter | identical backends |
| weighted RR | NGINX *smooth* WRR, computed once into a cycle (weights ÷ GCD), interleaved (5:1:1 → `aabacaa`) | mixed capacity, canaries |
| least connections | scan for the fewest in-flight, rotating start for ties | long or uneven requests |
| power of two choices | two random distinct backends, keep the less loaded | large pools: O(1) and no herding |
| consistent hash | 64-bit ring with `640 × weight` virtual nodes per backend, FNV-1a + splitmix64; key = header, cookie, client IP or path (missing key → client IP) | session affinity, cache locality |

**Unavailable backends** (down or ejected) are skipped. For round-robin, the skipped slot is re-drawn at random, so its share spreads across the others in proportion to their weights; a linear scan would give it all to the next backend. The hash ring walks clockwise to the next usable owner.

**Why 640 vnodes:** measured worst-case ring share for 4 backends was 16% off fair at 160 vnodes, 4.7% at 640 and 1.2% at 2560, close to the expected ~1/√vnodes.

## Health

- **Active** (`internal/health`): one goroutine per backend, `GET path` every interval with ±jitter and a random first delay. 2xx/3xx passes. The verdict flips only after `Unhealthy` consecutive failures or `Healthy` consecutive passes (hysteresis, so one lost packet doesn't flap a backend). Backends start healthy, so a new proxy serves at once.
- **Passive** (`lb.Pool.Report`): every attempt reports. Connect errors, timeouts and 502/503/504 *responses* count as gateway failures; a 500 does not (that is an application error, not a sick host). After `EjectAfter` consecutive failures the backend is ejected for `EjectFor`, then re-admitted automatically, with no event needed: availability is `now >= ejectedUntil`.
- A backend gets traffic only if **both** say yes. `portcullis_backend_up` is computed from live state at scrape time, so it never goes stale.

## Retries

See ADR 0005. In short: idempotent + nothing reached the backend (`httptrace` `WroteHeaders`) + attempts left + budget token. The budget is a lock-free token bucket: +0.2 per request, −1 per retry.

## Package boundaries

| Package | Owns | Depends on |
|---|---|---|
| `config` | schema, validation, immutable snapshot | `policy` (to validate scripts) |
| `proxy` | routing, forwarding, retries, upgrades, headers, streaming, errors, server/transport/TLS setup | `lb`, `metrics` (later `config`, `policy`) |
| `lb` | `Balancer` interface and algorithms | – |
| `health` | active checks (passive ejection is in `lb.Pool`) | `lb` |
| `policy` | PolicyScript language | `ratelimit` |
| `ratelimit` | token buckets | – |
| `metrics` | Prometheus collectors | `lb` (backend health collector) |
| `admin` | admin HTTP API | `config`, `metrics` |
| `controller` | Gateway API reconciliation, snapshot push | `config`, `policy` |

Rule: `proxy` is the only package on the hot path that sees the whole request; everything else gets small, explicit inputs so it can be unit-tested alone.

## Metrics

| Metric | Labels |
|---|---|
| `portcullis_requests_total` | route, method, code |
| `portcullis_request_duration_seconds` (histogram) | route |
| `portcullis_requests_in_flight` | – |
| `portcullis_proxy_errors_total` | kind |
| `portcullis_upstream_attempts_total` | pool, backend, result (`2xx`… or error kind) |
| `portcullis_retries_total` | pool |
| `portcullis_backend_ejections_total` | pool, backend |
| `portcullis_backend_up` (computed at scrape time) | pool, backend |

Plus the Go runtime and process collectors. Label values come only from config or fixed sets. Methods outside the standard nine become `OTHER`, so a client cannot create unlimited time series. Policy denials and config generation arrive in Phases 3 and 5.
