# Roadmap

What is deferred, and why. Anything not finished is listed here, never presented as done.

## Deferred from Phase 0

- **`portcullis-controller` does nothing yet.** It only supports `-version` until Phase 6.
- **Tools:** `kind` and `vegeta` are installed project-locally by `make tools` (ADR 0003). NGINX will run as a Docker image in Phase 4. `staticcheck` and `govulncheck` run via `go run` (ADR 0002).
- **CI has not run on GitHub yet.** The workflow is written and every step passes locally, but no remote repository exists yet.

## Deferred from Phase 1

- **Allocation optimisation.** Baseline measured now (`BenchmarkHandler`, in-memory transport): **~4.1 µs, 3.4 KB, 43 allocs per request**, including `httptest.NewRecorder`'s own. Optimising is Phase 4, which needs this "before" number.

### Resolved (Phases 1–2)

| Issue | Resolution |
|---|---|
| WebSockets stripped | Upgrade tunnelling (`upgrade.go`) |
| No HTTP/2 to upstreams | TLS: automatic; cleartext: `h2c://` via `NewH2CTransport` |
| Upstream base path | Obsolete: backends are `host:port`; rewriting comes with PolicyScript |
| No 503 | 503 when no backend is available |
| Weights not on the CLI | `-upstream http://a:80=3,...` with `-lb weighted_round_robin` |
| No per-pool upstream TLS | `Pool.Transport`; `-upstream-ca` |
| No canary split across pools | `Route.Split` with weights, exact and lock-free |
| No panic mode | `Pool.Panic` / `-panic` (ADR 0006) |
| No ejection back-off | linear `EjectFor × n`, capped, resets when clean (ADR 0006) |
| Tunnels ignored the drain | `Handler.Drain`, called by `Serve` |
| Hash on `ip` used the TCP peer | real client from trusted `X-Forwarded-For` |
| Config file / admin API | Phase 3 (ADR 0007) |
| TLS certificate needed a restart | `CertReloader`, re-read on SIGHUP |
| No ordering for config pushes | `ETag` + `If-Match` → 412 on stale pushes |
| CA file contents not in the hash | hash covers referenced files |
| Pool transports rebuilt every reload | reused when their settings are unchanged |
| WebSockets from HTTP/2 clients got 502 | RFC 8441 translation (ADR 0008) |
| CI never ran on GitHub | private repo created; push blocked until the `gh` token has the `workflow` scope |

## Deferred from Phase 4

- **A clean benchmark run.** The committed run (`bench/results/2026-11-20`) ran on a busy host (load average 3–65 on 8 cores). Keep-alive throughput and failure injection are usable; open-loop latency is not. Re-run `make bench` on a quiet machine and check the `host load (max)` column in `summary.md`.
- **Keep-alive-off hang in the harness.** Each vegeta worker hits exactly one 10 s timeout per run, against every target including the bare backend. The suspected cause is VM socket state between interleaved targets, but it has not been diagnosed. Next step: drain TIME_WAIT between targets (not only between scenarios) and compare `nstat` ListenDrops before and after.
- **Portcullis tail latency under contention** (p99 8.7 ms vs NGINX 0.9 ms at 256 connections on one core). Worth a profile on a quiet host: scheduler and GC settings (`GOGC`, `GOMEMLIMIT`), and per-connection goroutine costs.

## Deferred from Phase 5

All five gaps recorded at the end of Phase 5 are closed (ADR 0011):

| Gap | Resolution |
|---|---|
| Rate limits per instance | `instances: N` divides limits across replicas (approximate; exact needs a shared store) |
| Limiter reset when its position moved | optional `name` argument: `rate_limit(key, 10, "1m", "login")` |
| Response phase skipped proxy errors and upgrades | now runs on 502/503/504, client-body errors and WebSocket handshakes |
| No regex or clock | `s.matches("re")` (RE2, linear time), `now()` (fixed per request) |
| 3 extra allocations with policies | 1: pooled `Env`, allocation-free `Limiter.AllowBytes` |

## By design (not planned)

- **Policies cannot read request bodies** (ADR 0011). That would mean buffering bodies before forwarding, which the brief forbids.

- **Listeners and server timeouts need a restart** (ADR 0007). Changing a listen address means rebinding a socket; the server-level timeouts are fixed when `http.Server` is built. Everything that changes routing is live, and so is the TLS certificate.
- **WebSockets over HTTP/2 need `GODEBUG=http2xconnect=1`** (ADR 0008), Go's own switch for RFC 8441. Without it, clients use HTTP/1.1 WebSockets, which always work.

## Explicit non-goals

HTTP/3, a full WAF ruleset, full Gateway API support, ML, any paid or cloud-only dependency.

## Later ideas

- Post-quantum hybrid key exchange (X25519MLKEM768) in the single TLS config builder.
