# ADR 0001: Module path and Go version

- Status: accepted
- Date: 2026-11-19

## Context

The brief left the GitHub user as a placeholder, and asked for Go 1.23 or newer.

## Decision

- Module path `github.com/AaronRaj06/portcullis`, taken from the logged-in `gh` account.
- `go.mod` declares `go 1.23.0` as the minimum. Development happens on Go 1.27.4; CI uses the latest Go that satisfies `go.mod`.

## Consequences

- Code must not use language or stdlib features newer than 1.23 unless we raise the minimum (record it in a new ADR).
- Later dependencies (controller-runtime, gateway-api) may force a higher minimum. `go mod tidy` will raise it; we accept that and note it here.
- Renaming the module later means a mechanical find-and-replace of the import path.

## Update (Phase 1)

Adding `prometheus/client_golang v1.25.0` raised the minimum to **Go 1.26** (its own `go.mod` requires it). We accepted this rather than pinning an older client_golang with older transitive dependencies. The installed Go 1.27.4 already satisfies it, so nothing on the machine changes (ADR 0003). The Docker build stage now uses `golang:1.26`.
