# PolicyScript

PolicyScript is a small rule language that Portcullis runs on every request and every response, in the spirit of F5 iRules. It decides things like "block `/admin` from outside the office", "rate-limit each client", and "send canary traffic to the canary pool".

It is **safe by construction**:

- There are no loops, no recursion and no user-defined functions, so every policy finishes. Its running time is bounded by its size.
- Built-ins are pure: no files, no network, no clock. The one exception is `rate_limit`, whose state lives in memory inside the proxy.
- Every program is type-checked before it can be loaded, and errors point at `line:col`.
- Hard limits apply to source size, nesting depth and node count, and per request to evaluation steps and time.

```
policy "edge-rules" {
  on request {
    if path.starts_with("/admin") && !(client.ip in ["10.0.0.0/8", "192.168.0.0/16"]) {
      deny(403, "admin is internal only")
    }
    if header("user-agent").contains("sqlmap") { deny(403) }
    if rate_limit(client.ip, 100, "1m") { deny(429) }
    if header("x-canary") == "1" { route_to("canary") }
    set_header("x-frame-options", "DENY")
  }
  on response {
    remove_header("server")
  }
}
```

## 1. Attaching policies

Policies are part of the dynamic config, so they hot-reload like routes:

```yaml
policies:
  - script: |
      policy "edge-rules" { ... }
    routes: [api, admin]   # optional; omitted = every route
    onError: fail_closed   # or fail_open (default fail_closed)
    maxSteps: 10000        # optional, 1..1000000
    timeout: 1ms           # optional, 10µs..100ms
```

- **When it runs.** The request handler runs after a route has matched, before a backend is picked. The response handler runs when the upstream response arrives, before its headers go to the client. It does not run for errors the proxy generates itself (404 no route, 502, 503, 504), or for upgraded connections (WebSocket).
- **Order.** Policies run in config order. A request with no matching route gets a 404 and runs no policies.
- **Atomic effects.** One policy's effects (headers, path rewrite, pool choice) are applied only when that policy finishes without error. A later policy sees the effects of earlier ones: a rewritten path, a set header.
- **Terminal actions.** `deny`, `redirect` and `allow` stop the request phase for this policy *and all later policies*.

## 2. Lexical structure

- Source is UTF-8. Whitespace and newlines separate tokens and have no other meaning. A `;` after a statement is optional.
- Comments: `#` or `//` to the end of the line.
- **Identifiers:** `[A-Za-z_][A-Za-z0-9_]*`.
- **Keywords:** `policy on request response if else true false in`.
- **Integers:** decimal digits, with a maximum of 2^53.
- **Strings:** double-quoted. The escapes are `\" \\ \n \t`. A raw newline inside a string is an error.
- **Punctuation:** `{ } ( ) [ ] , . ; + ! && || == != < <= > >=`.

## 3. Grammar

```
script    = "policy" STRING "{" { handler } "}"            -- exactly one policy
handler   = "on" ( "request" | "response" ) block          -- each at most once
block     = "{" { stmt } "}"
stmt      = ifStmt | call [ ";" ]
ifStmt    = "if" expr block [ "else" ( ifStmt | block ) ]
expr      = and { "||" and }
and       = cmp { "&&" cmp }
cmp       = sum [ ( "==" | "!=" | "<" | "<=" | ">" | ">=" | "in" ) sum ]   -- not chainable
sum       = unary { "+" unary }
unary     = "!" unary | postfix
postfix   = primary { "." IDENT [ "(" [ args ] ")" ] }
primary   = STRING | INT | "true" | "false"
          | IDENT [ "(" [ args ] ")" ]
          | "(" expr ")"
          | "[" [ expr { "," expr } [ "," ] ] "]"
args      = expr { "," expr }
```

From loosest to tightest binding: `||`, `&&`, comparisons and `in`, `+`, `!`, then `.`. A statement must be an **action** call (section 5). An expression whose value is unused is an error.

## 4. Types and values

The types are `string`, `int`, `bool`, `ip`, `list<string>` and `list<int>`. Actions have no value (`void`). There is no `null`: a missing header, query parameter or cookie is `""`.

**Request values**, available in both handlers:

| Name | Type | Meaning |
|---|---|---|
| `method` | string | `GET`, `POST`, … (upper case) |
| `host` | string | `Host` header, lower case, without port |
| `path` | string | URL path, after any `rewrite_path` by an earlier policy |
| `header(name)` | string | first value of a request header (case-insensitive name), as the upstream will see it |
| `query(name)` | string | first value of a query parameter |
| `cookie(name)` | string | value of a request cookie |
| `client.ip` | ip | the real client: the peer, or behind trusted proxies the first untrusted `X-Forwarded-For` hop |

**Response values**, response handler only:

| Name | Type | Meaning |
|---|---|---|
| `response.status` | int | upstream status code |
| `response.header(name)` | string | first value of a response header |

**String methods:** `s.starts_with(t)`, `s.ends_with(t)` and `s.contains(t)` return bool; `s.lower()` and `s.upper()` return string; `s.len()` returns int (bytes).

**Operators:**

| Operator | Operands | Result |
|---|---|---|
| `+` | string, string | string (concatenation) |
| `==` `!=` | two of the same type: string, int or bool | bool |
| `<` `<=` `>` `>=` | int, int | bool |
| `in` | string in `list<string>`; int in `list<int>` | bool (membership) |
| `in` | ip in a **literal** list of string literals, each a CIDR (`"10.0.0.0/8"`) or an address (`"::1"`) | bool. The prefixes are parsed when the policy is loaded, so a bad CIDR is a load error |
| `&&` `\|\|` | bool, bool | bool, short-circuit |
| `!` | bool | bool |

Lists appear only as the right side of `in`. Their elements must be literals of one type, so a list is fixed when the policy loads. Lists and `ip` values cannot be compared with `==`.

**Rate limiting:** `rate_limit(key, limit, window)` is request-only and returns `bool`.

- `key` is a string or an ip. `limit` is an int literal of at least 1. `window` is a duration literal from `"1s"` to `"24h"`.
- It returns **true when the request is over the limit**. Each key gets a token bucket that holds `limit` tokens and refills `limit` per `window`. A request that finds a token takes it and gets `false`.
- Each `rate_limit` call site has its own buckets.
- The state is in memory and per proxy instance: two replicas each allow `limit`. It survives hot reloads as long as the call site's policy name, position, limit and window are unchanged. It is bounded: idle keys are evicted, and past the key limit the oldest-looking keys go first (see `internal/ratelimit`).

## 5. Actions

| Action | Phase | Effect |
|---|---|---|
| `deny(status)` / `deny(status, message)` | request | Respond with `status`, an int literal from 400 to 599, and a JSON error body carrying `message` (default: the status text) and the request ID. **Terminal.** |
| `allow()` | request | Stop evaluating; later policies' request handlers are skipped. The request proceeds. **Terminal.** |
| `redirect(url)` / `redirect(url, status)` | request | Respond with `Location: url`. `status` is a literal 301, 302, 303, 307 or 308 (default 302). **Terminal.** |
| `route_to(pool)` | request | Send this request to `pool`, a string literal naming a configured pool (checked at load), instead of the route's own pool or split. The last call wins. |
| `rewrite_path(p)` | request | Change the upstream path. It must start with `/`, or it is a runtime error. The query string is kept. |
| `set_header(name, value)` | both | Set a header: on the upstream request, or on the response to the client. |
| `remove_header(name)` | both | Remove a header (same targets). |
| `log(message)` | both | Write an info-level log line with the policy name and request ID. |
| `tag(name)` | both | Add a string-literal tag to the request's access-log `tags` field. |

`name` in `set_header` and `remove_header` must be a string literal and a valid header name. To prevent request smuggling, these cannot be touched: `Host`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Upgrade`, `Te`, `Trailer`, `Keep-Alive`, `Proxy-Connection` and `X-Request-Id`. A header value containing CR, LF or NUL is a runtime error.

## 6. Errors and budgets

**Load-time errors** reject the whole config, and the old config keeps serving. They are reported as `policies[i].script:LINE:COL: message`. The checker reports every type error it finds, not just the first. A syntax error stops at the first one.

**Runtime errors** are:
- a step budget breach (each evaluated expression or statement is one step),
- a time budget breach (checked every 64 steps),
- an invalid `rewrite_path` or header value.

On a runtime error, the policy's effects are discarded and `onError` applies:

- `fail_closed` (default): the request is answered `500` (error kind `policy_error`). In the response phase, the upstream response is replaced by a 500 if its headers have not been sent, and they never have at that point.
- `fail_open`: the policy is skipped as if it did not exist.

Both cases count in `portcullis_policy_errors_total{policy}` and are logged.

**Hard limits:**

| Limit | Value |
|---|---|
| Source size | 64 KiB |
| Syntax tree nodes | 10,000 |
| Nesting depth (blocks + expressions) | 64 |
| String literal | 4 KiB |
| List literal | 256 elements |
| Policies per config | 64 |
| Steps per evaluation | `maxSteps`, default 10,000 |
| Time per evaluation | `timeout`, default 1 ms |

## 7. Metrics

- `portcullis_policy_denials_total{policy,code}`: requests ended by `deny` or `redirect`.
- `portcullis_policy_errors_total{policy}`: runtime errors.

## 8. More examples

```
policy "canary-by-cookie" {
  on request {
    if cookie("beta") == "yes" || header("x-canary") == "1" { route_to("canary") tag("canary") }
  }
}

policy "legacy-paths" {
  on request {
    if path == "/old" { redirect("/new", 308) }
    if path.starts_with("/v1/") { rewrite_path("/api" + path) }
  }
}

policy "hide-errors" {
  on response {
    if response.status >= 500 { set_header("cache-control", "no-store") }
    remove_header("server")
    remove_header("x-powered-by")
  }
}
```
