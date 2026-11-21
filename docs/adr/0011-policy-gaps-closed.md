# ADR 0011: Closing PolicyScript's first gaps

- Status: accepted
- Date: 2026-11-21
- Supersedes: in ADR 0010, the response phase's scope and "limits are per instance" as the only option

## Context

ADR 0010 shipped PolicyScript with five recorded gaps (`docs/ROADMAP.md`):

- rate limits were per instance;
- a limiter reset when its `line:col` moved;
- the response phase skipped proxy-made errors and upgrades;
- there were no regular expressions, request bodies or clock;
- attaching a policy cost 3 allocations per request.

## Decision

- **Approximate global limits without a shared store.** A top-level `instances: N` divides every limit, giving `ceil(limit/N)` per replica. It is exact when traffic is spread evenly and drifts when it is not. A shared store (Redis, gossip between replicas) would be exact but adds a service or a protocol. That conflicts with "no extra services" and costs a network round trip per request.
- **Named limiters.** An optional fourth argument to `rate_limit` replaces the position in the limiter's identity.
- **The response phase runs on every response sent after routing:** proxy errors and WebSocket handshakes too. Protected headers (`Connection`, `Upgrade`, …) keep policies from breaking a handshake. A fail-closed failure answers 500 as before.
- **`matches` with RE2** (`regexp`): linear-time matching, compiled at load.
- **`now()`** is the request's arrival time, fixed for the whole request, so evaluation stays deterministic.
- **Request bodies stay out.** Reading a body inside a policy means buffering it before forwarding. The brief forbids buffering whole bodies, and it would add latency and memory per request. Not planned.
- **Allocations:** a `sync.Pool` of `Env`s, and an allocation-free rate-limit lookup for known IP keys (`Limiter.AllowBytes`). That takes 3 extra allocations per request down to 1, which is `set_header`'s value slice.

## Consequences

- With uneven load, a client pinned to one replica gets `ceil(limit/N)` instead of `limit`. That is stricter, which is the safe direction for a limit.
- Policies now see 502/503/504, so a rule like `if response.status >= 500 { … }` also fires on the proxy's own errors. The spec documents it.
