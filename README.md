# Portcullis

A small, production-minded application delivery and security stack in Go:

- **`portcullis`**: an L7 reverse proxy and load balancer (the *data plane*).
- **PolicyScript**: a small, safe rule language the proxy runs on every request and response, inspired by F5 iRules.
- **`portcullis-controller`**: a Kubernetes controller that reads Gateway API resources and programs the data plane (the *control plane*).

> **Status: Phase 5 (PolicyScript).** A small, safe rule language, inspired by iRules, runs on every request and response: `deny`, `redirect`, `route_to`, `rewrite_path`, header changes, and per-client `rate_limit`. It has a hand-written lexer, parser and type checker with `line:col` errors, no loops (so every policy terminates), step and time budgets with fail-closed or fail-open handling, and policies that hot-reload with the config. The spec's main example evaluates in about 0.4 µs. See [docs/POLICY_LANGUAGE.md](docs/POLICY_LANGUAGE.md).
>
> Phase 4 (`v0.1.0`): `make bench` compares Portcullis with the bare backend, Go's `httputil.ReverseProxy` and NGINX. With keep-alive, Portcullis uses 6–12% less CPU per request than `httputil` and 22–31% more than NGINX. When a backend dies under load it fails no requests. See [docs/BENCHMARKS.md](docs/BENCHMARKS.md).
>
> Phase 3: traffic config from YAML/JSON, swapped atomically via SIGHUP or the admin API, with zero failed requests across 100 reloads under load.
>
> Earlier phases: Reverse proxy with Gateway-API-style routing, five hand-written load balancers, weighted canary splits, active and passive health checks (with back-off and optional panic mode), safe retries with a budget, WebSockets, HTTP/2 and h2c upstreams, TLS, graceful shutdown, request IDs and metrics. See [docs/PLAN.md](docs/PLAN.md) and [docs/ROADMAP.md](docs/ROADMAP.md).

```sh
make build
./bin/portcullis -upstream http://127.0.0.1:9000,http://127.0.0.1:9001 -lb p2c -listen :8080   # admin: 127.0.0.1:9901/metrics
./bin/portcullis -config docs/examples/portcullis.yaml               # full config with a policy; `kill -HUP` to reload
curl -X PUT --data-binary @new.yaml 127.0.0.1:9901/config           # or push it to the admin API
./bin/portcullis -help                                              # all flags and timeouts
GODEBUG=http2xconnect=1 ./bin/portcullis ...                        # optional: WebSockets over HTTP/2 (RFC 8441)
```

## Quickstart (developers)

Requires Go 1.26+, GNU make and Docker. Nothing else is installed globally: `make tools` builds kind and vegeta into `bin/tools/`.

```sh
make help      # list targets
make all       # lint + test (-race) + build into bin/
make vuln      # govulncheck
make fuzz      # short fuzz smoke run (FUZZTIME=10s)
make docker    # distroless, non-root images
make tools     # project-local kind + vegeta
make bench     # full load-test matrix in Docker (about an hour; needs a quiet machine)
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
