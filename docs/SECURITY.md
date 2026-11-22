# Security

This document is the threat model for Portcullis: what it trusts, what an attacker can reach, what defends each boundary, and what is not defended. Every limit below is a constant or a default in the code, named so it can be checked.

Report vulnerabilities privately through GitHub's "Report a vulnerability" on this repository, not in a public issue.

## Assets

1. **Availability** of the proxy for legitimate clients.
2. **Integrity of the routing config.** Whoever writes the config decides where traffic goes, and can read it in transit.
3. **Integrity of policy decisions.** A bypassed `deny` exposes whatever it protected, such as `/admin`.
4. **The admin bearer token** and the TLS private keys.
5. **Upstream trust in forwarded headers.** Backends may believe `X-Forwarded-For` and `X-Request-Id`.

## Trust boundaries and actors

```
 internet client ──▶ [proxy :8080] ──▶ upstream pools
                         ▲
 controller ──token──▶ [admin :9901] ◀── operator (curl / SIGHUP / config file)
     ▲
 Kubernetes API ◀── route and policy authors (namespace-scoped RBAC)
```

| Actor | Trusted for | Not trusted for |
|---|---|---|
| Internet client | nothing | anything |
| Peer in `trustedProxies` | its `X-Forwarded-*` and `X-Request-Id` values | everything else |
| Upstream | its responses to us | the proxy's own headers or memory |
| Operator, controller (admin token) | the full config, which means all routing | n/a: full control |
| HTTPRoute / PortcullisPolicy author | routes and policies on the objects their RBAC allows | other namespaces' backends, unless a ReferenceGrant in that namespace allows them |

## Threats and defences

### Client to proxy

| Threat | Defence | Where |
|---|---|---|
| Slowloris (slow headers) | `ReadHeaderTimeout` 10 s; `IdleTimeout` 120 s between keep-alive requests | `internal/proxy/server.go` `DefaultLimits` |
| Huge headers | `MaxHeaderBytes` 64 KiB | same |
| Huge bodies | `-max-body-bytes`, 10 MiB by default: a declared `Content-Length` over the limit is refused with 413 before dialling, and a streamed body is cut by `http.MaxBytesReader`. Bodies are streamed, never buffered whole | `internal/proxy/proxy.go` |
| Spoofed client IP or request ID | `X-Forwarded-*` and `X-Request-Id` from a peer outside `trustedProxies` are replaced, not appended to. The client IP is found by walking `X-Forwarded-For` from the right, stopping at the first untrusted hop | `internal/proxy/headers.go` |
| Hop-by-hop header smuggling (`Connection: X-Internal-Auth`) | standard hop-by-hop headers and any header named in `Connection` are stripped in both directions | `removeHopByHop` |
| Request smuggling (CL/TE desync) | parsing is Go's `net/http`, which rejects ambiguous `Content-Length` plus `Transfer-Encoding`. The proxy never re-serialises raw bytes | stdlib |
| Retry storms amplifying an outage | retries only on idempotent methods, capped at 5 attempts, inside a token-bucket retry budget (20% by default) | ADR 0005, `internal/lb` |
| TLS downgrade | TLS 1.2 minimum on listeners and on upstream TLS; certificates reload from disk without a restart | `TLSConfigFrom`, `config/compile.go` |

### PolicyScript

Policies are code supplied by config authors and, through `PortcullisPolicy`, by namespace users, so the interpreter is a sandbox:

- **No I/O.** There is no file, network or environment access, and no loops or recursion: the grammar has neither.
- **Bounded size:** source 64 KiB, nesting depth 64, string literals 4 KiB, list literals 256 items (`internal/policy/lexer.go`).
- **Bounded time:**
  - 10,000 evaluation steps per run (`DefaultMaxSteps`).
  - A 1 ms wall-clock budget (`DefaultTimeout`).
  - A breach is a runtime error: fail-closed (500) by default, or fail-open per policy.
- **Linear-time regex:** `matches` uses RE2, and patterns are compiled and checked at load time, so there is no catastrophic backtracking.
- **Atomic effects:** a policy that errors applies none of its header edits.
- **Fuzzed:** the lexer, parser and evaluator have fuzz targets, run in CI (10 s each) and for longer during Phase 5.
- **Bounded rate-limiter memory:** 100,000 keys per limiter (about 100 B each). When it is full, about 1/8 of a shard is evicted.

### Admin API and config

| Threat | Defence |
|---|---|
| Unauthenticated config write | Everything except `/healthz`, `/readyz` and `/metrics` requires `Authorization: Bearer`, compared in constant time. With no token file the admin port binds to `127.0.0.1:9901` by default |
| Token leakage | read from a file (`-admin-token-file`), never from a flag or environment variable, so it does not show in `ps` or `/proc/*/environ`. In Kubernetes it is a Secret generated at install time (`openssl rand -hex 32`) |
| Malformed or hostile config | size limit `config.MaxSize`; strict parsing (unknown fields rejected); full validation and compilation before an atomic swap. An invalid config never replaces a working one |
| Lost update between two writers | `PUT /config` honours `If-Match` against the config hash |
| Profiling data exposure | `/debug/pprof` is off by default and sits behind the token when on |

### Kubernetes

- **Data plane:** no Kubernetes credentials (`automountServiceAccountToken: false`). It runs as non-root UID 65532 on distroless images.
- **Admin port isolation:** a NetworkPolicy lets only the controller reach port 9901. This depends on the CNI enforcing it.
- **Controller RBAC (least privilege):**
  - read Gateway API objects (ReferenceGrants included), PortcullisPolicies, Namespaces, Services and EndpointSlices;
  - write only `*/status`;
  - Leases and Events only in its own namespace.
- **Isolation between users:**
  - A route may reference Services in another namespace only if a ReferenceGrant there allows it.
  - Listener `allowedRoutes` is honoured. With a namespace selector, whoever can label a Namespace can let its routes attach.
  - A broken policy is rejected on its own and cannot block others.

## Known gaps (not defended)

- **Distributed rate-limit bypass.** Limits are per instance, divided by `instances`. Uneven load balancing lets a client exceed the global figure, by at most a factor of the replica count. An attacker rotating more than 100,000 keys forces evictions, which reset other keys' buckets. Use a per-IP key on a trusted client address, and do not treat `rate_limit` as abuse protection against a botnet.
- **No global connection limit or per-client concurrency limit.** A volumetric flood is left to the network layer or a cloud load balancer in front.
- **Gateway HTTPS listeners are not programmed** by the controller (`UnsupportedProtocol`). In Kubernetes, the proxy-to-client hop is plaintext unless TLS is terminated in front. Pod-to-pod admin traffic is plaintext HTTP, protected by the bearer token and the NetworkPolicy only.
- **Gateways are not isolated from each other.** They share one address, so a route on one Gateway can capture another's traffic for the same host and path.
- **PortcullisPolicies share one limit of 64.** Anyone who can create more than that many valid, attached policies (fewer, with the up to three generated deny policies) makes the snapshot invalid, and config updates stop for every Gateway until some are removed. Restrict who may create them.
- **Response bodies are not inspected.** Policies see headers and status, never bodies.
- **Trusted-proxy misconfiguration.** Putting a public range in `trustedProxies` lets anyone set their own client IP.
- **The config author is all-powerful.** Anyone with the admin token, or with write access to the controller's inputs at cluster scope, can route traffic anywhere.

## Verification

- `make vuln` runs `govulncheck`.
- `make lint` includes `gosec`.
- The fuzz targets are in `internal/policy`.
- Integration tests cover the header-trust rules, body limits and timeouts (`test/integration`). The admin auth paths are tested in `internal/admin`.
