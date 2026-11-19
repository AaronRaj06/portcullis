# ADR 0002: Run Go dev tools with pinned `go run`

- Status: accepted
- Date: 2026-11-19

## Context

`staticcheck` and `govulncheck` were not installed locally. Installing tools globally makes builds depend on each machine's setup.

## Decision

The Makefile runs them as `go run <tool>@<pinned version>`. The module cache stores them after the first run. `golangci-lint` is the exception: it is a large binary with its own release channel, so it is installed separately and pinned in CI (`v2.15.0`).

## Consequences

- Nothing to install beyond Go and golangci-lint. Versions are the same everywhere.
- The first run needs network access and takes longer.
- Pins must be bumped by hand. We already hit this once: `staticcheck 2025.1.1` crashes on Go 1.27 export data, so it is pinned to `v0.8.2`.
