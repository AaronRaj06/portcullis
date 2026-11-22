# Résumé bullets

Every number below comes from a recorded result in this repository, and the source is given after each bullet. Figures that were measured on a loaded host and could not be trusted (see `docs/BENCHMARKS.md`, Limitations) are not used.

## Short version (pick three)

- Built **Portcullis**, an L7 reverse proxy, load balancer and Kubernetes Gateway API controller in Go that passes **all 40 core tests** of the upstream Gateway API conformance suite (v1.7.0, `GATEWAY-HTTP`). Its iRules-style policy language evaluates a 5-rule policy in **372 ns with 1 allocation**.
- In failure-injection runs at 1,000 req/s, killing one of two backends caused **0 failed requests out of 89,990**, in each of 3 runs. The backend was re-admitted **240–464 ms** after it came back, against 1.2–1.8 s for NGINX on the same harness.
- Two profiling-driven optimisation rounds cut per-request handler allocations by **49% (35 → 18)** and latency by **27%**. The result is **6–12% less CPU per request** than Go's standard `httputil.ReverseProxy`; NGINX is still 22–31% cheaper.

## Data plane

- Wrote five load-balancing algorithms by hand: round robin, smooth weighted round robin, least connections, power-of-two-choices and consistent hashing. Added active and passive health checks with back-off and panic mode, and retries limited to idempotent requests under a token-bucket retry budget. *(`internal/lb`, `internal/health`, ADR 0005)*
- Made config reloads atomic with an immutable snapshot behind `atomic.Pointer`. **0 failed requests across 100 reloads (10 deliberately invalid)** with 16 concurrent clients under the race detector, and an invalid config never replaced a working one. *(`TestHotReloadUnderLoad`, docs/PLAN.md Phase 3)*
- Built a reproducible benchmark harness avoiding coordinated omission (vegeta at a fixed rate, 5 runs per scenario, medians), comparing against NGINX and `httputil`. It documents the cases where NGINX wins: p99 under 1 ms against 1.5–8.7 ms, and about 5 MiB of memory against 13–35 MiB. *(docs/BENCHMARKS.md)*

## Policy language

- Designed and implemented **PolicyScript**, a sandboxed rule language: hand-written lexer, recursive-descent parser, type checker with `line:col` errors, and a tree-walking interpreter. It has no loops or I/O, a 10,000-step and 1 ms budget, RE2 regex, and a sharded token-bucket rate limiter. *(docs/POLICY_LANGUAGE.md, ADR 0010)*
- Two optimisation rounds (interpreter state moved to the stack after escape analysis, then a pooled environment and an allocation-free rate-limit lookup) took evaluation from **506 ns, 3 allocations to 372 ns, 1 allocation**. Fuzzed the compiler and evaluator for **9.6 million executions** with no failures. *(docs/BENCHMARKS.md, `bench/results/policy/`)*

## Control plane

- Built a Kubernetes controller for Gateway API (`GatewayClass`, `Gateway`, `HTTPRoute`, `ReferenceGrant`) and a `PortcullisPolicy` CRD. A pure translation layer feeds hash-versioned config snapshots, which are acknowledged by every data-plane pod and retried with back-off. HTTPS listeners select certificates by SNI and rotate them without a restart; each Gateway gets its own Service and address on a shared data plane. *(ADR 0012, ADR 0013)*
- Wrote a one-command kind demo and an end-to-end suite. Results: an exact **90/10 canary (450/50 of 500)**, **400/400 requests succeeding** while every v1 pod was deleted under load, and an invalid policy reported with its `line:col`. *(`bench/results/evidence/v0.3.0-e2e.txt`)*
- Took upstream Gateway API conformance from **22 to 40 of 40 core tests** (v1.7.0, `GATEWAY-HTTP`, suite unmodified) by adding HTTPS listeners, header and redirect filters, and per-Gateway isolation. Self-run; the report is committed. *(docs/CONFORMANCE.md, `bench/results/conformance/report.yaml`)*
- Audited the multi-tenant boundaries and fixed **7 security bugs**, each with a regression test: a path-normalisation bypass of policy denies (`//admin`), cross-namespace `route_to`, policy-name shadowing between namespaces, a broken `fail_closed` policy being skipped, a cluster-wide policy limit that one namespace could exhaust, endpoint addresses pointing the proxy at loopback or cloud metadata, and unbounded memory from split weights. *(docs/SECURITY.md, ADR 0013)*

## Talking points (no numbers needed)

- **Why a full rebuild per reconcile:** consistency over efficiency, with the limit written down (ADR 0012).
- **Why unsupported Gateway API features are rejected, not approximated:** serving a route without its header filter is silently wrong.
- **Where the design stops:**
  - rate limits are per instance (ADR 0011);
  - every Gateway shares one data-plane Deployment: separate addresses, shared resources (ADR 0013);
  - extended Gateway API features (rewrites, mirrors, regex matches) are not built (docs/ROADMAP.md).
