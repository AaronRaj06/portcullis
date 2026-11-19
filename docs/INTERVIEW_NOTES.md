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

## Phase 2: Routing, load balancing and health

### Concepts, in plain language

- **Pool / backend / balancer.** A *pool* is a group of interchangeable servers (*backends*) for one service. A *balancer* decides which backend gets the next request.
- **Round robin / weighted round robin.** Take turns. Weighted means a backend with weight 3 gets three turns for every one a weight-1 backend gets. *Smooth* weighting interleaves the turns (A B A A) instead of bursting (A A A B), so no backend gets a burst.
- **Least connections.** Send to whoever is busiest the least right now (the fewest requests in flight). It's good when requests take very different amounts of time.
- **Power of two choices (P2C).** Pick two backends at random and use the less busy one. Surprisingly, this gets most of the benefit of least-connections at O(1) cost. Because each proxy samples randomly, many proxies don't all pile onto the same "least loaded" backend at once (*herding*).
- **Consistent hashing.** Map a key (user ID, cookie, IP) to a backend so the same key keeps going to the same place: session affinity, warm caches. "Consistent" means that when one backend leaves, only its own keys move; with `hash % N`, nearly every key would move. *Virtual nodes* give each backend many points on the ring so the shares even out.
- **Active vs passive health checks.** Active: we poll `/healthz` on a timer. Passive: we watch real traffic, and after N consecutive failures we stop sending to that backend for a while (*ejection*, or *outlier detection* in Envoy terms). Active catches dead backends with no traffic; passive reacts within a few requests.
- **Hysteresis.** Requiring several failures to mark a backend down, and several passes to bring it back, so it doesn't flap on one lost packet.
- **Idempotent.** Doing it twice has the same effect as doing it once (GET, PUT, DELETE). POST usually isn't. Charging a card twice is the classic bug.
- **Retry budget.** A cap on retries as a share of traffic, so retries can't multiply load during an outage (*retry storm*).
- **WebSocket upgrade.** The client asks to switch an HTTP/1.1 connection to another protocol. After the backend's `101 Switching Protocols`, the proxy becomes a dumb two-way pipe.

### Trade-offs and rejected alternatives

- **Lock-free balancers vs a mutex.** NGINX's smooth WRR updates shared counters on every pick, which normally needs a lock. We run the algorithm once when the pool is built, save one full cycle, and walk it with an atomic counter. Same output, no lock, 0 allocations.
- **Precomputed cycle vs live smooth WRR when a backend is down.** NGINX recomputes over live backends only. Our cycle keeps the dead backend's slots, and we re-draw those slots at random. That costs a random number on the failure path, but keeps the shares proportional; the first version did a linear scan and over-sent to the next backend (a test caught it).
- **Retrying only when headers were never written** (ADR 0005), vs Envoy-style "retry on 5xx". Stricter and less effective, but it can never duplicate a request a backend already saw.
- **Optimistic start** (new backends healthy) vs pessimistic. We avoid a 503 window on startup. The risk is a few failed requests to a backend that is dead from the start, and passive ejection and retries absorb those.
- **Counting 502/503/504 but not 500 toward ejection.** A 500 is usually one bad request or one buggy endpoint. Ejecting a whole host for it would turn one bug into lost capacity.
- **Gateway API precedence** for routing, rather than first-match-in-file order. Phase 6 translates HTTPRoutes directly, so the data plane should already behave the way the spec says.

### Likely interview questions

1. **How does consistent hashing limit disruption, and how did you pick the virtual node count?** Each backend owns `vnodes × weight` points on a 64-bit ring, and a key goes to the next point clockwise. Removing a backend only reassigns its own arcs: `TestConsistentHashStability` removes 1 of 5 and checks that zero keys move between survivors. On vnodes, I measured instead of guessing: worst-case share off fair was 16% at 160, 4.7% at 640 and 1.2% at 2560, close to 1/√v. 640 is the default.
2. **Why P2C over least-connections?** Least-connections scans every backend (O(n)), and in a fleet of proxies they all see the same idle backend and herd onto it. P2C is O(1) with randomness, so different proxies pick differently, and it still avoids hot backends. `TestP2CAvoidsLoadedBackend` shows a backend with 100 in flight never gets picked. With two backends P2C always compares both, so it behaves exactly like least-connections (`TestLoadAwareBalancersThroughProxy`).
3. **When do you retry, and how do you prevent a retry storm?** Only for idempotent methods, only if `httptrace` shows no request headers were written to the backend, only to a backend not yet tried, and only if the token-bucket budget allows: +0.2 tokens per request, −1 per retry. `TestRetryBudgetCapsAmplification` sends 20 requests to an all-dead pool and asserts exactly 23 attempts.
4. **How do active and passive health interact?** A backend gets traffic only if both allow it: actively healthy *and* `now >= ejectedUntil`. Re-admission after ejection needs no timer or event, because the comparison flips on its own. `portcullis_backend_up` is computed at scrape time for the same reason.
5. **How is routing precedence decided?** As in Gateway API: hostname specificity first (exact > longest wildcard > any), then exact path > longest prefix > method > header count. Routes are pre-sorted per hostname bucket, so matching is a map lookup plus a short scan: ~60 ns, 0 allocations.

### What went wrong or surprised us

- **Our own tests found three bugs on the first run.**
  1. Round-robin over-sent to the backend after a dead one (16.6k vs 13.4k of 30k); fixed with random re-draws.
  2. A retry budget built with `burst = 0` could never hold a token, so it never allowed a single retry; the cap is now at least one token.
  3. Consistent hashing was 16% off fair. That one was *not* a bug: I measured arc shares directly, found ring variance at 160 vnodes, and raised the default instead of loosening the test.
- **A test that couldn't fail.** The Phase 2 integration tests all passed on the first run, so I mutation-tested the guards: delete each retry condition and check its test fails. Five did. The sixth, clearing the write deadline on hijacked WebSocket connections, survived. Reading `net/http/server.go` showed `hijackLocked` already calls `SetDeadline(time.Time{})`, so the line was dead code. I deleted it and kept the test, which still proves idle tunnels survive `WriteTimeout`.
- **Allocation hiding in plain sight.** `net.SplitHostPort` allocates an error for every `Host` header without a port (the common case). A byte scan took routing from 1 allocation to 0.
