# Interview notes

One section per phase: concepts, trade-offs, likely questions, and one thing that went wrong.

## Phase 0: Scaffold

### Concepts, in plain language

- **Reverse proxy**: a server that sits in front of your real servers (*backends* or *upstreams*). Clients talk to it; it forwards each request to a backend and passes the answer back. Clients never see the backends directly.
- **L7 (layer 7)**: the proxy understands HTTP itself (paths, headers, methods), not just TCP bytes. That is what lets it route `/api` and `/static` to different places. An **L4** balancer only sees IPs and ports.
- **Data plane vs control plane**: the data plane moves user traffic and has to be fast. The control plane decides the configuration and pushes it. Splitting them means a slow or crashed controller does not stop traffic.
- **`internal/` packages**: Go refuses to let code outside this module import anything under `internal/`. That keeps our API surface at zero, so we can refactor freely.
- **Race detector (`-race`)**: a test mode that instruments memory accesses and fails if two goroutines touch the same data without synchronisation. A proxy is heavily concurrent, so every test runs with it.
- **Distroless, non-root image**: the final container holds only our static binary and CA certificates, with no shell or package manager, and runs as an unprivileged user. Less to attack, and less to patch.

### Trade-offs and rejected alternatives

- **One module, many packages** rather than several modules: one version, simple refactors. A multi-module repo pays off only when parts are released separately.
- **Tools via pinned `go run`** rather than a `tools.go` file or global installs (ADR 0002). Reproducible with no install step. The cost is a slower first run.
- **`go 1.23.0` minimum** rather than the local 1.27. A wider compatibility promise, at the price of skipping newer features until we bump it on purpose.
- **Both staticcheck and golangci-lint**: golangci-lint already bundles staticcheck, so this is partly redundant. The brief asks for both, and running standalone staticcheck keeps its exact upstream behaviour.

### Likely interview questions

1. **Why separate the control plane and data plane?** Different failure and performance needs. The proxy has to keep serving its last good config even if the controller is down or pushes garbage. Here they are separate binaries and Deployments, talking only through a versioned `PUT /config`.
2. **Why Go for a proxy?** Cheap goroutines for one-per-connection concurrency, a production-grade `net/http` and TLS stack in the stdlib, static binaries, and good profiling (pprof). The cost is GC pauses and less control than C or Rust, which is why Phase 4 measures allocations.
3. **How do you keep builds reproducible?** `go.mod` and `go.sum` pin dependencies. Tools are pinned by version in the Makefile. `-trimpath` removes local paths. CI uses the same `make` targets as a laptop.
4. **What does your CI enforce?** gofmt, go vet, staticcheck, golangci-lint (including gosec), `go test -race`, a fuzz smoke run, govulncheck and a Docker build. A pull request cannot pass with formatting drift, data races or known-vulnerable dependencies.
5. **Why run as non-root on distroless?** If someone finds a bug that lets them run code, there is no shell, no package manager and no root to escalate with. It also makes image scanners quieter, because there is almost nothing to report.

### What went wrong

The first `make lint` failed inside staticcheck with `export data version 5 is greater than maximum supported version 2`. The pinned `staticcheck 2025.1.1` predates Go 1.27 and cannot read the compiler's newer package format. Fix: pin the current release, `v0.8.2`, and record the lesson in ADR 0002. Linters must keep up with the toolchain.

## Phase 1: Core proxy

### Concepts, in plain language

- **Hop-by-hop vs end-to-end headers.** Some headers describe *this one connection* (`Connection`, `Keep-Alive`, `Transfer-Encoding`, `Upgrade`, `Proxy-Authorization`...). A proxy is two connections glued together, so it must drop them instead of forwarding them. A sender can also mark any header hop-by-hop by naming it in `Connection: X-Foo`. Forwarding `Proxy-Authorization` would leak credentials to the backend.
- **X-Forwarded-For (XFF).** Behind a proxy, the backend sees the proxy's IP, so the proxy writes the real client IP into XFF. Anyone can *send* an XFF header, though. We only append to an existing one when the TCP peer is in our trusted list; otherwise we replace it. Otherwise any client could claim to be `10.0.0.1` and get past IP allow-lists.
- **Via.** Records which proxies a message passed through, and with which protocol version (a backend speaking HTTP/1.0 shows up as `Via: 1.0 portcullis`). It is used for loop detection and debugging.
- **Streaming vs buffering.** Buffering means reading the whole body into memory before forwarding. It costs RAM per request and breaks server-sent events (SSE). We copy 32 KiB at a time, and flush after every chunk when the response has no length or is an event stream.
- **Context cancellation.** In Go, a `context.Context` is a "this work is no longer wanted" signal. The outbound request uses the client's context, so when the client disconnects, the Transport aborts the upstream request. No wasted backend work.
- **Slowloris.** An attack that opens many connections and sends headers one byte at a time, so the server runs out of connection slots. `ReadHeaderTimeout` closes any connection whose headers aren't complete in time.
- **Graceful shutdown / draining.** On SIGTERM (what Kubernetes sends before killing a pod): stop accepting, finish what is in flight, then exit. Users don't see errors during rolling deploys.
- **499.** NGINX's made-up status for "the client closed the connection before we answered". Nothing is sent; it only exists in logs and metrics so client aborts are not counted as server errors.
- **Post-quantum hybrid key exchange.** X25519MLKEM768 combines classic X25519 with ML-KEM, a lattice-based scheme believed to resist quantum computers. Recorded traffic can't be decrypted later even if one of the two is broken. Go enables it by default, and our test checks it is negotiated.

### Trade-offs and rejected alternatives

- **Own handler vs `httputil.ReverseProxy`.** The brief requires our own. The real benefit is that every behaviour is visible and tested, and Phase 4 can optimise the hot path. Cost: we have to re-learn the edge cases ReverseProxy already handles (we hit two; see below).
- **Per-read/per-write idle deadlines vs `Server.ReadTimeout`/`WriteTimeout`** (ADR 0004). The server-wide timeouts cap the *total* time, which kills SSE and big uploads. Idle deadlines only cut off peers that stop moving data.
- **Trusting incoming request IDs only from trusted proxies.** This keeps end-to-end tracing through our own load balancers, without letting the internet put arbitrary strings into our logs. The charset is restricted too, which blocks log injection.
- **Rejecting an upstream path** instead of silently joining paths. A wrong join is a silent routing bug; an explicit error is better until routing exists (Phase 2).
- **Metrics labels.** Methods are normalised to the nine standard ones plus `OTHER`. Otherwise a client sending random methods would create one time series per method and exhaust Prometheus memory (*cardinality explosion*).

### Likely interview questions

1. **How do you stop clients spoofing their IP through X-Forwarded-For?** The client IP always comes from the TCP peer address. A received XFF is kept and appended to only if that peer is inside `TrustedProxies`; otherwise XFF, X-Forwarded-Host/Proto and `Forwarded` are deleted and rewritten (`setForwarded` in `internal/proxy/headers.go`). `TestHeaders` covers both cases.
2. **What happens when the client disconnects mid-request?** The outbound request carries the client's context. net/http cancels that context when the connection drops, and the Transport aborts the upstream call. `TestClientCancelPropagatesUpstream` checks that the backend sees the cancellation. We log 499 and send nothing.
3. **How do you tell a 502 from a 504?** 504 means the upstream was too slow: a dial timeout or the response-header timeout (`net.Error.Timeout()`). 502 means it failed outright: refused, reset, or a bad response. Client-caused failures are checked first, so a stalled upload is a 408, not a 504.
4. **Why not just set `http.Server.WriteTimeout = 30s`?** It is a deadline for the whole response, so every SSE stream or long download dies at 30 seconds. We push the write deadline forward before every write instead, which bounds how long a *stalled* client can hold resources.
5. **How does graceful shutdown work, and what if a request never finishes?** `Server.Shutdown` closes the listeners and waits for active connections to go idle. After `-drain-timeout`, `Server.Close` force-closes them, which cancels their contexts and therefore their upstream calls. `TestGracefulShutdownDrainsInFlight` shows new connections refused while an in-flight request completes.

### What went wrong

Two net/http subtleties, both found by tests and both now covered by regression tests:

1. **A stalled upload hung for 3 seconds instead of returning 408.** The access log showed the right status, but the handler only returned when the client gave up. Cause: after the body-read timeout I *cleared* the read deadline. The Transport then closed the request body, and for a server request body `Close` drains the unread rest, which blocked forever on the stalled client. Fix: clear the deadline only after a *successful* read; after an error, the expired deadline stays, so the drain fails at once. 3 s → 104 ms.
2. **A body-read error cancels the request context.** I originally checked "context cancelled → client gone, send nothing" first. But net/http cancels the context *because* the body read failed, so a 408 was being reported as a silent 499. Fix: check the captured body error before the context.

A third issue was caught by design review before it shipped, and its regression test passes: once the body is fully read, net/http starts a background read on the connection. If our per-read deadline were still set, it would fire during a slow upstream response and cancel the request. `clientBody` clears the deadline the moment the last byte is read (`TestSlowUpstreamAfterBodyIsNotCancelled`).
