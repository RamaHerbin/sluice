# ADR 0005: Simple round-robin with per-table counters

Status: Accepted, 2026-09-03

## Context

Each `router.Route` points at one Service port that resolves to zero or more ready endpoint addresses, and the proxy must choose one per request. Ingress gives no weights, no policy field and no health-check configuration; readiness is already expressed per endpoint by EndpointSlice conditions fed by kubelet probes. ADR 0003 fixes the frame: a `Table` is immutable and owns nothing live, so whatever state a picker needs must be tiny, lock-free and disposable. Tests need a deterministic order.

## Prior art

- Caddy `reverse_proxy` offers `lb_policy` values `random` (the default), `random_choose`, `first`, `round_robin`, `weighted_round_robin`, `least_conn`, `ip_hash`, `client_ip_hash`, `uri_hash`, `query`, `header` and `cookie`, plus active health checks (`health_uri`, `health_interval` default 30s) and passive ones (`fail_duration`, `max_fails`) ([Caddyfile reverse_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)). The JSON module page at `/docs/modules/http.handlers.reverse_proxy` returned only navigation when fetched on 2026-09-03, so the Caddyfile directive page is the source used here.
- Traefik services default to `wrr`, which "Distributes requests evenly across all servers in rotation, respecting server weights", with `p2c`, `hrw` and `leasttime` as alternatives, sticky cookies and active health checks ([services](https://doc.traefik.io/traefik/reference/routing-configuration/http/load-balancing/service/)).
- ingress-nginx runs its load-balancing algorithms in Lua so that endpoint changes do not reload NGINX ([how it works](https://kubernetes.github.io/ingress-nginx/how-it-works/)).
- Kubernetes: EndpointSlice carries per-endpoint `ready`, `serving` and `terminating` conditions, which Endpoints never had ([Endpoints deprecation](https://kubernetes.io/blog/2025/04/24/endpoints-deprecation/)).

## Decision

`upstream.Picker` is the interface: `Pick() (addr string, ok bool)` and `Len() int`. `upstream.NewRoundRobin(addrs)` is the only implementation in the MVP: one `atomic.Uint32`, `i := next.Add(1) - 1`, return `addrs[i%len(addrs)]`. It starts at the first address so tests are deterministic, and an empty picker reports `ok=false`, which `proxy.Handler` turns into a 503. `router.Compile` builds one picker per `model.Backend` from its endpoints with `Ready == true` and stores it in `router.Route.Backend`, which is never nil. Counters therefore live inside the compiled `Table` and reset to zero on every swap.

Not in the MVP: weights, least connections, hashing or sticky sessions, active health checks, passive ejection, retries. "Healthy" means the EndpointSlice `ready` condition at translation time, with nil read as true.

## Consequences

Positive:
- Exactly one mutable cell per backend, no registry outside the table, no goroutines, no delete bookkeeping.
- Deterministic tests: two endpoints, four requests, A B A B.
- Matches what Ingress can express. Nothing is configurable that the API cannot declare.

Negative:
- After each swap every counter restarts at the first endpoint, so under frequent configuration churn endpoint 0 receives a few extra requests. Accepted; not yet measured.
- A pod that misbehaves while still `ready` keeps receiving 1/N of the requests until kubelet marks it not ready. No faster reaction exists in the MVP.
- Long or expensive requests are not balanced by load; round-robin counts requests, not work.
- `Uint32` wrap-around is harmless with modulo but tests must not depend on the absolute value.

## Alternatives considered

- **Random, Caddy's default.** No mutable state at all, so the table would be fully immutable. Nondeterministic tests and visibly uneven at two or three endpoints. Why not for sluice now: round-robin is the expected teaching default and costs one counter.
- **Weighted round-robin, Traefik's default.** Ingress has no weight field; Gateway API `backendRefs.weight` will provide one. Why not for sluice now: no source of weights until slice 8.
- **Least connections or power of two choices.** Needs an in-flight counter decremented when the response completes, shared across swaps to mean anything. Why not for sluice now: a second concurrency surface that touches the end of every request.
- **Counters keyed by `BackendKey` that survive swaps.** Smoother under churn. Needs a registry with insert and delete bookkeeping, and leaks when backends vanish. Why not for sluice now: it breaks "the table is immutable except one counter per backend", and the `Picker` interface keeps the door open.
- **Active health checks.** A goroutine per backend inside the table violates the no-live-resources rule of ADR 0003, and Kubernetes readiness probes already exist. Why not for sluice now: duplicate signal at a real lifecycle cost.
- **Passive health checks or retries.** Need shared failure state and idempotency rules. Why not for sluice now: out of scope for the MVP.

## Revisit when

- Slice 8 introduces `HTTPRoute` `backendRefs.weight`.
- The race and benchmark tests show measurable imbalance after swaps under a realistic churn pattern.
- A table needs to own live health checkers, which reopens the release rule of ADR 0003 at the same time.

## References

- Caddy reverse_proxy directive: https://caddyserver.com/docs/caddyfile/directives/reverse_proxy
- Caddy reverse_proxy module (fetch returned navigation only on 2026-09-03): https://caddyserver.com/docs/modules/http.handlers.reverse_proxy
- Traefik HTTP services (the `/routing/services/` URL in the brief redirects here): https://doc.traefik.io/traefik/reference/routing-configuration/http/load-balancing/service/
- ingress-nginx, how it works: https://kubernetes.github.io/ingress-nginx/how-it-works/
- Kubernetes v1.33 Endpoints deprecation: https://kubernetes.io/blog/2025/04/24/endpoints-deprecation/
