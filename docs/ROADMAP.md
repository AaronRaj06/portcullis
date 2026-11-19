# Roadmap

What is deferred, and why. Anything not finished is listed here, never presented as done.

## Deferred from Phase 0

- **Binaries do nothing yet.** `portcullis` and `portcullis-controller` only support `-version`; anything else exits 1 with "not implemented yet". Phase 1 and Phase 6 replace them.
- **Tools not yet installed locally:** `kind` (Phase 6), `wrk2` or `vegeta` (Phase 4), `nginx` (Phase 4 baseline). `staticcheck` and `govulncheck` run via `go run` (ADR 0002), so they need no install.
- **CI has not run on GitHub yet.** The workflow is written and every step passes locally, but no remote repository exists yet.

## Explicit non-goals

HTTP/3, a full WAF ruleset, full Gateway API support, ML, any paid or cloud-only dependency.

## Later ideas

- Post-quantum hybrid key exchange (X25519MLKEM768) in the single TLS config builder.
