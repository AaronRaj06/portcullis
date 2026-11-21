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

## Update (first CI run)

The first GitHub Actions run failed `govulncheck`: `setup-go` read `go 1.26.0` from `go.mod` and installed exactly 1.26.0, whose `net/http` has GO-2026-6859 (fixed in 1.26.11). Local runs on Go 1.27.4 had not shown it. Fixes:

- `go.mod` now says `toolchain go1.26.11`: anyone building with an older 1.26 gets the patched toolchain automatically (into the module cache, nothing system-wide; ADR 0003).
- CI installs `1.26.x` (newest patch) instead of reading the minimum from `go.mod`.

Lesson: the `go` line is a *minimum*, and CI should test what users actually build with (the newest patch), not the floor.

## Update (Phase 6): Go 1.27

Adding `sigs.k8s.io/controller-runtime v0.25.3` (with `k8s.io/* v0.37`) raised the minimum to **Go 1.27**. The same reasoning applies as for client_golang: accept the newer minimum rather than pin older Kubernetes libraries. `go.mod` says `go 1.27.0` with `toolchain go1.27.4` (the installed version). CI installs `1.27.x`, and the Docker build stage uses `golang:1.27`.
