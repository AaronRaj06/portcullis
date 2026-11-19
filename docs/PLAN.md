# Plan

Task breakdown per phase. Each phase ends with: `go test -race ./...`, docs updated, commit, summary, stop.

## Phase 0: Scaffold ✅
- [x] Environment check
- [x] Module, layout, package docs, version-only binaries
- [x] Makefile (build, test, lint, vuln, fuzz, docker), golangci-lint config, CI workflow
- [x] Distroless non-root Dockerfile
- [x] docs skeleton, ADRs 0001–0002, interview notes

## Phase 1: Core proxy
- [ ] `proxy.Handler`: build outbound request, `http.Transport` round trip, copy response
- [ ] Hop-by-hop header stripping (RFC 9110 §7.6.1, plus headers named in `Connection`)
- [ ] `X-Forwarded-For/Proto/Host`, `Via`; trusted-proxy CIDR list
- [ ] Streaming copy with flush for SSE / `Transfer-Encoding: chunked`; pooled copy buffers
- [ ] Cancellation via request context; max body size via `http.MaxBytesReader`
- [ ] Error mapping: dial fail → 502, no healthy backend → 503, timeout → 504; JSON body with request ID
- [ ] Server timeouts (read-header, idle, write), dial and response-header timeouts, max header bytes
- [ ] Request ID middleware; `slog` JSON access log; basic Prometheus metrics
- [ ] Graceful shutdown with drain timeout
- [ ] Integration tests: headers, streaming, client cancel, 502, 504, drain, slowloris

## Phase 2: Routing, load balancing, health
- [ ] Route matcher: host, exact/prefix path, method, header; specificity ordering
- [ ] `Balancer` interface; RR, weighted RR (smooth), least-conn, P2C, consistent hash with vnodes (header/cookie/IP/path key)
- [ ] Active health checks with thresholds and jitter
- [ ] Passive ejection after N failures, timed re-admission
- [ ] Retries: idempotent only, only if no request bytes sent; retry budget
- [ ] Distribution tests per algorithm (chi-square-style tolerance), ejection tests, non-idempotent no-retry test

## Phase 3: Config and hot reload
- [ ] YAML schema, defaults, validation with field paths in errors
- [ ] Immutable compiled snapshot behind `atomic.Pointer`; version + hash
- [ ] SIGHUP reload; admin API: `/healthz`, `/readyz`, `/metrics`, `GET/PUT /config`, `POST /reload`, optional pprof, bearer token
- [ ] Test: sustained load + 100 reloads, count failed requests (target 0); invalid config never replaces good one

## Phase 4: Benchmarks (v0.1.0)
- [ ] Install wrk2 or vegeta, nginx
- [ ] Harness: direct, Portcullis, `httputil.ReverseProxy`, NGINX; small/large bodies; keep-alive on/off; concurrency sweep; ≥3 runs, medians
- [ ] p50/p95/p99, RPS, CPU, memory; failure-injection scenario
- [ ] pprof; ≥2 optimisation rounds with before/after
- [ ] `docs/BENCHMARKS.md`; tag v0.1.0

## Phase 5: PolicyScript
- [ ] `docs/POLICY_LANGUAGE.md` first
- [ ] Lexer, recursive-descent parser, type checker with `line:col` errors
- [ ] Evaluator with step budget and time budget; fail-closed/fail-open
- [ ] Built-ins and actions; sharded token-bucket rate limiter with idle eviction
- [ ] Fuzz (lexer, parser) with seed corpus; property tests; benchmarks
- [ ] Wire into request/response path; e2e test for admin lockdown, rate limit, canary

## Phase 6: Kubernetes control plane
- [ ] Install kind; controller-runtime + gateway-api deps
- [ ] Reconcile GatewayClass, Gateway, HTTPRoute; EndpointSlice resolution
- [ ] Snapshot translation, push with ack and backoff; status conditions
- [ ] `PortcullisPolicy` CRD with validation in status
- [ ] Manifests with least-privilege RBAC; `make demo`; e2e test

## Phase 7: Polish (v0.2.0)
- [ ] README with Mermaid diagram and benchmark table
- [ ] Gateway API conformance run, honest table
- [ ] `docs/SECURITY.md` threat model; asciinema demo script; `docs/RESUME_BULLETS.md`
