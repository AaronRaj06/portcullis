# Roadmap

What is deferred, and why. Anything not finished is listed here, never presented as done.

## Deferred from Phase 0

- **`portcullis-controller` does nothing yet.** It only supports `-version` until Phase 6.
- **Tools:** `kind` and `vegeta` are installed project-locally by `make tools` (ADR 0003). NGINX will run as a Docker image in Phase 4. `staticcheck` and `govulncheck` run via `go run` (ADR 0002).
- **CI has not run on GitHub yet.** The workflow is written and every step passes locally, but no remote repository exists yet.

## Deferred from Phase 1

- **WebSockets / `Upgrade`.** `Upgrade` is stripped as a hop-by-hop header, so WebSocket handshakes do not pass through yet. Proper support means handling `101 Switching Protocols` and splicing both directions.
- **Upstream HTTP/2 and h2c.** The upstream side speaks HTTP/1.1 only. Clients can use HTTP/2 over TLS.
- **Upstream base path.** `-upstream` must have no path (`New` rejects one) instead of joining paths silently.
- **503.** There is no "no healthy backend" state until pools exist (Phase 2).
- **Config file and full admin API** (`/readyz`, `GET/PUT /config`, `/reload`, pprof). Flags only until Phase 3; the admin port serves only `/metrics` and `/healthz`.
- **Per-request allocations not measured yet.** The header clone, `WithContext` copy and label formatting all allocate. Phase 4 measures with `-benchmem` and optimises.

## Explicit non-goals

HTTP/3, a full WAF ruleset, full Gateway API support, ML, any paid or cloud-only dependency.

## Later ideas

- Post-quantum hybrid key exchange (X25519MLKEM768) in the single TLS config builder.
