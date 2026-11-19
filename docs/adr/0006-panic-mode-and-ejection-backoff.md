# ADR 0006: Panic mode is opt-in; ejection back-off is linear and capped

- Status: accepted
- Date: 2026-11-20

## Context

Health checking can fail in two opposite ways. Backends really are broken: keep them out, and keep repeat offenders out longer. Or the *checks* are broken (a bad `/healthz`, a network partition between proxy and backends) while the app is fine: then excluding every backend turns a monitoring bug into an outage.

## Decision

- **Panic mode** (`Pool.Panic`, `-panic`): when no backend is available, pick one anyway (round-robin over the backends not yet tried) instead of returning 503. **Off by default.** Every panic pick increments `portcullis_panic_picks_total{pool}` and sets `"panic": true` in the access log.
  - Envoy enables a similar *panic threshold* (50%) by default. We chose opt-in because a 503 is an honest signal, while panic mode silently sends traffic to backends that may really be dead.
  - We trigger at 0% available rather than at a percentage, so normal partial failures keep strict health semantics.
- **Ejection back-off:** an ejection lasts `EjectFor × n`, where n is the number of consecutive ejections, capped at `MaxEjectFor` (CLI default 5 min). This is linear, like Envoy's `base_ejection_time`. The multiplier resets once the backend serves successfully for a full `EjectFor` after re-admission.

## Consequences

- A flapping backend is retried less and less often instead of every `EjectFor`.
- With `-panic`, a fully failed pool is served at 100% best effort rather than 0%. Operators should alert on the panic counter.
- Tests: `TestEjectionBackoffAndReset` (40 → 80 → 100 → 100 ms with a 100 ms cap, then back to 40 ms after a clean period), `TestPanicModeSendsWhenNothingIsAvailable` and `TestPanicModeServesWhenHealthChecksAreWrong`, plus a real-binary run: broken health check gives 5/5 × 503 without `-panic` and 5/5 × 200 with it.
