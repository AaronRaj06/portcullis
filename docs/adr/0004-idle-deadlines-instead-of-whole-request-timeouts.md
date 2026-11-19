# ADR 0004: Per-read/per-write idle deadlines instead of whole-request timeouts

- Status: accepted
- Date: 2026-11-19

## Context

`http.Server` has `ReadTimeout` and `WriteTimeout`, which bound the *whole* request read and the *whole* response write. For a proxy that is wrong in both directions:

- `WriteTimeout = 30s` kills every SSE stream or large download after 30 seconds, even if the client is reading happily.
- `ReadTimeout = 30s` kills every upload that takes longer than 30 seconds. Worse, once the deadline passes, net/http's background connection read fails and **cancels the request context**, which would abort an upstream request that is still in progress.

Leaving both at zero is not safe either: a client that stops reading, or stops sending its body, would hold a goroutine and an upstream connection forever.

## Decision

- `ReadTimeout = WriteTimeout = 0` on the server. `ReadHeaderTimeout` stays (slowloris defence).
- `WriteTimeout` becomes "how long one write may block": the handler calls `ResponseController.SetWriteDeadline(now + d)` before every write.
- `BodyReadTimeout` is "the longest gap between body reads": the request body wrapper calls `SetReadDeadline(now + d)` before every read.
  - It **clears** the deadline as soon as the body has been read successfully, because net/http starts its background read at that moment.
  - It **keeps** the expired deadline after an error, because the Transport then closes the body, net/http drains what's left, and that drain must not block on a stalled client.

## Consequences

- Long streams and large transfers work as long as data keeps moving. Stalled peers are cut off after `d`.
- One extra deadline syscall per read and write chunk (32 KiB). Phase 4 will measure the cost.
- Two subtle net/http behaviours are covered by regression tests: `TestSlowUpstreamAfterBodyIsNotCancelled` and `TestStalledClientBodyIs408`.
