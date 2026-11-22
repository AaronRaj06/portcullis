# 0013: Per-Gateway listeners, HTTPS, filters and tenant isolation

Status: accepted (v0.3.0). Supersedes the "rejected, not approximated" rows of ADR 0012 for HTTPS listeners and the two core filters.

## Context

v0.2.1 had one address for every Gateway, no HTTPS listeners and no filters: 11 of 40 core conformance tests failed. An audit before closing the project also found that one namespace could affect another: `route_to` reached any Service, policy names were global, a broken `fail_closed` policy was silently skipped, and 64 policies anywhere stopped every config push. A `/admin` deny could also be bypassed with `//admin`.

## Decision

- **Listeners are data, not flags.** The config gained `listeners[]` (a port, and certificates for HTTPS) and `routes[].port`. The data plane opens and closes ports on reload, binding new ports *before* the snapshot swap so a bind failure rejects the config. A route with a port is matched only on that port, with no fallback.
- **One Service per Gateway.** The controller gives each `(Gateway, protocol, port)` a target port in 10000–19999 and creates a Service with the data plane's selector. The Service is the durable record of the allocation; ports are found by hash with linear probing. The Service is written only after a successful push, so it never points at a closed port. A port whose protocol changes is rebound rather than rejected.
- **Certificates are pushed, not mounted.** The controller reads each referenced Secret (`get` only, re-read every 15 s) and pushes the PEM. This keeps the data plane free of Kubernetes credentials and lets certificates rotate without a restart. The admin channel is TLS with a pinned CA, and keys are redacted from `GET /config`.
- **Filters are native route fields**, not generated PolicyScript: header edits and redirects are a few lines in the proxy, and generated scripts would have counted against the policy limit.
- **A rule we cannot honour fails closed.** An unsupported filter makes its rule answer 500, because dropping it would let its traffic fall through to a broader rule. An unsupported match is still dropped (nothing can fall through to a match that does not exist).
- **Tenant isolation in the translator:** policy identity is `namespace/name`; `route_to` targets are limited to the policy's namespace; a policy that does not compile closes its routes unless `fail_open`; the 64-policy limit is per route; unsafe endpoint addresses are skipped.

## Consequences

- Every Gateway still shares one Deployment: isolation is of addresses and routes, not of resources.
- The whole config, certificates included, is rebuilt and pushed on every change (ADR 0012); `config.MaxSize` rose to 16 MiB to leave room for certificates (32 KiB each at most).
- Redirect scheme, port and path, other status codes, URL rewrites, mirrors, regex and query matches are still unsupported and reported.
