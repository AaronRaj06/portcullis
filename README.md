# Portcullis

A small, production-minded application delivery and security stack in Go:

- **`portcullis`**: an L7 reverse proxy and load balancer (the *data plane*).
- **PolicyScript**: a small, safe rule language the proxy runs on every request and response, inspired by F5 iRules.
- **`portcullis-controller`**: a Kubernetes controller that reads Gateway API resources and programs the data plane (the *control plane*).

> **Status: Phase 0 (scaffold).** Nothing serves traffic yet. See [docs/PLAN.md](docs/PLAN.md) and [docs/ROADMAP.md](docs/ROADMAP.md).

## Quickstart (developers)

Requires Go 1.23+, GNU make and Docker.

```sh
make help      # list targets
make all       # lint + test (-race) + build into bin/
make vuln      # govulncheck
make fuzz      # short fuzz smoke run (FUZZTIME=10s)
make docker    # distroless, non-root images
```

## Layout

| Path | Contents |
|---|---|
| `cmd/portcullis`, `cmd/portcullis-controller` | binaries |
| `internal/{proxy,lb,health,config,policy,ratelimit,metrics,admin,controller}` | one package per concern |
| `bench/` | benchmark scripts and raw results |
| `deploy/` | Dockerfile, kind config, manifests |
| `test/{integration,e2e}` | tests that span packages or need a cluster |
| `docs/` | design, ADRs, benchmarks, interview notes |
