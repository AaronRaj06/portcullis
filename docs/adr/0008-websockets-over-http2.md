# ADR 0008: WebSockets over HTTP/2 (RFC 8441), opt-in via GODEBUG

- Status: accepted
- Date: 2026-11-20

## Context

HTTP/2 connections cannot be hijacked, so the HTTP/1.1 upgrade path cannot serve a WebSocket that arrives over an HTTP/2 client connection. RFC 8441 defines the HTTP/2 way: an *extended CONNECT* (`:method CONNECT`, `:protocol websocket`), answered with `200`, after which the stream carries the WebSocket bytes.

Go's HTTP/2 server parses extended CONNECT, but only advertises and accepts it when the process starts with `GODEBUG=http2xconnect=1` (golang/go#71128). It is off by default because browsers that see the setting will use it, and many Go WebSocket libraries can't handle it.

## Decision

- The proxy translates RFC 8441 to RFC 6455 toward backends, since backends almost always speak HTTP/1.1 WebSockets:
  - rewrite to `GET` with `Upgrade: websocket`, generating the `Sec-WebSocket-Key` that HTTP/2 clients don't send;
  - **verify the backend's `Sec-WebSocket-Accept`** against that key (502 if wrong);
  - answer the client `200`, then copy client stream → backend and backend → response stream, flushing each chunk;
  - track tunnels for graceful drain, the same as HTTP/1.1 tunnels.
- Plain `CONNECT` (a raw TCP tunnel request) gets 405. It is checked before routing, since CONNECT has no path.
- **Enabling it is the operator's choice:** set `GODEBUG=http2xconnect=1` in the environment. Without it, Go refuses extended CONNECT at the protocol level and browsers keep using HTTP/1.1 WebSockets, which already work.
- **A test-only dependency on `golang.org/x/net`** (`http2.Framer`, `hpack`). `net/http`'s client rejects the `:protocol` header name, and recent `x/net/http2` Transports route through `net/http`, so the test speaks raw HTTP/2 frames, as a browser does. `x/net` is the Go project's own module, was already in the module graph via Prometheus, and becomes a direct dependency anyway with controller-runtime in Phase 6. No production code imports it.

## Consequences

- `TestWebSocketOverHTTP2` re-runs itself in a subprocess with the GODEBUG set. It checks that the server advertises `SETTINGS_ENABLE_CONNECT_PROTOCOL`, then covers an echo that idles past `WriteTimeout`, a forged accept (502), and plain CONNECT (405).
- When Go turns extended CONNECT on by default, the feature works with no code change.
