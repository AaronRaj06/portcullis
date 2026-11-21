# ADR 0010: PolicyScript design

- Status: accepted
- Date: 2026-11-21

## Context

The brief asks for a small, safe rule language run on every request (section 3): a hand-written lexer, parser and type checker; termination by construction; budgets; and fail-closed or fail-open behaviour. The decisions below fill in what the brief leaves open.

## Decision

- **A tree-walking interpreter over checked nodes, not bytecode.** The checker resolves every name, literal list, CIDR, header name and rate limiter at load time into a `node` tree. Evaluation is a `switch` over opcodes. The spec's main example evaluates in about 0.4 µs with 2 allocations (`bench/results/policy`), small next to the ~3 µs proxy core, so a VM would add complexity for little gain.
- **One policy per script, attached to routes.** A config entry names its routes (default: all). Policies run after route matching, so `route_to` and per-route attachment work. Requests that match no route get 404 without running policies.
- **Atomic effects.** A policy's header changes, rewrite and pool choice are buffered and applied only when it finishes. A budget breach therefore never leaves a half-modified request.
- **Fail closed by default, with a 500.** A runtime error is a server-side fault, not a client one, so it is not a 403.
- **No list or ip values at runtime.** `in` only takes literal lists, resolved at load time. Values are a small struct (`string`, `int64`), so evaluation does not allocate for typical rules.
- **Smuggling guard.** Policies cannot touch framing headers (`Content-Length`, `Transfer-Encoding`, `Host`, …) or the request ID, and CR/LF/NUL in a computed value is a runtime error.
- **Rate-limit state survives hot reloads.** A limiter is keyed by its call site: policy name, `line:col`, limit and window. `config.Compile` takes unchanged ones from the previous snapshot, as it already does for backend health. Editing the script above a call site moves it, which resets that limiter; this is accepted and documented.
- **Memory-bounded limiter.** 64 mutex-guarded shards, at most 100k keys per call site. When a shard is full, idle keys go first (forgetting a full bucket changes nothing), then an arbitrary eighth.

## Consequences

- Policies cannot see the request body, and the response phase does not run on errors the proxy generates itself. Both are documented in the spec.
- Limits are per proxy instance: N replicas allow N × limit. Shared limits would need an external store, which is a non-goal (no paid or extra services).
- The time budget is wall-clock, so it can fire spuriously on a starved CPU. That is why it defaults to 1 ms, about 2,000 times the typical evaluation time.
