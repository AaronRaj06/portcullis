# Roadmap

What is deferred, and why. Anything not finished is listed here, never presented as done.

## Deferred from Phase 0

- **`portcullis-controller` does nothing yet.** It only supports `-version` until Phase 6.
- **Tools:** `kind` and `vegeta` are installed project-locally by `make tools` (ADR 0003). NGINX will run as a Docker image in Phase 4. `staticcheck` and `govulncheck` run via `go run` (ADR 0002).
- **CI has not run on GitHub yet.** The workflow is written and every step passes locally, but no remote repository exists yet.

## Deferred from Phase 1

- **Upstream h2c** (HTTP/2 without TLS). HTTP/2 to TLS upstreams works; cleartext h2 would need `Transport.Protocols` and has no user yet (gRPC is not a goal).
- **Config file and full admin API** (`/readyz`, `GET/PUT /config`, `/reload`, pprof). Flags only until Phase 3; the admin port serves only `/metrics` and `/healthz`.
- **Per-request allocations not measured yet** (header clone, request copies, label formatting). Phase 4 measures with `-benchmem` and optimises.
- ~~WebSockets~~ fixed in Phase 2. ~~Upstream base path~~ obsolete: backends are `host:port`; path rewriting arrives with PolicyScript's `rewrite_path`. ~~503~~ done in Phase 2.

## Deferred from Phase 2

- **Multiple routes, weights and per-route upstream TLS from the CLI.** The flags build one catch-all route with equal weights. Phase 3's YAML config exposes everything the Go API already supports.
- **Weighted traffic splitting across pools** (one route → 90% pool A, 10% pool B), needed for Gateway API `backendRefs` canaries. Planned with the controller translation in Phase 6.
- **Panic mode / max ejection percentage.** If every backend gets ejected, we return 503 rather than ignoring ejection the way Envoy's panic threshold does.
- **Exponential back-off for repeatedly ejected backends.** Re-admission is a fixed `EjectFor`.
- **Upgraded connections ignore graceful drain.** Hijacked tunnels are not tracked by `Shutdown`; they close when the process exits.
- **Hash key `ip` uses the TCP peer,** not a trusted `X-Forwarded-For`. Behind another load balancer, use `header:` or `cookie:`.

## Explicit non-goals

HTTP/3, a full WAF ruleset, full Gateway API support, ML, any paid or cloud-only dependency.

## Later ideas

- Post-quantum hybrid key exchange (X25519MLKEM768) in the single TLS config builder.
