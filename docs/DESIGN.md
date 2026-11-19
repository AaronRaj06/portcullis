# Design

Filled in phase by phase. This file explains *how* Portcullis works; ADRs in `adr/` explain *why* a choice was made.

## Big picture

```
            Gateway API objects + PortcullisPolicy
                          │ watch
                ┌─────────▼──────────┐
                │ portcullis-controller │  (control plane)
                └─────────┬──────────┘
                          │ PUT /config (versioned snapshot)
 client ──HTTP──▶ ┌───────▼────────┐ ──▶ backend pods
                  │   portcullis    │  (data plane)
                  │ route → policy → LB → upstream
                  └────────────────┘
```

- **Data plane**: the process that actually handles user traffic. It must be fast and must never stop serving because of a bad config.
- **Control plane**: the process that decides what the data plane should do, and tells it. It can be slow; it must be correct.

## Package boundaries

| Package | Owns | Depends on |
|---|---|---|
| `config` | schema, validation, immutable snapshot | `policy` (to validate scripts) |
| `proxy` | forwarding, headers, streaming, errors | `config`, `lb`, `policy`, `metrics` |
| `lb` | `Balancer` interface and algorithms | – |
| `health` | active checks, passive ejection | `lb` |
| `policy` | PolicyScript language | `ratelimit` |
| `ratelimit` | token buckets | – |
| `metrics` | Prometheus collectors | – |
| `admin` | admin HTTP API | `config`, `metrics` |
| `controller` | Gateway API reconciliation, snapshot push | `config`, `policy` |

Rule: `proxy` is the only package on the hot path that sees the whole request; everything else gets small, explicit inputs so it can be unit-tested alone.

## Sections to come

- Phase 1: request lifecycle, header handling, timeouts.
- Phase 2: routing table, balancers, health.
- Phase 3: snapshot swap and reload.
- Phase 5: PolicyScript evaluation model.
- Phase 6: controller reconciliation and status.
