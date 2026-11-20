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

## Configuration and hot reload (ADR 0007)

```
 file ──SIGHUP / POST /reload──┐
                               ▼
 controller ──PUT /config──▶ Parse (strict) → Validate (all errors, with paths)
                               │ error: 400 + problems; old snapshot keeps serving
                               ▼
                             Compile → Snapshot{Router, pools, transports, hash, version}
                               │ backends inherit health/ejection state by (pool, address)
                               ▼
                         atomic.Pointer.Swap ──▶ new requests
                               │                 in-flight requests finish on the old one
                               └─ stop old health checks, start new ones
```

- **Process-level (flags, restart):** `-listen`, `-admin-listen`, `-tls-cert/-key`, timeouts and limits, `-admin-token-file`, `-pprof`.
- **Traffic (file or API, live):** `trustedProxies`, `pools` (scheme, lb, hashKey, backends, healthCheck, ejection, retries, panic, tls), `routes` (hosts, path, pathType, methods, headers, backends with weights). Example: `docs/examples/portcullis.yaml`.
- `-upstream` and friends are shorthand: they build a one-route config and go through the same `Manager.ApplyConfig` path.
- **Admin API** (`internal/admin`): `/healthz` (liveness), `/readyz` (a config is serving and we are not draining), `/metrics`, `GET/PUT /config`, `POST /reload`, optional `/debug/pprof/`. On SIGTERM `/readyz` turns 503 first, the proxy drains, and the admin server stops last.
- **Concurrency control:** `ETag: "<hash>"` on `GET`/`PUT /config`; `PUT` with `If-Match` is applied only over that config (412 otherwise), with the check and the swap under one lock.
- **Unchanged pool transports are reused** (keyed by scheme, SNI, CA path and CA contents), so reloads don't drop upstream connections. The config hash covers CA file contents.
- **Certificate rotation:** SIGHUP also re-reads `-tls-cert/-tls-key` into `CertReloader`; handshakes read the current certificate through an atomic pointer.
- **Metrics:** `portcullis_config_generation`, `portcullis_config_reloads_total{result=success|unchanged|error}`, `portcullis_config_last_success_timestamp_seconds`.

## Request lifecycle

`internal/proxy.Handler.ServeHTTP`, in order:

1. **Identify the peer and the real client.** The peer IP comes from the TCP connection (`RemoteAddr`). If the peer is inside `TrustedProxies`, we also believe its `X-Forwarded-*` and `X-Request-Id`, and the *real client* is found by walking `X-Forwarded-For` from the right past trusted hops; the first untrusted address wins, and entries further left are never believed because the client could forge them. Otherwise the real client is the peer. The real client is used for hashing and logs (`client` and `peer` are both logged).
2. **Request ID.** Keep a trusted, well-formed ID (`[A-Za-z0-9._:-]{1,128}`); otherwise generate 128 random bits as hex. It is sent upstream, returned to the client, written to the access log, and included in every error body.
3. **Early size check.** If `Content-Length` > `MaxBodyBytes`, return 413 before contacting the upstream.
3a. **Route.** `Router.Match` picks the most specific route (see Routing below), or returns 404. The route chooses a pool: either its single pool, or the next one in its weighted split. The pool's retry budget gets its per-request deposit.
4. **Build the outbound request.** Clone the headers, then:
   - remove hop-by-hop headers (the fixed list plus anything named in `Connection`), keeping `TE: trailers`;
   - set `X-Forwarded-For` (append if trusted, replace if not), `-Proto`, `-Host` and `Via`;
   - strip `Forwarded` from untrusted clients;
   - keep the original `Host`.

   The request body is wrapped (`clientBody`) to apply the body limit, the per-read idle deadline, and error capture. The outbound request shares the client's context, so a client disconnect cancels the upstream call.
5. **Pick and send.** The pool's balancer picks an available backend that hasn't been tried yet. If there is none, return 503, unless the pool is in panic mode (ADR 0006), which then picks one regardless. The pool's own `Transport` is used if set (private CA, h2c); otherwise the default. The request goes through our own `http.Transport` (`NewTransport`): dial timeout, response-header timeout, pooled keep-alive connections, HTTP/2 to TLS upstreams, no environment proxy, and compression left untouched. Every outcome is reported to the pool for passive ejection. If the attempt fails and ADR 0005's retry conditions hold, go back to Pick.
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
7a. **Upgrades (WebSocket).** If the client asked to upgrade (`Connection: upgrade` + `Upgrade: X`), those two headers are re-added after hop-by-hop stripping. If the backend answers 101 with the same protocol, we hijack the client connection, write the 101, and copy bytes both ways until either side closes. A mismatched protocol gives 502. Hijacking resets connection deadlines, so idle tunnels are not cut by `WriteTimeout`. Tunnels are tracked by the handler, because `Shutdown` forgets hijacked connections. `proxy.Serve` calls `Handler.Drain` after `Shutdown`: it waits for tunnels until the drain deadline, then closes them.
7b. **WebSockets over HTTP/2 (RFC 8441, ADR 0008).** An extended CONNECT (`:protocol websocket`) is rewritten into an HTTP/1.1 `GET` upgrade with a generated `Sec-WebSocket-Key`. The backend's `Sec-WebSocket-Accept` is verified, the client gets `200`, and the HTTP/2 stream becomes the tunnel. This needs `GODEBUG=http2xconnect=1`. Plain CONNECT gets 405.
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

`proxy.Serve` waits for the context (SIGINT or SIGTERM in `main`), then calls `Server.Shutdown`. That closes the listeners, so new connections are refused, and lets in-flight requests finish. It then calls `Handler.Drain` for upgraded connections, which `Shutdown` cannot see. After `-drain-timeout` it force-closes everything left; that cancels their request contexts, which cancels the upstream calls.

## Upstream transports

The default transport speaks HTTP/1.1, or HTTP/2 when a TLS upstream offers it. A pool can bring its own: `-upstream-ca` builds one that trusts a private CA, and `h2c://` upstreams use `NewH2CTransport` (cleartext HTTP/2, prior knowledge). Health checks reuse the pool's transport, so they test exactly what traffic uses.

## TLS

`proxy.TLSConfig` is the only place TLS policy lives:
- TLS 1.2 minimum.
- ECDHE + AEAD cipher suites for TLS 1.2 (the 1.3 suites are fixed by Go).
- `CurvePreferences` left nil on purpose. Go's default prefers the post-quantum hybrid **X25519MLKEM768**, and `TestTLSTermination` checks that a modern client actually negotiates it.

## Routing

`proxy.Router` follows Gateway API precedence:
1. **Hostname:** exact (case-insensitive, port and trailing dot ignored) > longest `*.suffix` wildcard (matches one or more labels, never the apex) > routes with no hostname.
2. **Within a hostname group,** sorted once at build time: exact path > longer prefix > has a method match > more header matches > declaration order.

**Traffic splitting.** A route can name one `Pool` or a weighted `Split` across pools (canary 90/10). The split runs smooth WRR once at build time into a cycle of pools, walked with an atomic counter: exact proportions, interleaved, lock-free. Weight 0 sends nothing; an all-zero split is rejected. A pool with no available backend is *not* skipped, so a broken canary fails its share visibly instead of silently shifting traffic to stable.

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
- **Back-off and panic** (ADR 0006): repeat ejections last `EjectFor × n`, capped at `MaxEjectFor`; the multiplier resets after a clean period. Optional panic mode serves from all backends when none is available.
- **Passive** (`lb.Pool.Report`): every attempt reports. Connect errors, timeouts and 502/503/504 *responses* count as gateway failures; a 500 does not (that is an application error, not a sick host). After `EjectAfter` consecutive failures the backend is ejected for `EjectFor`, then re-admitted automatically, with no event needed: availability is `now >= ejectedUntil`.
- A backend gets traffic only if **both** say yes. `portcullis_backend_up` is computed from live state at scrape time, so it never goes stale.

## Retries

See ADR 0005. In short: idempotent + nothing reached the backend (`httptrace` `WroteHeaders`) + attempts left + budget token. The budget is a lock-free token bucket: +0.2 per request, −1 per retry.

## Package boundaries

| Package | Owns | Depends on |
|---|---|---|
| `config` | schema, strict parsing, validation, compile to snapshot, Manager (swap + health lifecycle) | `proxy`, `lb`, `health`, `metrics` (later `policy`) |
| `proxy` | routing, forwarding, retries, upgrades, headers, streaming, errors, server/transport/TLS setup | `lb`, `metrics` (later `config`, `policy`) |
| `lb` | `Balancer` interface and algorithms | – |
| `health` | active checks (passive ejection is in `lb.Pool`) | `lb` |
| `policy` | PolicyScript language | `ratelimit` |
| `ratelimit` | token buckets | – |
| `metrics` | Prometheus collectors | `lb` (backend health collector) |
| `admin` | admin HTTP API | `config` |
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
| `portcullis_panic_picks_total` | pool |

Plus the Go runtime and process collectors. Label values come only from config or fixed sets. Methods outside the standard nine become `OTHER`, so a client cannot create unlimited time series. Policy denials and config generation arrive in Phases 3 and 5.
