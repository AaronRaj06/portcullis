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

## Request lifecycle (Phase 1)

`internal/proxy.Handler.ServeHTTP`, in order:

1. **Identify the peer.** The client IP comes from the TCP connection (`RemoteAddr`), never from a header. If that IP is inside `TrustedProxies`, we also believe its `X-Forwarded-*` and `X-Request-Id`.
2. **Request ID.** Keep a trusted, well-formed ID (`[A-Za-z0-9._:-]{1,128}`); otherwise generate 128 random bits as hex. It is sent upstream, returned to the client, written to the access log, and included in every error body.
3. **Early size check.** If `Content-Length` > `MaxBodyBytes`, return 413 before contacting the upstream.
4. **Build the outbound request.** Clone the headers, then:
   - remove hop-by-hop headers (the fixed list plus anything named in `Connection`), keeping `TE: trailers`;
   - set `X-Forwarded-For` (append if trusted, replace if not), `-Proto`, `-Host` and `Via`;
   - strip `Forwarded` from untrusted clients;
   - keep the original `Host`.

   The request body is wrapped (`clientBody`) to apply the body limit, the per-read idle deadline, and error capture. The outbound request shares the client's context, so a client disconnect cancels the upstream call.
5. **Round trip** through our own `http.Transport` (`NewTransport`): dial timeout, response-header timeout, pooled keep-alive connections, no environment proxy, and compression left untouched.
6. **Errors before headers** map to a status and a log/metric label (`classify`):

   | Cause | Status | Label |
   |---|---|---|
   | client body over limit | 413 | `body_too_large` |
   | client body stalled | 408 | `client_body_timeout` |
   | client body malformed | 400 | `client_body` |
   | client went away | 499 (logged only, nothing sent) | `client_closed` |
   | dial or header timeout | 504 | `upstream_timeout` |
   | anything else (refused, reset) | 502 | `upstream_error` |

   Client causes are checked first so the upstream is never blamed for them. (503 arrives in Phase 2, when "no healthy backend" exists.) Error bodies are JSON: `{"error": "...", "request_id": "..."}`.
7. **Response.** Strip hop-by-hop headers, copy the rest, add `Via` and `X-Request-Id`, then stream the body through a pooled 32 KiB buffer. If the response has no `Content-Length` or is `text/event-stream`, flush after every read so events are not held in a buffer. Copy trailers at the end.
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

## Package boundaries

| Package | Owns | Depends on |
|---|---|---|
| `config` | schema, validation, immutable snapshot | `policy` (to validate scripts) |
| `proxy` | forwarding, headers, streaming, errors, server/transport/TLS setup | `metrics` (later `config`, `lb`, `policy`) |
| `lb` | `Balancer` interface and algorithms | – |
| `health` | active checks, passive ejection | `lb` |
| `policy` | PolicyScript language | `ratelimit` |
| `ratelimit` | token buckets | – |
| `metrics` | Prometheus collectors | – |
| `admin` | admin HTTP API | `config`, `metrics` |
| `controller` | Gateway API reconciliation, snapshot push | `config`, `policy` |

Rule: `proxy` is the only package on the hot path that sees the whole request; everything else gets small, explicit inputs so it can be unit-tested alone.

## Metrics (Phase 1)

`portcullis_requests_total{method,code}`, `portcullis_request_duration_seconds{method}`, `portcullis_requests_in_flight`, `portcullis_proxy_errors_total{kind}`, plus Go runtime and process collectors. Methods outside the standard nine are reported as `OTHER`, so a client cannot create unlimited time series. Route and upstream labels arrive in Phase 2.
