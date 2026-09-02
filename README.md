# sluice

An experimental reverse proxy and ingress controller that runs **only inside Kubernetes**, written in Go.

sluice exists to learn, by building it, how these things actually work:

- a reverse proxy on top of `net/http` and `net/http/httputil`;
- a Kubernetes controller driven by informers;
- dynamic service discovery through `Service` and `EndpointSlice`;
- HTTP routing by host and path with deterministic priority;
- configuration updates with no restart and no interrupted request.

It borrows the readability and request-time placeholders of Caddy, and the Kubernetes-native discovery of Traefik, inside an architecture small enough that one person can follow the full path from a Kubernetes event to the upstream chosen for an HTTP request.

sluice is **not** production-ready and does not claim to be. It is not a general web server, not a multi-environment orchestrator, and not a replacement for Traefik, Caddy, Envoy or NGINX.

## How it works

One binary, two logical halves.

```
Kubernetes API ──informers──▶ controller ──Translate──▶ model.Snapshot
                                                            │ Compile + validate
                                                            ▼
                                                     router.Table (immutable)
                                                            │ atomic swap
HTTP request ──▶ proxy.Handler ──Match(host, path)──▶ Route ──Pick()──▶ upstream
```

- **Control plane** (`internal/controller`): watches `Ingress`, `IngressClass`, `Service` and `EndpointSlice`, turns them into a Kubernetes-independent `model.Snapshot`, and publishes it. It is the only package that imports `k8s.io/*`.
- **Router** (`internal/router`): validates and compiles a snapshot into an immutable table, then swaps it in with an `atomic.Pointer`. An invalid snapshot never replaces the last valid table.
- **Data plane** (`internal/proxy`, `internal/upstream`): loads the current table once per request, matches host and path, picks an endpoint in round-robin, and proxies with `httputil.ReverseProxy`. In-flight requests keep the table they started with.
- **Placeholders** (`internal/placeholders`): a tiny, explicit set of request-time variables such as `{http.request.host}` or `{k8s.service}`, usable in headers added to the upstream request or the response.

Design documents live in `docs/design/`, decisions in `docs/adr/`.

## Status

Under construction. See `docs/design/roadmap.md` for the vertical slices and what is done.

## Scope and non-goals

In scope for the MVP: HTTP routing by host and path, `Ingress` (networking.k8s.io/v1), `Service` + `EndpointSlice` discovery, round-robin, forwarded headers, 404/502/503/504 handling, zero-restart config updates, a few placeholders, structured logs, `/healthz`, `/readyz`, Prometheus metrics, a `kind` demo.

Planned after the MVP: TLS from `Secret`s, Gateway API on the same intermediate model.

Explicitly out of scope: TCP/UDP proxying, ACME, configuration outside Kubernetes, service mesh, plugins, WebAssembly, dashboard, distributed tracing, distributed rate limiting, retries, circuit breakers, canary routing, custom CRDs, multi-cluster, persistence, HA control plane, `Ingress` status writes, `defaultBackend`, active health checks, trusted-proxy lists, compatibility with Traefik or Caddy configuration.

Note: the Kubernetes `Ingress` API is GA and frozen; Gateway API is its successor. sluice starts with `Ingress` because it is smaller and everywhere, and keeps the data plane independent of either API.

## Development

```
make fmt vet lint test test-race    # quality gates
make build                          # bin/sluice
make kind-up docker-build kind-load deploy demo   # local cluster demo, see docs
```

Requires Go 1.26+, and for the local cluster: Docker (or colima), `kind`, `kubectl`.

## License

MIT.
