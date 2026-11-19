# ADR 0005: When a request may be retried

- Status: accepted
- Date: 2026-11-19

## Context

Retrying hides single-backend failures from users. Retrying wrongly can charge a card twice, or turn a partial outage into a total one (a *retry storm*: every client retries, multiplying load on backends that are already struggling).

## Decision

A failed attempt is retried on a different backend only if **all** of these hold:

1. **The failure is the upstream's.** A connect error or a timeout, mapped to 502/504. Client faults (408, 413, client gone) never retry.
2. **The method is idempotent** (GET, HEAD, OPTIONS, TRACE, PUT, DELETE; RFC 9110 §9.2.2).
3. **Nothing reached the backend.** We install an `httptrace.ClientTrace` whose `WroteHeaders` hook flips a flag. If request headers were written, the backend may have started acting on the request, so even a GET is not retried. Also no body bytes may have been read, so the body is still intact to resend.
4. **Attempts remain** (`Pool.Retries`), and each backend is tried at most once per request.
5. **The retry budget has a token.** Each request deposits `ratio` tokens (default 0.2); each retry spends 1. In a full outage, retries add at most 20% extra load, plus a small fixed burst.

Responses (even 502/503/504 from the backend) are never retried; they feed passive ejection instead.

## Rejected alternatives

- **Retry any idempotent failure, including timeouts after the request was sent.** Common (Envoy's `retry_on: 5xx`), but it breaks the brief's "no request bytes sent" rule and doubles work on a slow backend.
- **A fixed retry count with no budget.** This is how retry storms happen: N retries means up to (N+1)× load during an outage.
- **A time-windowed counter for the budget.** Needs a clock and a lock or rotating buckets. The token bucket is one lock-free CAS per request.

## Consequences

- Retries in practice cover refused or reset connections before the request was sent: dead pods, closed ports, and stale keep-alive connections the Transport itself doesn't already retry.
- Only requests that may be retried pay for the trace hook (a few small allocations).
- Tests: `TestRetriesIdempotentRequestsThatNeverReachedABackend`, `TestNoRetryForNonIdempotentRequests`, `TestNoRetryOnceRequestReachedBackend` and `TestRetryBudgetCapsAmplification` (20 requests to a dead pool make exactly 23 attempts). Each guard was mutation-tested: removing it makes its test fail.
