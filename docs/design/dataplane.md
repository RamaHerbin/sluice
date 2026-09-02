# Data plane prototype: findings

Status: v0, observed 2026-09-03 on Go 1.26.4 (darwin/arm64) with `prototypes/dataplane`, a throwaway
package that exercises the assumptions of `architecture.md` sections "proxy", "router", "upstream" and
"Atomic publication" against the standard library only. The prototype is deleted after review; this file
keeps what it taught. Reproduce with `go test -race -count=3 -v ./prototypes/...`.

Statements below are backed by the test named in brackets, except where a paragraph says the behavior was not observed (dial-timeout classification, https detection through `In.TLS`, percent-decoding of `r.URL.Path`).

## 1. Rewrite, Host and forwarded headers [TestRewriteForwardedHeaders]

Hook: `pr.SetURL(&url.URL{Scheme: "http", Host: addr}); pr.Out.Host = pr.In.Host; pr.SetXForwarded()`.
The client sent `Host: app.example.com`, `X-Forwarded-For: 1.2.3.4`, `X-Forwarded-Host: evil.example.net`,
`X-Forwarded-Proto: https`, `Forwarded: for=1.2.3.4`, `X-Custom: kept`. The backend saw:

| Header | Value seen by the backend |
|---|---|
| `Host` | `app.example.com` (Ingress semantics; `SetURL` blanks `Out.Host`, so the assignment after it is required) |
| `X-Forwarded-For` | exactly `[127.0.0.1]`: replaced, not appended |
| `X-Forwarded-Host` | `[app.example.com]`: client value dropped |
| `X-Forwarded-Proto` | `[http]`: client value dropped (`https` only when `In.TLS != nil`) |
| `Forwarded` | absent: dropped as well |
| `X-Custom` | `kept` |

Why: when `Rewrite` is set, `ReverseProxy.ServeHTTP` deletes `Forwarded`, `X-Forwarded-For`,
`X-Forwarded-Host` and `X-Forwarded-Proto` from the outbound clone before calling the hook.
`SetXForwarded` then appends to whatever is left, which is nothing. With `Director` the client values
would survive. A trusted-proxy list later means copying `In.Header` values into `Out.Header` before
`SetXForwarded`, which then joins them with a comma.

## 2. ErrorHandler paths [TestErrorHandler*]

| Situation | Exact error observed | Checks that hold | Answer |
|---|---|---|---|
| Connection refused (closed loopback port) | `*net.OpError{Op:"dial", Net:"tcp", Err:*os.SyscallError{Syscall:"connect", Err:syscall.ECONNREFUSED}}`, text `dial tcp 127.0.0.1:NNNNN: connect: connection refused` | `errors.As(err, &*net.OpError)` with `Op == "dial"`; `errors.Is(err, syscall.ECONNREFUSED)`; `net.Error.Timeout() == false` | 502, kind `dial` |
| `Transport.ResponseHeaderTimeout` (100 ms) expiring | `*http.timeoutError` (unexported), text `net/http: timeout awaiting response headers`, fired after 102 ms | `errors.As(err, &net.Error)` with `Timeout() == true`; not a `*net.OpError`; **`errors.Is(err, context.DeadlineExceeded) == true`** because the type implements `Is`; `errors.Is(err, context.Canceled) == false` | 504, kind `timeout` |
| Client closes the connection while the backend is busy (real listener) | the sentinel `context.Canceled` itself (`err == context.Canceled`) | `errors.Is(err, context.Canceled)` | write nothing |

Consequences for `internal/proxy`:

- Classify in this order: `context.Canceled` first, then `net.Error.Timeout()`, then `*net.OpError` with
  `Op == "dial"`, then everything else. Never use `errors.Is(err, context.DeadlineExceeded)` to mean "the
  inbound request had a deadline": on Go 1.26 the upstream header timeout also satisfies it.
- Writing nothing from `ErrorHandler` on cancel is safe: no panic, the status wrapper records 0 and zero
  `WriteHeader` calls, the server finalizes the dead connection on its own
  [TestErrorHandlerClientCanceled, TestErrorHandlerCanceledWithRecorder]. The access log therefore sees
  status 0 for a cancelled request; decide whether to log 0 or a synthetic 499 (see open questions).
- A dial timeout was not exercised (needs a black-hole address). It would be a `*net.OpError{Op:"dial"}`
  whose `Timeout()` is true, so with the order above it is reported as 504/`timeout`, not 502/`dial`.
- The `ErrorHandler` is only reached for failures before the response headers are copied. See section 5
  for what happens when the client leaves mid-body.

## 3. Per-request state through the context [TestContextValuesReachHooks]

A `*requestState{route, addr, in}` stored with `context.WithValue` on the inbound request before
`rp.ServeHTTP` is readable, as the same pointer, from `pr.In.Context()` and `pr.Out.Context()` in
`Rewrite`, from `resp.Request.Context()` in `ModifyResponse`, and from `req.Context()` in `ErrorHandler`.
`ReverseProxy` clones the request with `req.Clone(ctx)` where `ctx` derives from the inbound context, and
`resp.Request` is that outbound clone, so values survive. No alternative mechanism is needed.

One subtlety: `resp.Request` is the rewritten clone (it carries `X-Forwarded-For`, has lost hop-by-hop
headers and gained the request header rules). Response header templates that read
`{http.request.header.X}` or `{http.request.remote}` must evaluate against the untouched inbound
request. The state should therefore carry the inbound `*http.Request` and `placeholders.Context.Request`
must be set from it, not from `resp.Request`.

## 4. Response header templates [TestContextValuesReachHooks]

`resp.Header.Set("X-Route", "r")` in `ModifyResponse` reaches the client, alongside the backend's own
headers. `pr.Out.Header.Set(...)` in `Rewrite` reaches the backend. Static strings were used; the real
implementation evaluates `placeholders.Template` at the same two places.

## 5. Status wrapper, Unwrap, streaming [TestStatusWriter*, TestStreamingThroughProxy, TestEarlyHints*, TestClientLeavesMidBody]

- A wrapper embedding `http.ResponseWriter` promotes only `Header`, `Write` and `WriteHeader`. With
  `Unwrap() http.ResponseWriter`, `http.NewResponseController(w).Flush()` reaches the real `Flusher`
  (`ReverseProxy` flushes through `ResponseController`, so this is what keeps streaming alive). Without
  `Unwrap` the call fails with `http.ErrNotSupported` ("feature not supported").
- Streaming: a backend flushing three chunks 100 ms apart was seen by the client at 1 ms, 102 ms and
  203 ms after the request; the backend finished at 203 ms. `ReverseProxy` flushes immediately when the
  response has `ContentLength == -1` (chunked) or is `text/event-stream`, with `FlushInterval` left at 0.
  The frontend `httptest.Server` has no `WriteTimeout`, matching the design.
- 1xx: `ReverseProxy` forwards informational responses (103 Early Hints) by calling `rw.WriteHeader(103)`
  before the final status. The wrapper must ignore codes below 200 (except 101) or the access log records
  103 instead of 200. Observed: with the guard, the captured status is 200.
- Client leaving mid-body: `ErrorHandler` is **not** called. Under a real server `ReverseProxy` panics
  with `http.ErrAbortHandler`, which `net/http` recovers silently. Deferred code in `ServeHTTP` (access
  log, metrics) still runs during the unwind and the wrapper already holds status 200. The metric for
  this case is therefore "a 200 that took d", not an upstream error. Outside a server (recorder), the
  same failure is only logged; tests that rely on the panic must go through a listener.

## 6. Matching rules as validated [TestMatchRules, TestNewTableValidation, TestNormalizeHost]

Structure: exact-host map, wildcard-host map keyed by the literal `*.suffix`, one catch-all bucket for
the empty host. Per bucket: exact-path map plus prefix slice sorted by trimmed length descending.

Validated behaviors (36 table cases):

- Prefix `/foo` matches `/foo`, `/foo/`, `/foo/baz`; not `/foobar`; `/foo/barbaz` falls back to `/foo`.
- Prefix `/trail/` is stored as `/trail` and matches `/trail`, `/trail/`, `/trail/x`, not `/trailer`.
  Prefix `/` is kept as `/`. Two prefixes that trim to the same string are a duplicate at build time.
- Exact is case-sensitive (`/Case` does not match `/case`) with a significant trailing slash
  (`/slash/` Exact does not match `/slash`; Prefix `/slash` takes `/slash` and `/slash/x`).
- Same length, Exact beats Prefix (`/foo/bar`); otherwise the longest prefix wins.
- Hosts: `Example.COM:8080` and `A.Example.Com:443` normalize to lowercase without port.
  `*.example.com` matches `a.example.com`, not `example.com` nor `a.b.example.com`; the apex and the
  two-label host fall to the empty-host bucket.
- Priority: exact host, then wildcard, then empty. Once a bucket matched the host there is no
  fallthrough: `strict.example.com/y` is 404 even though `*.example.com` and `""` both have `/`.
- With no empty-host routes an unknown host is 404.

Lessons and ambiguities found in `architecture.md`:

1. "Longest path wins; equal length Exact beats Prefix" collapses to "Exact map first, then the prefix
   scan": an Exact match spans the whole request path, so it is always at least as long as any Prefix
   match. Two distinct prefixes of equal length can never both match one path, so the prefix order among
   equals is irrelevant; only duplicates need a tie-break, and that is `Compile`'s job.
2. Wildcard hosts do not need a slice scan. Because a wildcard matches exactly one label, the only
   candidate key for host `h` is `"*" + h[strings.IndexByte(h, '.'):]`, a map lookup. The `Table` TODO
   comment says "wildcard-host slice"; either works, the map is simpler.
3. Host normalization uses `net.SplitHostPort`: `[::1]:8080` becomes `::1` but a bare `[::1]` keeps its
   brackets. Ingress hosts cannot be IP literals, so this only matters for what `Match` receives from odd
   clients. A trailing dot (`example.com.`) is not stripped. Both need a decision in `Compile`/`Match`.
4. Paths are matched on `r.URL.Path`, which is percent-decoded and not cleaned: `/foo%2Fbar` matches as
   `/foo/bar`, `//foo` and `/foo/../bar` match literally. The spec is silent; document the choice.
5. An empty request `Host` (HTTP/1.0) only reaches the empty-host bucket, which is the sane reading.

## 7. Round-robin [TestRoundRobin*]

`i := next.Add(1) - 1; addrs[i % len]` over two addresses yields `a b a b a` from a fresh picker; an
empty picker reports `("", false)` and `Len() == 0`. `NewRoundRobin` must copy its input slice. Around
the `uint32` wrap the index sequence is `MaxUint32-1, MaxUint32, 0, 1`: alternation is preserved for two
addresses (`a b a b`); for three it produces `c a a b`, one repeated address once every 4 billion
requests. Harmless, as the design says.

## 8. Atomic publication under load [TestSwapUnderLoad, TestSwapMidFlightKeepsOldBackend]

16 goroutines x 2000 requests through one `Handler` while a goroutine alternates `atomic.Pointer`
stores between a table pointing at backend "A" and one at "B". Observed under `-race`: 32000 responses,
all 200, A=16062 B=15938, bad=0; the backends counted exactly the same split; 15 TCP connections per
backend in total. The detector stayed silent over `-count=3`.

Lesson for the real test: 1000 swaps with no sleep complete in microseconds, before most requests have
started, so the prototype swapper keeps alternating until the workers finish (4.3 million swaps here).
Write the `router`/`proxy` race test the same way, or the swap will not overlap the traffic.

Mid-flight: a request inside a 200 ms backend, a swap to a fast backend while it waits, body `slow`
returned; the next request gets `fast`. Loading the pointer once at the top of `ServeHTTP` and carrying
`{route, addr}` in the context is sufficient; nothing else in the hooks touches the store.

## 9. One Transport, one ReverseProxy [TestSingleTransportAndProxy, TestSwapUnderLoad]

Both are created once in `New` and `Publish` never replaces them. The `Transport` pools connections by
target `host:port`, which the `Rewrite` hook reads from the context per request, so tables are
irrelevant to the pool: with `MaxIdleConnsPerHost: 64` the hammer above opened 15 connections per
backend for 32000 requests. Per-snapshot transports are unnecessary and would only leak idle
connections at every swap. Leave `Transport.Proxy` nil (a fresh `Transport` ignores `HTTP_PROXY`;
`http.DefaultTransport` does not).

## 10. Notes for the contract files (no signature change needed)

- `internal/proxy/proxy.go`: the status wrapper must ignore 1xx codes; `ErrorHandler` never sees
  body-copy failures (they surface as `panic(http.ErrAbortHandler)` and the deferred access log must
  cope with status already set); classification order as in section 2; keep `Transport.Proxy` nil.
- `internal/proxy/proxy.go` and `internal/placeholders/placeholders.go`: `placeholders.Context.Request`
  for response header templates must be the inbound request kept in the context state, not
  `resp.Request`.
- `internal/router/router.go`: the wildcard structure can be a map keyed by `*.suffix` (comment says
  slice); `Match` should decide on trailing dots and IPv6 literals; the "equal length" rule can be
  implemented as "Exact first".
- `internal/upstream/picker.go`: copy `addrs` in `NewRoundRobin`.
- `internal/proxy/proxy.go` `Metrics.ObserveRequest`: define the status reported for a cancelled request
  (0 as observed, or a synthetic 499). `code="0"` as a label value is unusual but honest.

## 11. Open questions

1. Status to log and count when the client cancels before headers (0 vs 499) and when it leaves
   mid-body (200 already captured; `ErrAbortHandler` passes through the handler).
2. Dial timeout classification: it satisfies both `Timeout()` and `Op == "dial"`; the order above makes
   it a 504. Acceptable, or should a dial failure of any kind be 502?
3. Path normalization before matching (percent-decoding, `//`, dot segments) and the trailing-dot host.
4. Whether `FlushInterval` should be set to a small value for fixed-length slow bodies. Today those are
   copied in 32 KiB chunks without an explicit flush; chunked and SSE responses already stream.
5. Body-size or header-size limits on the proxy port are not covered by this prototype.
