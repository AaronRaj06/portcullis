# Plan

Task breakdown per phase. Each phase ends with: `go test -race ./...`, docs updated, commit, summary, stop.

## Phase 0: Scaffold ✅
- [x] Environment check
- [x] Module, layout, package docs, version-only binaries
- [x] Makefile (build, test, lint, vuln, fuzz, docker), golangci-lint config, CI workflow
- [x] Distroless non-root Dockerfile
- [x] docs skeleton, ADRs 0001–0002, interview notes

## Phase 1: Core proxy ✅
- [x] `proxy.Handler`: build outbound request, `http.Transport` round trip, copy response
- [x] Hop-by-hop header stripping (RFC 9110 §7.6.1, plus headers named in `Connection`; keep `TE: trailers`)
- [x] `X-Forwarded-For/Proto/Host`, `Via`; trusted-proxy CIDR list; strip `Forwarded` from untrusted clients
- [x] Streaming copy with flush for SSE / unknown length; pooled copy buffers
- [x] Cancellation via request context; max body size via `http.MaxBytesReader`
- [x] Error mapping: 400/408/413 (client), 502/504 (upstream), JSON body with request ID. 503 needs pools → Phase 2
- [x] Timeouts: read-header, idle, per-write, per-body-read, dial, response-header; max header bytes (ADR 0004)
- [x] Request ID (trusted IDs kept); `slog` JSON access log; basic Prometheus metrics on the admin port
- [x] Graceful shutdown with drain timeout
- [x] TLS termination (TLS 1.2+, AEAD suites, PQ hybrid verified)
- [x] Integration tests: headers, streaming both ways, client cancel, 502, 504, 413, 408, drain, slowloris

## Phase 2: Routing, load balancing, health ✅
- [x] Route matcher: host (exact/wildcard), exact/prefix path, method, header; Gateway API specificity ordering
- [x] `Balancer` interface; RR, smooth weighted RR, least-conn, P2C, consistent hash with vnodes (header/cookie/IP/path key); lock-free, 0 allocs/pick
- [x] Active health checks with rise/fall thresholds and jitter
- [x] Passive ejection after N gateway failures, timed re-admission
- [x] Retries: idempotent only, only if no request bytes sent (httptrace), retry budget (ADR 0005)
- [x] 503 when no backend is available, 404 when no route matches
- [x] Distribution tests per algorithm (unit + through the proxy), ejection/re-admission, active health, no-retry tests
- [x] Phase 1 fixes: WebSocket/Upgrade tunnelling, HTTP/2 to TLS upstreams
- [x] Follow-ups: weighted split across pools, panic mode, ejection back-off, tunnel drain, per-pool transport (CA, h2c), real-client hashing, CLI weights, handler allocation baseline

## Phase 3: Config and hot reload ✅
- [x] YAML/JSON schema, strict parsing, defaults, validation reporting every problem with its field path
- [x] Immutable compiled snapshot behind `atomic.Pointer`; version + canonical hash + generation; no-op on equal hash
- [x] Backend health/ejection state carried across reloads; health checks restarted per snapshot
- [x] SIGHUP reload; admin API: `/healthz`, `/readyz`, `/metrics`, `GET/PUT /config`, `POST /reload`, optional pprof, bearer token (constant-time)
- [x] Test: 16 clients + 100 reloads (10 invalid): 0 failed requests; invalid config never replaces a good one; in-flight requests finish on the old snapshot
- [x] CLI flags converted to the same config path; end-to-end SIGHUP/PUT test through `run()`

## Phase 4: Benchmarks (v0.1.0)
- [x] Install wrk2 or vegeta, nginx
- [x] Harness: direct, Portcullis, `httputil.ReverseProxy`, NGINX; small/large bodies; keep-alive on/off; concurrency sweep; ≥3 runs, medians
- [x] p50/p95/p99, RPS, CPU, memory; failure-injection scenario
- [x] pprof; ≥2 optimisation rounds with before/after
- [x] `docs/BENCHMARKS.md`; tag v0.1.0

## Phase 5: PolicyScript
- [x] `docs/POLICY_LANGUAGE.md` first
- [x] Lexer, recursive-descent parser, type checker with `line:col` errors
- [x] Evaluator with step budget and time budget; fail-closed/fail-open
- [x] Built-ins and actions; sharded token-bucket rate limiter with idle eviction
- [x] Fuzz (lexer, parser) with seed corpus; property tests; benchmarks
- [x] Wire into request/response path; e2e test for admin lockdown, rate limit, canary

## Phase 6: Kubernetes control plane
- [x] Install kind; controller-runtime + gateway-api deps
- [x] Reconcile GatewayClass, Gateway, HTTPRoute; EndpointSlice resolution
- [x] Snapshot translation, push with ack and backoff; status conditions
- [x] `PortcullisPolicy` CRD with validation in status
- [x] Manifests with least-privilege RBAC; `make demo`; e2e test

## Phase 7: Polish (v0.2.0)
- [x] README with Mermaid diagram and benchmark table
- [x] Gateway API conformance run, honest table (`docs/CONFORMANCE.md`)
- [x] `docs/SECURITY.md` threat model; asciinema demo script (`deploy/record-demo.sh`); `docs/RESUME_BULLETS.md`

## v0.2.1: Fixes and closure
- [x] ReferenceGrant, namespace selectors, route kinds, status fixes; snapshot-breaking bugs
- [x] Conformance rerun (29 of 40); v0.2.0 report corrected

## v0.3.0: Closure
- [x] HTTPS listeners with SNI and live certificate rotation; one Service and address per Gateway
- [x] RequestHeaderModifier and RequestRedirect filters; invalid split shares answer 500
- [x] Tenant isolation fixes (route_to, policy names, fail-closed invalid policies, per-route policy limit, endpoint addresses); path-normalisation bypass; split-table memory cap
- [x] Admin API over TLS; private keys redacted; conformance suite run unmodified
