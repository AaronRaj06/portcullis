# Roadmap

What is deferred, and why. Anything not finished is listed here, never presented as done.

## Deferred from Phase 0

- **`portcullis-controller` does nothing yet.** It only supports `-version` until Phase 6.
- **Tools:** `kind` and `vegeta` are installed project-locally by `make tools` (ADR 0003). NGINX will run as a Docker image in Phase 4. `staticcheck` and `govulncheck` run via `go run` (ADR 0002).
- **CI has not run on GitHub yet.** The workflow is written and every step passes locally, but no remote repository exists yet.

## Deferred from Phase 1

- **Allocation optimisation.** Baseline measured now (`BenchmarkHandler`, in-memory transport): **~4.1 µs, 3.4 KB, 43 allocs per request**, including `httptest.NewRecorder`'s own. Optimising is Phase 4, which needs this "before" number.
- **Upgrades from HTTP/2 clients** (RFC 8441 extended CONNECT). HTTP/2 connections cannot be hijacked, so a WebSocket over an HTTP/2 client connection gets 502; browsers fall back to HTTP/1.1.

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
| CI never ran on GitHub | private repo created; push blocked until the `gh` token has the `workflow` scope |

## Deferred from Phase 3

- **Hot-reloading process-level settings** (listeners, TLS certificate, timeouts). Restart needed (ADR 0007). Certificate rotation without restart (`GetCertificate` reloading files) is a natural next step.
- **Ordering of pushes.** `version` is opaque, so the data plane cannot tell that an old snapshot arrived after a newer one. The controller pushes sequentially and retries only its latest snapshot; a monotonic `generation` precondition (`If-Match`) would make this explicit.
- **Pool transports are rebuilt on every reload** (h2c / private-CA pools only), costing new upstream handshakes. Reusing a transport when its TLS settings are unchanged is an easy optimisation.
- **The hash covers the config text, not referenced files.** Changing the contents of a `caFile` without changing the config is treated as "unchanged".

## Explicit non-goals

HTTP/3, a full WAF ruleset, full Gateway API support, ML, any paid or cloud-only dependency.

## Later ideas

- Post-quantum hybrid key exchange (X25519MLKEM768) in the single TLS config builder.
