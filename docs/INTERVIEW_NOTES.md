# Interview notes

One section per phase: concepts, trade-offs, likely questions, and one thing that went wrong.

## Phase 0: Scaffold

### Concepts, in plain language

- **Reverse proxy**: a server that sits in front of your real servers (*backends* or *upstreams*). Clients talk to it; it forwards each request to a backend and passes the answer back. Clients never see the backends directly.
- **L7 (layer 7)**: the proxy understands HTTP itself (paths, headers, methods), not just TCP bytes. That is what lets it route `/api` and `/static` to different places. An **L4** balancer only sees IPs and ports.
- **Data plane vs control plane**: the data plane moves user traffic and has to be fast. The control plane decides the configuration and pushes it. Splitting them means a slow or crashed controller does not stop traffic.
- **`internal/` packages**: Go refuses to let code outside this module import anything under `internal/`. That keeps our API surface at zero, so we can refactor freely.
- **Race detector (`-race`)**: a test mode that instruments memory accesses and fails if two goroutines touch the same data without synchronisation. A proxy is heavily concurrent, so every test runs with it.
- **Distroless, non-root image**: the final container holds only our static binary and CA certificates, with no shell or package manager, and runs as an unprivileged user. Less to attack, and less to patch.

### Trade-offs and rejected alternatives

- **One module, many packages** rather than several modules: one version, simple refactors. A multi-module repo pays off only when parts are released separately.
- **Tools via pinned `go run`** rather than a `tools.go` file or global installs (ADR 0002). Reproducible with no install step. The cost is a slower first run.
- **`go 1.23.0` minimum** rather than the local 1.27. A wider compatibility promise, at the price of skipping newer features until we bump it on purpose.
- **Both staticcheck and golangci-lint**: golangci-lint already bundles staticcheck, so this is partly redundant. The brief asks for both, and running standalone staticcheck keeps its exact upstream behaviour.

### Likely interview questions

1. **Why separate the control plane and data plane?** Different failure and performance needs. The proxy has to keep serving its last good config even if the controller is down or pushes garbage. Here they are separate binaries and Deployments, talking only through a versioned `PUT /config`.
2. **Why Go for a proxy?** Cheap goroutines for one-per-connection concurrency, a production-grade `net/http` and TLS stack in the stdlib, static binaries, and good profiling (pprof). The cost is GC pauses and less control than C or Rust, which is why Phase 4 measures allocations.
3. **How do you keep builds reproducible?** `go.mod` and `go.sum` pin dependencies. Tools are pinned by version in the Makefile. `-trimpath` removes local paths. CI uses the same `make` targets as a laptop.
4. **What does your CI enforce?** gofmt, go vet, staticcheck, golangci-lint (including gosec), `go test -race`, a fuzz smoke run, govulncheck and a Docker build. A pull request cannot pass with formatting drift, data races or known-vulnerable dependencies.
5. **Why run as non-root on distroless?** If someone finds a bug that lets them run code, there is no shell, no package manager and no root to escalate with. It also makes image scanners quieter, because there is almost nothing to report.

### What went wrong

The first `make lint` failed inside staticcheck with `export data version 5 is greater than maximum supported version 2`. The pinned `staticcheck 2025.1.1` predates Go 1.27 and cannot read the compiler's newer package format. Fix: pin the current release, `v0.8.2`, and record the lesson in ADR 0002. Linters must keep up with the toolchain.
