# Portcullis: build brief for Claude Code

How to use: create an empty folder, run `claude` inside it, and paste everything below the line.
Replace `YOUR_GITHUB_USER` first.

---

You are my senior engineering partner. We are building **Portcullis**, a small but production-minded application delivery and security stack in Go. It is a portfolio project for an interview with F5, so it has to be technically honest, well tested, benchmarked and easy to explain.

## 0. About me and how to work with me

- I am a final-year CS student. I know Go basics and DSA, but I am new to proxies, load balancing and Kubernetes controllers. Teach as you build: define terms in plain language the first time they appear.
- Never fabricate results. Benchmark numbers, test results and conformance results must come from commands you actually ran. If something fails or is slower than expected, say so and explain why.
- Never present a stub as finished work. If something is deferred, record it in `docs/ROADMAP.md`.
- Work in phases (section 6). At the end of each phase: run the full test suite with `-race`, update docs, commit, then print a short summary (what works, what you measured, what is deferred, questions for me) and **stop and wait for my go-ahead**.
- Ask me a question only when you are truly blocked. Otherwise choose a sensible default, record it as a short ADR in `docs/adr/`, and continue.
- First action: check the environment (`go version`, `docker --version`, `kind --version`, `kubectl version --client`, `wrk` or `vegeta`, `make`). Tell me what is missing. Target Go 1.23 or newer. Everything must run free and locally, with no paid services.

## 1. What we are building

Three cooperating parts, one repository:

1. **Data plane: `portcullis`.** An L7 reverse proxy and load balancer.
2. **Policy engine: PolicyScript.** A small, safe, event-driven rule language the proxy runs on each request and response. It is inspired by F5 iRules.
3. **Control plane: `portcullis-controller`.** A Kubernetes controller that watches Gateway API resources and programs the data plane, with the control plane and data plane as separate deployments.

Non-goals: HTTP/3, a full WAF ruleset, full Gateway API support, any ML, any paid or cloud-only dependency.

## 2. Hard requirements: data plane

- **Forwarding:** build your own handler on `net/http` and `http.Transport`. Do not use `httputil.ReverseProxy` for the main path. Use it only as a baseline in benchmarks.
- **Correct proxy behavior:**
  - Strip hop-by-hop headers.
  - Handle `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host` and `Via`. Only trust a client-supplied `X-Forwarded-For` if the client IP is in a configured trusted-proxy list.
  - Stream request and response bodies. Flush for streaming and SSE responses.
  - Do not buffer entire bodies.
  - Propagate cancellation from client to upstream.
  - Return correct 502, 503 and 504 responses with a request ID.
- **Timeouts and limits (all configurable, with safe defaults):**
  - Read-header, idle and write timeouts.
  - Dial timeout.
  - Upstream response-header timeout.
  - Max header bytes and max request body size.
  - Slowloris resistance.
- **Routing:** match on host, path (exact and prefix), method and header. Use the most specific match.
- **Upstream pools and load balancing,** hand-written:
  - Round-robin and weighted round-robin.
  - Least-connections.
  - Power-of-two-choices.
  - Consistent hashing with virtual nodes. The key can be a header, cookie, client IP or path.
- **Health:**
  - Active HTTP health checks with thresholds and jitter.
  - Passive ejection after N consecutive failures, with timed re-admission.
  - Retries only for idempotent requests, and only when no request bytes have been sent upstream.
  - A retry budget so retries cannot amplify an outage.
- **Graceful shutdown and connection draining.**
- **TLS termination:** static cert and key, with TLS 1.2+ and sane ciphers. Keep the TLS config in one place so I can later add post-quantum hybrid key exchange.
- **Observability:**
  - Prometheus metrics: requests by route, status and upstream; latency histograms; in-flight; upstream health; retries; policy denials; config generation.
  - Structured JSON logs via `log/slog`.
  - A request ID on every request.
  - Optional pprof on the admin port only.
- **Admin API on a separate port:** `/healthz`, `/readyz`, `/metrics`, `GET /config` (current version and hash), `PUT /config` (versioned snapshot push from the controller, with optional bearer token), `POST /reload`.
- **Hot path rules:**
  - Config is an immutable snapshot swapped with `atomic.Pointer`. Requests that started on the old snapshot finish on it.
  - Validate before swapping. An invalid config is rejected and the old one keeps serving.
  - No global mutex on the request path.
  - Aim for near-zero allocations per request in the proxy core, and prove it with `-benchmem`.

## 3. Hard requirements: PolicyScript

Write `docs/POLICY_LANGUAGE.md` first (grammar, types, built-ins, semantics, limits, examples), get it consistent, then implement. Example:

```
policy "edge-rules" {
  on request {
    if path.starts_with("/admin") && !(client.ip in ["10.0.0.0/8", "192.168.0.0/16"]) {
      deny(403, "admin is internal only")
    }
    if header("user-agent").contains("sqlmap") { deny(403) }
    if rate_limit(client.ip, 100, "1m") { deny(429) }
    if header("x-canary") == "1" { route_to("canary") }
    set_header("x-frame-options", "DENY")
  }
  on response {
    remove_header("server")
  }
}
```

- **Design:** a hand-written lexer, parser (recursive descent) and static type checker with `line:col` error messages that point at the problem. Then evaluate with a tree-walking interpreter or a small bytecode VM.
- **Safety by construction:**
  - No loops, recursion or user-defined functions, so every policy terminates.
  - Hard caps on policy size, AST depth, and an instruction or step budget per evaluation.
  - A per-request evaluation time budget.
  - Configurable behavior on budget breach: fail-closed (default) or fail-open.
  - Pure built-ins only: no I/O, no network, no files, no clock beyond a deterministic time function.
- **Built-in actions:** `deny`, `allow`, `redirect`, `set_header`, `remove_header`, `rewrite_path`, `route_to`, `log`, `tag`.
- **Rate limiting:** a token bucket per key with sharded in-memory state. It needs bounded memory (evict idle keys) and a concurrency test.
- **Testing:**
  - Table-driven unit tests.
  - Go native fuzzing for the lexer and parser, with a seed corpus.
  - Property tests where useful, for example "a valid policy never panics" and "evaluation always terminates within budget".
  - Benchmarks for ns/op and allocs/op per evaluation.

## 4. Hard requirements: control plane

- Go, using `sigs.k8s.io/controller-runtime` and `sigs.k8s.io/gateway-api`.
- Reconcile `GatewayClass` (our controller name), `Gateway` and `HTTPRoute`:
  - Hostnames.
  - Path, header and method matches.
  - `backendRefs` with weights, which gives canary traffic splitting.
- Resolve Services through **EndpointSlices** so upstreams are real pod IPs, and update them as pods come and go.
- Translate the resources into a Portcullis config snapshot and push it to the data plane admin API. Each snapshot carries a version and a hash. The data plane acknowledges it, and the controller retries with backoff.
- Write proper status conditions (`Accepted`, `Programmed`, `ResolvedRefs`) on the Gateway and HTTPRoute resources, with clear reasons.
- Deploy the controller and the data plane as separate Deployments with least-privilege RBAC.
- **Policy attachment (the headline feature):** define a small CRD `PortcullisPolicy` (`portcullis.dev/v1alpha1`) with a `targetRef` pointing at a Gateway or HTTPRoute and a `spec.script` containing PolicyScript. The controller validates the script with the same parser the data plane uses and reports errors in `status`.

## 5. Engineering standards

- **Layout:**
  - `cmd/portcullis/`, `cmd/portcullis-controller/`
  - `internal/{proxy,lb,health,config,policy,ratelimit,metrics,admin,controller}/`
  - `bench/` (scripts, raw results), `deploy/` (Dockerfile, kind config, manifests), `docs/`, `test/{integration,e2e}/`
- **Module path:** `github.com/YOUR_GITHUB_USER/portcullis`.
- **Dependencies:** stdlib first. Allowed: `prometheus/client_golang`, `controller-runtime`, `gateway-api`, a YAML library, `testify`. The load balancers, health checker, rate limiter and policy language must be written by hand, not imported.
- **Interfaces for testability:** for example `Balancer { Pick(ctx, req) (*Backend, error); Report(*Backend, Result) }`. Keep packages small with clear boundaries.
- **Quality gates in CI (GitHub Actions) and a Makefile:** `gofmt`, `go vet`, `staticcheck`, `golangci-lint`, `go test -race ./...`, `govulncheck`, a short fuzz smoke test, and a Docker build. Run containers as non-root on a distroless or scratch image.
- **Commits:** small, with conventional messages. Tag `v0.1.0` after Phase 4 and `v0.2.0` after Phase 7.

## 6. Phases (stop after each one)

**Phase 0: Scaffold.** Module, layout, Makefile, CI, lint config, `docs/` skeleton (README, DESIGN, ROADMAP, INTERVIEW_NOTES, adr/), and a `docs/PLAN.md` with your task breakdown. *Done when:* `make test lint build` passes on an empty skeleton.

**Phase 1: Core proxy.** Single upstream, streaming, correct headers, timeouts, graceful shutdown, request IDs, basic metrics. *Done when:* integration tests cover header handling, streaming, client cancel, upstream down (502), slow upstream (504) and shutdown draining.

**Phase 2: Routing, load balancing and health.** Everything in section 2 under routing, pools, health and retries. *Done when:* unit and integration tests show correct distribution for each algorithm, ejection and re-admission, and no retries on non-idempotent requests.

**Phase 3: Config and hot reload.** The config schema, validation, the immutable snapshot, SIGHUP and the admin API. *Done when:* a test sustains load while reloading config 100 times and records the number of failed requests (the target is zero), and an invalid config never replaces a good one.

**Phase 4: Benchmarks and optimization** (milestone `v0.1.0`). Build a reproducible harness (`make bench`):
  - Compare direct backend, Portcullis, `httputil.ReverseProxy` and NGINX as a proxy on the same machine.
  - Use `wrk2` or `vegeta` so latency is not hidden by coordinated omission.
  - Run 3 or more runs per scenario and report medians.
  - Cover small and large bodies, keep-alive on and off, and several concurrency levels.
  - Record requests/sec, p50/p95/p99, CPU and memory.
  - Add a failure-injection scenario that kills a backend under load and measures error rate and recovery time.
  - Profile with pprof and do at least two optimization rounds with before and after numbers (for example `sync.Pool` buffers, fewer allocations, a tuned `Transport`).
  - Write `docs/BENCHMARKS.md` with methodology, raw data links, flame graphs and limitations, including the cases where NGINX wins.

**Phase 5: PolicyScript.** The spec first, then the implementation as in section 3, integrated into the request and response path. *Done when:* the fuzz targets run clean for a fixed time, benchmarks are recorded, and an end-to-end test proves that the admin-lockdown, rate-limit and canary-routing examples work.

**Phase 6: Kubernetes control plane.** Everything in section 4. *Done when:* a one-command `make demo` brings up a `kind` cluster, installs the CRDs, the controller and the data plane, deploys two versions of a sample app, and shows a 90/10 canary split, a failover when pods are deleted, and a `PortcullisPolicy` blocking a path. Add a short end-to-end test that checks the same behaviors.

**Phase 7: Polish** (milestone `v0.2.0`).
  - README: the hook, a Mermaid architecture diagram, a quickstart, a benchmark summary table and a "Design decisions" section.
  - Run the Gateway API conformance suite for the HTTPRoute features we claim, and publish the honest pass/fail table.
  - Write a `docs/SECURITY.md` with a threat model.
  - Add a demo script suitable for recording with asciinema.
  - Write `docs/RESUME_BULLETS.md` with bullets whose numbers come only from recorded results.

## 7. Teaching deliverable (every phase)

Append to `docs/INTERVIEW_NOTES.md` a section for that phase with:
- the concepts used, in plain language
- the main trade-offs and alternatives you rejected
- 5 likely interview questions with concise answers grounded in this codebase
- one thing that went wrong or surprised us, and how it was resolved

## 8. Start now

Do the environment check, then Phase 0, then stop and report. Do not start Phase 1 until I say so.
