# Architecture

Status: v0, decided at planning time (2026-09-03). Every choice below is reversible unless stated otherwise; the "Reversible decisions" section lists the ones most likely to move.

## 1. Package tree

```
cmd/sluice/              wiring only: flags, logger, clientset, store, controller, two http servers, ordered shutdown
internal/model/          k8s-free intermediate model: Snapshot, Route, Backend, Endpoint, HeaderRule. Pure data. Leaf.
internal/placeholders/   Parse(template) -> Template; Evaluate at request time; registry of allowed variables. Leaf.
internal/upstream/       endpoint selection: Picker interface + round-robin. Leaf.
internal/router/         Compile(model.Snapshot) -> immutable Table (the ONLY validator); Match(host, path); Store (atomic.Pointer).
internal/proxy/          http.Handler: load table once, match, pick, httputil.ReverseProxy, 404/502/503/504, access log.
internal/controller/     the only package importing k8s.io/*: informers -> Translate (pure fn) -> model.Snapshot -> Store.Apply.
internal/observability/  slog setup, request-ID middleware, Prometheus metrics (only package importing prometheus), readiness, admin mux.
deploy/  docs/  examples/  hack/
```

Deviations from the indicative tree in the original brief:

- No `internal/config`. All routing configuration comes from Kubernetes; process settings are a handful of flags parsed in `main`. `internal/model` names the thing precisely: the Kubernetes-independent intermediate model.
- `internal/kubernetes` is merged into `internal/controller`. One package, one `k8s.io` boundary, enforced by `make check-boundary`.
- The atomic store lives in `router` next to the type it holds. Exactly one type crosses the control-plane / data-plane boundary: `*router.Table`.
- `cmd/sluice` instead of `cmd/proxy` so the binary name matches the directory.

Dependency direction: `model`, `placeholders`, `upstream` <- `router` <- `proxy`. `controller` -> `model`, `router`, `placeholders` (validation). `observability` is imported by `main` only; `proxy` and `controller` declare tiny consumer-side `Metrics` interfaces instead.

## 2. Core types

Signatures are frozen in the contract files (`internal/*/…go`). Summary:

### model

- `PathType`: `PathPrefix`, `PathExact`. Ingress `ImplementationSpecific` is translated to `PathPrefix`.
- `RouteKey{Namespace, Name string; Rule, Path int}`: the Ingress and rule indices a route came from. `String()` gives `ns/name#r0p1`.
- `BackendKey{Namespace, Service string; Port int32}`: a Service port. `String()` gives `ns/svc:8080`.
- `Endpoint{Address string /* host:port */; Ready bool}`, `Backend{Key; Endpoints}` sorted by address.
- `HeaderRule{Name, Value string}`: `Value` is the raw placeholder template. Set semantics only in the MVP.
- `Route{Key; Host ("" | "a.b.c" | "*.b.c"); Path; PathType; Backend BackendKey; RequestHeaders; ResponseHeaders; Created}`.
- `Snapshot{Routes; Backends}`. `Normalize()` sorts everything deterministically. `Hash()` is FNV-64a over the normalized snapshot.

Identity: `Version` (monotonic `uint64`) and `Hash` are stamped by `router.Store` at publish time and appear in every config log line and in the `sluice_config_version` gauge. Equal hash short-circuits no-op rebuilds.

### router

- `Compile(*model.Snapshot) (*Table, error)` is the sole validator. Errors wrap `*RouteError{Key model.RouteKey; Err error}` so a log line names the offending Ingress.
- `Table{Version, Hash, BuiltAt}` plus unexported matching structures: exact-host map, wildcard-host slice, catch-all (empty host). Per host: an exact-path map and a prefix slice sorted by length descending, linear scan. Pedagogical and fast enough for hundreds of routes; a trie is a later, local change.
- `Table.Match(host, path) (*Route, bool)`. Host is lowercased and its `:port` stripped.
- `Route{Key; Host; Path; PathType; Namespace; Service; Backend upstream.Picker (never nil, Len()==0 means 503); RequestHeaders, ResponseHeaders []Header{Name; Value placeholders.Template}}`.
- `Store{mu sync.Mutex; cur atomic.Pointer[Table]; version uint64}`: `NewStore()` starts at `Empty()` so the proxy never sees nil; `Current()` is lock-free; `Apply(snap) (t, changed, err)` compiles under the mutex and swaps only on success.

Priority rules:

1. Host: exact > wildcard (`*.x.y` matches exactly one label, never `x.y` nor `a.b.x.y`) > empty host. The most specific matching host wins and there is no fallthrough to a less specific host when its paths do not match (ingress-nginx semantics, deterministic).
2. Path within that host: longest matching path wins; equal length: Exact beats Prefix. Prefix is element-wise: `/foo` matches `/foo`, `/foo/`, `/foo/x`, never `/foobar`. Trailing slash of a Prefix is trimmed except for `/`. Exact is case-sensitive, trailing slash significant.
3. Identical `(host, path, type)` from several Ingresses: oldest `Created`, then `Namespace/Name` lexical.

### upstream

- `Picker{Pick() (addr string, ok bool); Len() int}`.
- `NewRoundRobin(addrs)`: one `atomic.Uint32` counter, `i := next.Add(1)-1; addrs[i%len]`. Starts at 0 so tests are deterministic.
- Decision: counters live per snapshot, inside the compiled Table, and reset on swap. Trade-off: under very frequent config churn the first endpoint gets a few extra requests. Gain: no cross-snapshot mutable registry, no delete bookkeeping, and the story stays "the table is immutable except one atomic counter per backend". Persistent counters keyed by `BackendKey` would be smoother but add a second concurrency surface and a leak hazard. Reversible through the `Picker` interface and a future compile option.
- "Healthy" in the MVP means `ready` per EndpointSlice conditions. No active health checks.

### proxy

- `Provider{Current() *router.Table}` (satisfied by `*router.Store`), `Metrics{ObserveRequest(route string, status int, d time.Duration); UpstreamError(route, kind string)}`.
- `New(p Provider, o Options) *Handler` builds exactly one `http.Transport` and one `httputil.ReverseProxy` for the process lifetime.
- `ServeHTTP`: `t := p.Current()` once, `route, ok := t.Match(...)` else 404, `addr, ok := route.Backend.Pick()` else 503, store `{route, addr}` in the request context, `rp.ServeHTTP`, access log. Status is captured by a small `ResponseWriter` wrapper implementing `Unwrap()` so `Flush`/`Hijack` keep working through `http.ResponseController`.
- `ReverseProxy.Rewrite` (not `Director`): `pr.SetURL(&url.URL{Scheme: "http", Host: addr})`, `pr.Out.Host = pr.In.Host` (Ingress semantics: the backend sees the original Host), `pr.SetXForwarded()` sets X-Forwarded-For/Host/Proto from our view of the connection and drops client-supplied values (no trusted-proxy list in the MVP), then request header templates are evaluated and set on `pr.Out.Header`.
- `ModifyResponse` evaluates response header templates from the state carried in the request context.
- `ErrorHandler`: `context.Canceled` means the client left: log at debug, write nothing, no upstream-error metric. `net.Error.Timeout()` -> 504. Anything else -> 502. Metric `kind` is `dial|timeout|other`.
- Transport: `Dialer{Timeout: 5s, KeepAlive: 30s}`, `ResponseHeaderTimeout: 30s`, `IdleConnTimeout: 90s`, `MaxIdleConnsPerHost: 64`, `ForceAttemptHTTP2: false`. Server: `ReadHeaderTimeout: 10s`, `IdleTimeout: 120s`, no `WriteTimeout` (streaming).
- Request ID: the middleware guarantees an inbound `X-Request-ID`, so `ReverseProxy` forwards it for free and the access log reads it.

### controller

- `Translate(Input) (*model.Snapshot, []Warning)` is a pure function with no I/O and the only code that reads Kubernetes objects. `Input` holds plain slices of typed objects so tests need no client.
- `Sink{Apply(*model.Snapshot) (*router.Table, bool, error)}` (satisfied by `*router.Store`), `Metrics{ConfigApplied(version uint64, routes int); ConfigRejected(); IngressSkipped()}`.
- `New(cs kubernetes.Interface, sink, log, metrics, Options) *Controller`, `Run(ctx) error`, `Synced() <-chan struct{}`.
- Informer handlers do exactly one thing: a non-blocking send on `kick chan struct{}` (size 1). One rebuild goroutine: kick, sleep `Debounce` (100 ms), drain, read listers, `Translate`, `sink.Apply`. Full rebuild every time: O(objects), deterministic, no incremental-state drift.
- Last known good is structural: `Store.Apply` swaps only on success. The controller logs `config rejected` with the `RouteError`, bumps the counter and keeps serving the previous table.
- Per-object isolation: `Translate` skips an Ingress whose header annotations fail `placeholders.Parse`, whose rule uses a resource backend, or whose Service is `ExternalName`, with a `Warning` and `sluice_ingresses_skipped_total`. One typo never freezes updates for the whole cluster. A missing Service or zero ready endpoints keeps the route with an empty picker (503, Ingress semantics). `Compile` rejection remains the safety net.

### placeholders

- `Parse(src) (Template, error)`: unknown `{name}` -> `*UnknownVariableError`; unbalanced braces -> error. `Template.Evaluate(Context) string` is a concatenation loop over segments, no reflection, no `text/template`. `IsStatic()` for templates without variables. `Known(name)` consults the registry: eight fixed names plus the `http.request.header.` prefix family.
- `Context{Request *http.Request; Namespace, Service, Route, UpstreamAddress string}`.
- Missing value (absent header, no upstream) -> empty string. A header rule whose evaluated value is empty is not set, which gives "copy if present" for free. `{http.request.remote}` is the client IP without port.
- Security: header names must be RFC 9110 tokens; CR and LF are stripped from evaluated values; `Host`, `Content-Length`, `Transfer-Encoding` and hop-by-hop headers cannot be set; template length is capped.

### observability

- `NewLogger(level, json)`, `RequestID(next)` middleware, `Metrics` (satisfies both consumer interfaces), `Readiness{atomic.Bool}` with `Set` and `Handler`, `AdminMux(readiness, gatherer)` serving `/healthz`, `/readyz`, `/metrics` on a separate port.
- Metrics: `sluice_http_requests_total{route,code}`, `sluice_http_request_duration_seconds{route}`, `sluice_upstream_errors_total{route,kind}`, `sluice_config_version`, `sluice_config_applied_total`, `sluice_config_rejected_total`, `sluice_ingresses_skipped_total`, `sluice_routes`, `sluice_ready`. Labels are route key and status only, never host, path or headers.
- Access log: request id, method, host, path (no query string), status, duration, route key, upstream. Never headers or bodies.

## 3. Atomic publication

- Mechanism: `cur atomic.Pointer[router.Table]`. `Store.Apply`: lock `mu`, `snap.Normalize()`, `Compile` builds a brand-new object graph, on error unlock and return (nothing published), if the hash equals the current hash return `changed=false`, else `version++`, stamp Version/Hash/BuiltAt, `cur.Store(t)`, unlock. The mutex serializes writers so versions are monotonic; readers never take it.
- In-flight requests: the handler loads the pointer once at the top of `ServeHTTP`. Everything it needs afterwards (Route, Picker, Templates) is reachable from that pointer and travels into the `ReverseProxy` hooks through the request context. A request can never observe two tables.
- Release: no explicit lifecycle. Once swapped out, a Table becomes unreachable when the last request holding it returns and the GC collects it. This works because a Table owns nothing live: the Transport is shared and pickers are passive. Rule for the future: anything with a goroutine (active health checks) must not live inside a Table without a `Close` tied to swap-out.
- Validation before publish: `Compile` is total. Unknown placeholder, duplicate `(host, path, type, key)`, path not starting with `/`, invalid host, unresolved `BackendKey`, invalid header name -> `*RouteError`. Nothing is partially applied.
- Race proof (`router` and `proxy` tests, run with `-race -count=3`): two `httptest` backends answering "A" and "B"; two snapshots; 16 goroutines each sending 2000 requests through `proxy.Handler`; one goroutine alternating `store.Apply(snapA/snapB)` 1000 times with no sleep. Assert: every response is 200, body is "A" or "B", totals match, race detector silent. Second test: a slow backend (200 ms) receives a request, a swap happens mid-flight, the response body is still the old backend's. Third: `Apply` of an invalid snapshot returns an error and `Current()` still returns the previous pointer.

## 4. Concurrency risks

1. Informer callbacks run on informer goroutines: they must never build or touch the store, only kick. Dropping a kick when the channel is full is safe because a rebuild is total. Listers are safe for concurrent reads.
2. Multiple writers to the store (controller plus tests): `Apply` holds a mutex around compile and swap so version numbers stay monotonic.
3. Round-robin counter: `atomic.Uint32`, the only mutable cell inside a Table. Wrap-around is harmless with modulo.
4. Transport and ReverseProxy: exactly one instance of each for the process lifetime, never per snapshot or per request. Per-request data travels only through the context.
5. Readiness flag: `atomic.Bool`, set true after `Synced()`, set false first thing at shutdown. The proxy handler never reads it.
6. Shutdown ordering: readiness false -> `server.Shutdown` (20 s) on both servers (in-flight requests finish; a swap during drain is harmless) -> cancel the root ctx (informers stop, `Run` returns) -> `wg.Wait()` -> `CloseIdleConnections()`. Never close the transport before `Shutdown` returns.
7. `ErrorHandler` after client disconnect: `errors.Is(err, context.Canceled)` must not write a status nor count as an upstream error.
8. Debounce: sleep-then-drain, not `Timer.Reset`, which avoids the classic timer race.

## 5. Reversible decisions

- Name `sluice`, module path, annotation prefix `sluice.dev/`.
- Model shape: Backends as a shared sorted slice referenced by key vs inlined per route.
- Round-robin state per snapshot (behind the `Picker` interface).
- Metrics library (behind consumer-side interfaces).
- Header semantics: Set only; empty evaluated value drops the header.
- `ImplementationSpecific` treated as Prefix; timeouts mapped to 504; no host fallthrough; readiness = synced + first apply attempted.
- Store located in `router` rather than its own package.
- Static-route flags removed at slice 4 (could return as a `-dev` mode).

## 6. Top pitfalls and how the design avoids them

1. One invalid Ingress freezing all future updates because the snapshot is rejected as a whole. `Translate` isolates per Ingress and skips the bad one with a warning metric; `Compile` rejection is only the safety net.
2. Kubernetes types leaking or work happening inside informer callbacks. Handlers only kick a channel; `Translate` is a pure function and the sole reader of Kubernetes objects; `make check-boundary` (`go list -deps` on non-controller packages must not contain `k8s.io`) enforces it.
3. Recreating Transport or ReverseProxy per snapshot or per request, leaking connections and file descriptors. One shared instance each, per-request state through context.
4. Forwarded-header trust and Host handling. `SetXForwarded` overwrites client values, `Out.Host = In.Host` keeps Ingress semantics, hop-by-hop headers are handled by `ReverseProxy` itself.
5. Matching edge cases: `/foo` vs `/foobar`, trailing slashes, `Host: example.com:8080`, uppercase hosts, wildcard matching more than one label. Normalization inside `Match` plus a table-driven test file that encodes the Ingress spec examples.

Also watched: metric label cardinality and sensitive data (route key and status only), and readiness reported before informers have synced (gated on `Synced()`).
