# ADR 0007: Immutable snapshots, atomic swap, and a bootstrap/dynamic split

- Status: accepted
- Date: 2026-11-20

## Context

The controller (Phase 6) and operators must be able to change routing at any time without dropping a request, and a bad config must never take traffic down.

## Decision

1. **Two kinds of config.** Process-level settings (listeners, TLS certificate, server timeouts, admin auth) are flags read once at start-up. Traffic config (routes, pools, backends, health, retries, trusted proxies) is a YAML or JSON document that can change at runtime. This is Envoy's bootstrap vs dynamic split. Changing a listener needs a socket rebind anyway, so nothing is lost by restarting for it.
2. **Validate, then compile, then swap.** `config.Compile` runs strict parsing (unknown fields are errors), applies defaults, collects every problem with its field path, and builds a complete `proxy.Snapshot` (router, pools, balancers, transports). Only then does `Manager` publish it with one `atomic.Pointer.Swap`. Any error leaves the old snapshot untouched.
3. **One load per request.** `ServeHTTP` loads the pointer once and uses that snapshot for the whole request, retries included. A request that started before a swap finishes on the old snapshot; the garbage collector frees it after the last such request.
4. **State carries over.** Backends with the same pool name and address inherit health and ejection state, so a reload never revives a backend known to be down. Active health checks belong to the snapshot: the old ones are cancelled at swap, new ones start.
5. **Identity.** Each config has a free-form `version` (set by the author or controller), a `hash` (sha256 of canonical JSON after defaults, so formatting and comments don't matter) and a process-local `generation` (+1 per successful swap). A push whose hash equals the current one is a no-op (`changed: false`), so the controller can retry freely.
6. **Three ways in:** SIGHUP and `POST /reload` re-read the file; `PUT /config` takes a document (JSON or YAML, max 4 MiB). If a token file is configured, all of these and `GET /config` need `Authorization: Bearer`, compared in constant time.

## Rejected alternatives

- **A RWMutex around a mutable config.** Readers would contend on every request ("no global mutex on the request path"), and partial updates could be seen half-applied.
- **Diffing and patching the live router.** Fewer allocations per reload, but far more ways to leave inconsistent state. Reloads are rare; requests are not.
- **Accepting unknown fields.** Friendlier to version skew, but a typo (`pathtype:`) would silently fall back to a default. Strictness is the safer failure.

## Consequences

- Measured: 100 reloads (90 valid, 10 deliberately invalid, alternating file and API) under 16 concurrent clients: **0 failed of 7,148 requests** under `-race`; five repeats: 0 of 40,634.
- A reload rebuilds pool-specific transports (h2c, private CA), so those pools open new upstream connections afterwards. Default-transport pools keep their connections.
- `GET /config?full=1` shows the config with defaults applied, which helps when debugging what is actually serving.
