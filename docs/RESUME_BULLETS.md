# Résumé bullets

Every number below comes from a recorded result in this repository, and the source is given after each bullet. Figures that were measured on a loaded host and could not be trusted (see `docs/BENCHMARKS.md`, Limitations) are not used.

## Short version (pick three)

- Built **Portcullis**, an L7 reverse proxy, load balancer and Kubernetes Gateway API controller in Go (about 6,900 lines plus 5,200 lines of tests). Its iRules-style policy language evaluates a 5-rule policy in **372 ns with 1 allocation**.
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

- Built a Kubernetes controller for Gateway API (`GatewayClass`, `Gateway`, `HTTPRoute`) and a `PortcullisPolicy` CRD. A pure translation layer feeds hash-versioned config snapshots, which are acknowledged by every data-plane pod and retried with back-off. *(ADR 0012)*
- Wrote a one-command kind demo and an end-to-end suite. Results: an exact **90/10 canary (450/50 of 500)**, **399/399 requests succeeding** while every v1 pod was deleted under load, and an invalid policy rejected with its `line:col` without affecting valid ones. *(`bench/results/evidence/phase6-demo-e2e.txt`)*
- Ran the upstream Gateway API conformance suite and published the honest pass/fail table, including the features not supported. *(docs/CONFORMANCE.md)*

## Talking points (no numbers needed)

- **Why a full rebuild per reconcile:** consistency over efficiency, with the limit written down (ADR 0012).
- **Why unsupported Gateway API features are rejected, not approximated:** serving a route without its header filter is silently wrong.
- **Where the design stops:**
  - rate limits are per instance (ADR 0011);
  - HTTPS listeners are not programmed by the controller (docs/ROADMAP.md).
