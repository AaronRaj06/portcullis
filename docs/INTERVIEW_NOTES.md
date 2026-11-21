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

### Phase 2 follow-ups: closing the gaps

**Concepts.**
- **Real client IP behind proxies:** read `X-Forwarded-For` right to left and stop at the first address you don't trust. Everything to its left was written by the client and could be fake.
- **Panic mode:** when *every* backend looks dead, the health checks are probably wrong, so serve anyway. It's off by default here, because a 503 is the honest answer.
- **Ejection back-off:** a backend that keeps failing is kept out longer each time (linear, capped).
- **h2c:** HTTP/2 without TLS, sent with "prior knowledge" because there is no TLS handshake to negotiate it.

**Likely questions.**
1. *Why read XFF from the right?* Each trusted proxy appends the address it saw, so the rightmost entries are written by infrastructure we control. The client controls the left end. `TestRealClient` includes a forged `6.6.6.6` on the left that must be ignored.
2. *Why doesn't a weighted split fail over when the canary is down?* Silent failover hides a broken canary from the very rollout that is supposed to detect it. The canary's 10% fails visibly, and alerts and rollback work as intended.
3. *How do WebSockets interact with graceful shutdown?* `http.Server.Shutdown` stops tracking a connection once it's hijacked. We track tunnels ourselves, and `Serve` calls `Handler.Drain`, which waits until the drain deadline and then closes them. Two tests cover the early-finish and forced-close cases.

**What went wrong.**
- **A data race I designed in, caught on review before any test.** I first wrote tunnel tracking with a `sync.WaitGroup`, but `Add` running concurrently with `Wait` is documented misuse, and a WebSocket handshake can finish during a drain. I replaced it with a mutex-guarded set plus a 25 ms poll, the same strategy `http.Server.Shutdown` uses.
- **A smoke test that tested the wrong binary.** A `make lint build` stopped at lint, so `bin/portcullis` was stale, and the new flags looked broken. Lesson: a failed earlier make step leaves old artefacts, so rebuild before testing. (The smoke script now prints the help line it relies on.)

## Phase 3: Config and hot reload

### Concepts, in plain language

- **Hot reload.** Changing what the proxy does without restarting it, and without dropping connections.
- **Immutable snapshot + atomic pointer swap.** Build a complete new routing table off to the side, then flip one pointer. A reader sees either the whole old table or the whole new one, never half of each, and readers never take a lock (`atomic.Pointer` is a single CPU instruction).
- **Read-copy-update (RCU).** The general name for "copy, modify the copy, publish it atomically, free the old one when its last reader is done". Here Go's garbage collector does the "free when the last reader is done" part: an in-flight request holds a reference to its snapshot.
- **Bootstrap vs dynamic config.** Some settings need a restart (which port to listen on); others can change live (where to route). Keeping them separate makes it obvious which is which.
- **Strict parsing.** Reject unknown fields, so that a typo is an error instead of a silently ignored setting.
- **Liveness vs readiness.** Liveness means "the process is alive; don't restart me". Readiness means "send me traffic". During shutdown we stay live but turn unready, so Kubernetes stops sending new requests while we drain.

### Trade-offs and rejected alternatives

- **Atomic swap vs `sync.RWMutex`.** An RWMutex read lock still writes to a shared counter, so every core fights over one cache line on every request. The atomic load is contention-free.
- **Full rebuild vs incremental patching.** Rebuilding the whole router allocates more per reload, but the router can never be half-updated. Reloads are rare (seconds apart); requests run thousands per second.
- **Carry backend state across reloads.** Without it, a reload would mark a just-ejected backend healthy again and send it traffic. Matching on (pool name, address) is simple and predictable.
- **Hash on canonical JSON after defaults.** "Same meaning, same hash", so `pathType: Prefix` written out explicitly and left implicit hash the same, and the controller's retries are no-ops.
- **Token in a file, compared in constant time.** A flag value shows up in `ps`. A plain string compare returns at the first wrong byte, which leaks timing.

### Likely interview questions

1. **How do you reload without dropping requests?** We compile a complete snapshot, validate it, and only then swap an `atomic.Pointer`. Each request loads the pointer once and keeps that snapshot for its whole life, retries included. `TestHotReloadUnderLoad` runs 16 clients against 100 reloads (10 deliberately invalid, half via `PUT /config`) under the race detector: 0 failed out of about 7–8.6k requests, every run.
2. **What if someone pushes a bad config?** Parse, validate and compile all happen before the swap. Any error returns 400 with *every* problem and its field path (`pools[0].backends[0].address: "nope": want host:port`), plus the status of the config still serving. The test asserts the snapshot pointer is unchanged after each rejected push.
3. **What happens to a request in flight during a reload that removes its backend?** It finishes normally on the old snapshot (`TestInFlightRequestFinishesOnOldSnapshot`: a 300 ms request to a backend removed mid-flight returns 200). New requests already see the new config.
4. **How does the controller know its push landed?** The 200 response is the acknowledgement and carries `hash` and `generation`. The controller compares hashes and retries with back-off on failure. Pushing the same config again is a no-op (`changed: false`), so retries are safe.
5. **Why does `/readyz` fail during shutdown but `/healthz` doesn't?** Failing readiness takes the pod out of Service endpoints so no new traffic arrives, while liveness stays OK so Kubernetes doesn't kill the pod mid-drain. The admin server is stopped last for exactly this reason.

### What went wrong

- **`go vet` caught a lock-copy bug.** The canary split counter (`atomic.Uint64`) lived inside `proxy.Route`, and the config compiler builds `[]proxy.Route` by value. Copying an atomic is a bug: two copies count separately. The counter is now a pointer allocated by `NewRouter`.
- **A shutdown sequence that would have hung.** My first draft read three results from a channel that only ever receives two, and it stopped the admin server together with the proxy, so `/readyz` couldn't report "draining". I caught it on review before running: there is now one channel per server, and the admin server stops last.
- **The stale-binary trap, again.** `make fmt` failed (an edited file needed formatting), so `build` never ran, and the smoke test exercised the previous binary, which doesn't know `-config`. The smoke script now aborts with the proxy's own error output if the proxy fails to start, so a stale binary can't masquerade as a bug.

### Phase 3 follow-ups: closing the gaps

**Concepts.**
- **Optimistic concurrency (ETag / If-Match):** "apply this only if the config is still the one I last read". A writer working from stale data gets `412 Precondition Failed` instead of silently overwriting a newer config. This is HTTP's standard lost-update protection.
- **Certificate rotation:** swap in a renewed certificate without restarting. New handshakes get the new one; existing connections keep theirs.
- **RFC 8441:** WebSockets over HTTP/2. There is no connection to take over (hijack), so the WebSocket rides inside one HTTP/2 stream opened with `CONNECT` + `:protocol`.

**Likely questions.**
1. *Two controllers push at once. What happens?* Each sends `If-Match` with the hash it read. The Manager checks and swaps under one mutex, so exactly one wins and the other gets 412 together with the serving status, so it can re-read and retry. `TestIfMatchRejectsStalePushes` plays out this race.
2. *How do you rotate a TLS certificate with zero downtime?* `tls.Config.GetCertificate` reads an `atomic.Pointer[tls.Certificate]`. SIGHUP loads the new files and stores the pointer only if they parse. `TestCertificateRotation` shows serial 1 → 2, then a broken key leaves 2 serving; the real binary did the same with openssl-made certificates.
3. *How does the proxy verify a backend's WebSocket handshake when the client never sent a key?* HTTP/2 clients send no `Sec-WebSocket-Key`, so the proxy generates one and checks `Sec-WebSocket-Accept = base64(SHA-1(key + RFC GUID))`. A backend that answers anything else gets 502 (tested with a deliberately forged accept).

**What went wrong.**
- **Go's HTTP/2 client can't send RFC 8441 requests.** `net/http` rejects `:protocol` as a header name, and recent `x/net/http2` Transports turned out to route back through `net/http`, so switching packages failed with the identical error. Reading the source (`transport.go:628`) explained it. The test now speaks raw HTTP/2 frames with `x/net`'s `Framer` and `hpack`, the same exchange a browser performs.
- **Routing ran before the method check.** A plain `CONNECT` has no path, so it got a 404 from the router instead of a 405. The method guard now runs first.
- **Dead code, found by mutation testing again.** Clearing the write deadline on HTTP/2 tunnels did nothing, because no deadline is ever set on that path. I deleted it; the idle-tunnel test keeps guarding the behaviour.

## Phase 4: Benchmarks and optimisation

### Concepts, in plain language

- **Closed loop vs open loop.** A closed-loop load generator keeps N requests in flight and sends the next one only when one finishes. It measures maximum throughput. An open-loop generator sends at a fixed rate whatever happens, the way real users arrive. Only open loop shows how long requests wait when the server stalls.
- **Coordinated omission.** In a closed loop, a server that stalls for one second also stops the generator from sending, so the second of requests that *would* have waited never gets measured, and the percentiles look far better than reality. vegeta's `-rate` mode avoids this.
- **p50 / p95 / p99.** The latency that 50%, 95% and 99% of requests beat. The tail (p99) is what users notice and what queues amplify.
- **CPU per 1k req/s.** The CPU used, divided by the rate served. It compares the *cost* of proxying even when the targets reach different rates, or when the load generator is the bottleneck.
- **pprof and flame graphs.** Go's built-in sampling profiler records where CPU time and allocations go. A flame graph draws the call stacks as stacked bars: the wider the bar, the more time.
- **Allocations and GC pressure.** Every heap allocation is work for the garbage collector later. Fewer allocations per request means less GC CPU and fewer pauses.

### Trade-offs and rejected alternatives

- **vegeta over wrk2.** Both avoid coordinated omission. vegeta builds with `go install` (no C toolchain), runs in a container next to the targets, and writes per-request results, which failure injection needs for per-second error counts.
- **Pinned CPUs and host networking over the Docker bridge.** On the bridge network, every target, including the bare backend, capped at about 12–13k req/s, so the bridge was being measured, not the proxies. Each container now gets its own vCPUs (`--cpuset-cpus`), and all of them share the VM's loopback.
- **Interleaved runs (A B C D, A B C D…) over back-to-back runs.** Host noise then lands on every target alike instead of penalising whichever ran last.
- **Removing allocations rather than pooling them.** `sync.Pool` is used where ownership is clear: the 32 KiB body-copy buffers (`internal/proxy/proxy.go`). Header maps and requests are handed to `net/http`, which may still hold them after the handler returns, so pooling them would risk use-after-free bugs. They were made cheaper instead: a one-pass header copy and shared constant slices.
- **Reporting CPU per request instead of only req/s,** because the generator, not the proxy, set the throughput ceiling.

### Likely interview questions

1. **How does Portcullis compare with NGINX?** Honestly: NGINX wins on CPU (22–31% less per request), on tail latency (p99 under 1 ms vs 1.5–8.7 ms) and on memory (5 vs 13–35 MiB). Portcullis beats Go's own `httputil.ReverseProxy` by 6–12% in CPU per request with the same transport settings, while doing routing, health tracking, retries and metrics on top. All figures are from `bench/results/2026-11-20`.
2. **What did profiling show, and what did you change?** 67% of end-to-end CPU is `read`/`write` system calls, and the proxy handler is about 1.3%. Two rounds cut handler allocations from 31 to 17 and ns/op by 27%: a one-pass hop-by-hop header filter instead of clone-then-delete, shared constant header slices, one backing array for per-request headers, and copying the request only for actual retries.
3. **How do you avoid fooling yourself with benchmarks?** Open-loop latency, 5 interleaved runs with medians *and* min–max spread, pinned CPUs, the same transport settings for the Go baseline, every raw report committed, and host load recorded per run. That last one is what showed this run's latency numbers were unusable, and the doc says so instead of quoting them.
4. **What happens when a backend dies under load?** 0 failed requests out of about 90k across 3 runs. The first dial failure ejects the backend passively, the idempotent request retries on the other backend, and the active health check re-admits it within 240–464 ms of its return (NGINX: 1.2–1.8 s).
5. **Why is panic mode on by default?** Under a CPU stall, every health check times out together and every healthy backend gets marked down. Without panic mode, that run returned 10.3% 503s. With panic mode, a pool where every backend looks down sends traffic to all of them instead of to none (ADR 0009).

### What went wrong

- **The first benchmarks measured Docker, not the proxies.** The bare backend was as slow as the proxies on the bridge network. Fixed with host networking on the VM loopback.
- **The benchmark filled the disk and broke Docker.** Storing every response body produced gigabytes, the VM's ext4 volume remounted read-only, and Docker would not start. The harness now never stores bodies, stops below 3 GiB free, and saves results after every run.
- **A real bug found by a benchmark:** correlated health-check false positives (see question 5), now ADR 0009.
- **The final run was on a busy machine.** Recording host load per run turned "the numbers look odd" into evidence. Even NGINX's p99 swung from 1.5 to 422 ms. Only the comparisons that held up are quoted, and the keep-alive-off hang is recorded as an open harness defect in the roadmap.

## Phase 5: PolicyScript

### Concepts, in plain language

- **Lexer → parser → checker → interpreter.**
  - The lexer cuts text into tokens (`if`, `"admin"`, `&&`).
  - The parser arranges tokens into a tree by the grammar. *Recursive descent* means one function per grammar rule, each calling the next tighter rule, so operator precedence falls out of the call order.
  - The checker walks the tree and proves the types line up before anything runs.
  - The interpreter walks the checked tree for each request.
- **Static type checking.** Errors like "comparing a string with an int" are found when the config is loaded, with `line:col`, not at 3 a.m. on a live request.
- **Termination by construction.** With no loops, recursion or user functions, every program is a finite tree evaluated at most once per node. The step budget is a second fence, not the only one.
- **Token bucket.** A bucket holds N tokens and refills N per window. Each request takes one. It allows short bursts while capping the average.
- **Sharding.** Splitting one map + lock into 64, picked by key hash, so concurrent requests rarely wait on the same lock.
- **Fail closed vs fail open.** When the policy itself breaks, either refuse the request (safe, less available) or skip the policy (available, less safe). Security rules should fail closed; cosmetic ones can fail open.
- **Fuzzing.** Go's fuzzer mutates inputs to find ones that crash the code. Here it feeds random text through the whole pipeline.

### Trade-offs and rejected alternatives

- **Tree-walking interpreter vs bytecode VM.** A VM would be faster, but evaluation is already about 0.4 µs against a ~3 µs proxy core. The tree walker is easier to read, explain and audit (ADR 0010).
- **Embedding Lua, Starlark or CEL.** The brief requires a hand-written language. Also, a general language would need sandboxing to guarantee termination and no I/O; ours cannot express those things in the first place.
- **Atomic effects per policy.** Buffering header writes costs a little. In return, a budget breach halfway through can never leave a half-modified request.
- **Literal-only lists.** CIDRs are parsed once at load time. Membership checks then never allocate, and a bad CIDR is a load error, not a runtime surprise.
- **Rate-limit state keyed by call site, handed over on reload.** Otherwise every config push from the controller would reset everyone's counters, a free bypass.

### Likely interview questions

1. **How do you guarantee a policy can't hang the proxy?** There are three fences. The grammar has no loops, recursion or functions, so evaluation visits each node at most once. Programs are capped at 64 KiB and 10,000 nodes. At runtime there is a step budget (default 10,000) and a time budget (default 1 ms, checked every 64 steps). `FuzzCompile` ran 4.9M random programs through compile and evaluation without a panic.
2. **What happens when a policy breaks at runtime?** Its effects are discarded, the error is counted (`portcullis_policy_errors_total`) and logged, and `onError` decides: `fail_closed` (default) answers 500, `fail_open` skips the policy and the next one still runs. Both are tested end to end (`TestPolicyBudgetFailClosedAndOpen`).
3. **How is the rate limiter safe under concurrency, and how is memory bounded?** 64 shards, each a mutex plus map. A concurrency test with 32 goroutines checks that a 1,000-token bucket grants exactly 1,000 (±1 for refill). Each limiter holds at most 100k keys. A full shard first drops idle keys, which is invisible because a new key starts full, then an arbitrary eighth.
4. **Could a policy enable request smuggling or header injection?** Framing headers (`Content-Length`, `Transfer-Encoding`, `Host`, …) are rejected when the policy loads. A computed header value or path containing CR, LF or NUL is a runtime error, so it never reaches the wire.
5. **Does a hot reload reset rate limits?** No, as long as the call site is unchanged (same policy, position, limit and window): `config.Compile` reuses the previous snapshot's limiter, just as it reuses backend health. `TestPolicyExamplesEndToEnd` exhausts a client's bucket, reloads, and still gets 429.

### What went wrong

- **A nil-map panic in the response phase.** I built the request `Env` but forgot to set the response status and headers before running response policies, so the first `remove_header` crashed the request. The end-to-end test caught it immediately. The fix sets them in `serveResponse`, and the interpreter now creates a header map if a caller passes none.
- **The evaluation state escaped to the heap.** It kept a slice pointing at its own array, which costs 576 bytes per request even for a one-line policy. A fixed array plus a count cut the floor from 208 ns and 1 allocation to 116 ns and 0 (`go build -gcflags=-m` shows it).
- **A wall-clock budget is a flaky test oracle.** Under `-race` on a busy laptop, a 200-rule policy passed 1 ms of real time without exceeding its step budget. The step test now uses a generous timeout, and the time budget has its own test with a 1 ns limit, which must always fire.
- **My own test expectations were wrong three times** (two `line:col` positions I counted by hand, one subtest whose backends shut down when it ended). Each time the code was right. Counting columns by hand is not a test; the tests now pin positions the parser reports, and I checked each one against the source by hand.

## Phase 6: Kubernetes control plane

### Concepts, in plain language

- **Control plane vs data plane.** The data plane (`portcullis`) moves bytes. The control plane (`portcullis-controller`) decides what the data plane should do and tells it. They are separate Deployments, so a controller crash never drops traffic: the data plane keeps serving its last config.
- **Gateway API.** The Kubernetes-standard way to describe L7 routing.
  - `GatewayClass` says which controller handles it.
  - `Gateway` is a listener ("HTTP on port 80").
  - `HTTPRoute` maps hostnames, paths, headers and methods to Services with weights.
- **Reconcile loop.** Instead of reacting to each event ("pod X deleted"), the controller repeatedly asks "what should the world look like?", computes it from scratch and makes it so. That is *level-triggered*: missing an event cannot leave things wrong, because the next reconcile sees the full state.
- **EndpointSlices.** Kubernetes' list of the actual pod IPs behind a Service, with readiness. Routing straight to pod IPs, instead of through the Service's virtual IP, lets Portcullis do its own load balancing, health tracking and retries.
- **Status conditions.** How a controller reports back on objects it handles. `Accepted` means "I understood this"; `ResolvedRefs` means "every reference points at something real"; `Programmed` means "the data plane acknowledged it".
- **CRD.** A custom resource definition teaches the API server a new object type, here `PortcullisPolicy`, so users manage policies with `kubectl` like anything else.
- **Policy attachment.** The Gateway API pattern of attaching extra behaviour to a Gateway or route through a separate object with a `targetRef`.

### Trade-offs and rejected alternatives

- **Push vs pull.** The controller pushes snapshots to each data plane's admin API. The data plane needs no Kubernetes credentials and no client library, and the same API works outside Kubernetes. The cost is that the controller must find every data-plane pod, which it does through a headless Service with `publishNotReadyAddresses`.
- **Full rebuild vs incremental updates.** A full rebuild is simple and always consistent. Incremental updates scale further but are easy to get wrong (ADR 0012).
- **Reject vs approximate unsupported features.** A route with a regex match or a header filter is reported `PartiallyInvalid` and that rule is dropped, instead of being served without the filter.
- **Generated PolicyScript for edge cases.** No valid backend → `deny(500)`; no ready endpoints → `deny(503)`. That reuses the policy engine instead of adding special cases to the data plane.
- **controller-gen vs hand-written deep copies.** For three structs, hand-written copies with a test are cheaper than another tool in the build.

### Likely interview questions

1. **Walk me through what happens when I apply an HTTPRoute.** The watch fires, everything is listed and `Translate` turns it into one Portcullis config. The controller validates it with the data plane's own `config` package, versions it by its hash, and PUTs it to every data-plane pod. Each pod's reply must carry the same hash. Then the route gets `Accepted`, `ResolvedRefs` and `Programmed`.
2. **A pod is deleted. How does traffic move?** The EndpointSlice drops the pod (it becomes `terminating`), the controller pushes a config without it, and the data plane swaps its snapshot atomically (Phase 3). Requests already sent to the pod finish. Meanwhile passive ejection and retries on connect failures cover the gap.
3. **What if the controller is down?** Data planes keep serving their last config. A new data-plane pod stays unready (`-wait-for-config`), so it never receives traffic before it is programmed.
4. **How do you keep a bad policy from breaking everything?** Each `PortcullisPolicy` is compiled alone with the data plane's parser. A bad one gets `Accepted=False, reason Invalid` with `line:col`, and is simply left out of the snapshot.
5. **What permissions does each part have?**
   - The controller can read Gateway API objects, policies, Services and EndpointSlices, write only their status, and use Leases in its namespace.
   - The data plane has no Kubernetes token at all.
   - A NetworkPolicy lets only the controller reach the admin port, which also requires a bearer token from a Secret generated at install.

### Measured (`bench/results/evidence/phase6-demo-e2e.txt`)

- **Canary 90/10:** 180/20 of 200 requests in `make demo`, and 450/50 of 500 in `make e2e`. That is exact, because each of the two data-plane replicas runs the same smooth-weighted cycle.
- **Failover:** every v1 pod deleted under load. 200/200 requests at 10 req/s in the demo and 399/399 at 20 req/s in the e2e test returned 200, as traffic moved to v2 and back.
- **Policy:** `/admin` → 403 with the policy's message. A deliberately broken `PortcullisPolicy` reported `Invalid` at `2:19` within seconds, while traffic and the valid policy were unaffected.

### What went wrong

- **`make tools` had always broken in a path with a space** ("Local Files"): `GOBIN=$(TOOLS_BIN)` was unquoted. Earlier phases never ran it from scratch. It is quoted now.
- **`kind load docker-image` failed** with "content digest … not found". Docker Desktop's containerd image store keeps multi-platform image indexes, but only the local platform's layers. kind imports `--all-platforms` and trips on the missing ones. The demo now saves each image for the node's platform (`docker save --platform linux/arm64`) and loads that archive.
- **Pods refused to start:** "runAsNonRoot and image has non-numeric user (nonroot)". The kubelet can only verify a numeric UID. The Dockerfile now says `USER 65532:65532`, and the manifests set `runAsUser` too.
- **The controller image took 13 minutes to build** on a loaded laptop (client-go is large), and any doc edit would have rebuilt it. The Dockerfile now copies only `cmd/` and `internal/` and uses a build-cache mount.
- **Go 1.27 became the minimum**, because controller-runtime v0.25 requires it. Recorded in ADR 0001's update; CI and the Docker image moved together.
