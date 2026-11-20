# ADR 0009: Panic mode on by default (supersedes ADR 0006's default)

- Status: accepted
- Date: 2026-11-20
- Supersedes: the "off by default" part of ADR 0006 (the mechanism is unchanged)

## Context

ADR 0006 made panic mode opt-in: a 503 when every backend looks down is an honest signal. The first full benchmark run produced evidence against that default.

In one of three failure-injection runs, after the killed backend had already come back, the whole laptop stalled for about a second (the host's load average was over 100 from unrelated work). The benchmark pool's active health checks (500 ms timeout, down after 2 misses) timed out on **both** healthy backends at once. Both were marked down, and Portcullis answered **2,559 requests (10.3%) with 503 "no available backend"** for about 5 s while both backends were serving fine. NGINX, under the same conditions, had zero errors in all three runs: when every upstream server is marked failed, it resets them and tries anyway.

The failure mode is *correlated false positives*: whatever slows the health checks (a stall in the proxy's own host, a network blip, an overloaded checker) tends to slow them for every backend at once. Excluding every backend then turns a monitoring problem into a full outage. Envoy guards against the same thing with a panic threshold that is on by default.

## Decision

- In the config schema, `pools[].panic` now **defaults to true** (the field is a pointer, so an explicit `panic: false` still works). The CLI flag `-panic` defaults to true.
- The Go API is unchanged: `lb.Pool.Panic`'s zero value is false, so code that builds pools directly keeps its behaviour, and the tests of strict 503s still hold.
- The mechanism is still ADR 0006's: panic picks only when **no** backend is available, and every panic pick is counted (`portcullis_panic_picks_total`) and flagged in the access log, so it stays visible.

## Consequences

- A pool whose backends are all really down now returns 502s (connect errors, retried within the budget) instead of 503s. The panic counter and `portcullis_backend_up` make that distinguishable on a dashboard.
- Operators who want strict behaviour write `panic: false`.
- Lesson recorded: a default chosen on principle was reversed by data, which is what the benchmark was for.
