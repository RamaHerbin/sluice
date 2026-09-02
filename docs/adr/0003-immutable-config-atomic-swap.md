# ADR 0003: Immutable routing table published by atomic swap

Status: Accepted, 2026-09-03

## Context

Requests are served on many goroutines while the controller keeps producing new configuration. The data plane must read routing state without locks, one request must never observe two different configurations, and an invalid configuration must never be served in part. The Go standard library offers three shapes for this: a mutex around a mutable structure, copy-on-write behind `sync/atomic.Pointer[T]`, or message passing. `httputil.ReverseProxy` adds a twist: `Rewrite`, `ModifyResponse` and `ErrorHandler` run at different points of one request, so whatever the handler decided must travel with the request.

## Prior art

- Caddy: "configs are treated as immutable, atomic units: either the whole thing is replaced, or nothing gets changed"; on reload "a whole new config document is generated and loaded", old and new run briefly side by side, then the old modules are cleaned up ([architecture](https://caddyserver.com/docs/architecture)). `POST /load` "blocks until the reload completes or fails" and "If the new config fails for any reason, the old config is rolled back into place without downtime" ([API](https://caddyserver.com/docs/api)).
- ingress-nginx rebuilds its whole model on every event and compares it with the current one before reloading NGINX; endpoint-only changes bypass the reload through Lua ([how it works](https://kubernetes.github.io/ingress-nginx/how-it-works/)).
- Go: `atomic.Pointer[T]` (Go 1.19) is "an atomic pointer of type *T. The zero value is a nil *T." The package warns that atomics "require great care to be used correctly" and that synchronization "is better done with channels or the facilities of the sync package" for most programs ([sync/atomic](https://pkg.go.dev/sync/atomic)). `ReverseProxy.Rewrite` (Go 1.20) supersedes `Director`, whose handling of hop-by-hop and `X-Forwarded-*` headers "can permit IP spoofing" ([net/http/httputil](https://pkg.go.dev/net/http/httputil)).

## Decision

`router.Table` is immutable after `router.Compile` returns; the only mutable cells inside it are the round-robin counters of ADR 0005. `router.Store` holds `cur atomic.Pointer[Table]` and a `sync.Mutex` for writers. `NewStore()` stores `Empty()` so `Current()` never returns nil. `Store.Apply(snap)` locks, calls `snap.Normalize()`, calls `Compile` (the sole validator, errors wrapped in `*RouteError`), returns on error with the current table untouched, returns `changed=false` when the hash equals the current one, and otherwise increments `version`, stamps `Version`, `Hash` and `BuiltAt`, and calls `cur.Store(t)`. Validation therefore always precedes publication, and the last known good table is kept by construction rather than by a rollback step.

`proxy.Handler.ServeHTTP` calls `Provider.Current()` exactly once, matches, picks, and carries the chosen `*router.Route` and address in the request context so that the `ReverseProxy` hooks read the same table. There is no release step: a swapped-out table becomes garbage when the last request holding it returns, which is safe because a table owns nothing live (the `http.Transport` is shared, pickers are passive).

## Consequences

Positive:
- Readers never lock and never see a partially applied table. Versions are monotonic because writers are serialized.
- "Last known good" is one early return on error, exercised by the third race test in `docs/design/architecture.md` section 3.
- Old tables need no lifecycle; the garbage collector is the cleanup.

Negative:
- Every change is a full `Compile`, O(routes). Fine for hundreds of routes; a trie or an incremental build is a later local change.
- Anything that would own a goroutine or a connection (active health checks) cannot live in a `Table` without a `Close` tied to swap-out. This is a standing constraint, restated in ADR 0005.
- A slow `Compile` holds the writer mutex and delays the next `Apply`. Acceptable with one controller goroutine.
- `sync/atomic` use is deliberately confined to one `Pointer[Table]` and one `Uint32` per backend.

## Alternatives considered

- **`sync.RWMutex` around a mutable table.** Familiar and small. Readers contend on the lock and in-place mutation invites half-applied states. Why not for sluice now: copy and swap is simpler to reason about and to test under `-race`.
- **Caddy-style module lifecycle with Provision and Cleanup.** Needed when configuration owns listeners and goroutines. sluice has one app and its table owns nothing live. Why not for sluice now: there is nothing to clean up.
- **Incremental updates, ingress-nginx Lua style.** Avoids a rebuild on endpoint churn. Two update paths and a chance of drift between them. Why not for sluice now: rebuild is cheap at this scale and one path is easier to prove correct.
- **Channel handoff to handler goroutines.** `net/http` spawns a goroutine per connection with no natural receive point. Why not for sluice now: the pointer load is the receive.

## Revisit when

- `Compile` time approaches the 100 ms debounce at realistic route counts.
- A table must own something live, for example health checkers; then add an explicit swap-out `Close`.
- Picker state should survive swaps (ADR 0005), which would move mutable state out of the table.

## References

- Caddy architecture: https://caddyserver.com/docs/architecture
- Caddy admin API: https://caddyserver.com/docs/api
- ingress-nginx, how it works: https://kubernetes.github.io/ingress-nginx/how-it-works/
- Go sync/atomic: https://pkg.go.dev/sync/atomic
- Go net/http/httputil: https://pkg.go.dev/net/http/httputil
