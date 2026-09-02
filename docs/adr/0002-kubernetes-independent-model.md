# ADR 0002: A Kubernetes-independent routing model

Status: Accepted, 2026-09-03

## Context

The router needs hosts, paths, backends, endpoint addresses and header rules. Kubernetes objects carry far more than that, in types that are large, pointer-heavy and versioned: `k8s.io/api`, `k8s.io/apimachinery` and `k8s.io/client-go` v0.37.0 bring about 58 transitive modules. The APIs also move: `v1 Endpoints` is deprecated since Kubernetes 1.33 in favor of EndpointSlice, Ingress is frozen, and Gateway API is its successor. If the data plane spoke Kubernetes types directly, every such move would ripple into matching and proxying code, and every router test would need API fixtures.

## Prior art

- Traefik has one dynamic configuration (entrypoints, routers, services, middlewares) that every provider produces; Kubernetes Ingress, the IngressRoute CRD and Gateway API are three providers feeding the same model ([providers overview](https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/), [services](https://doc.traefik.io/traefik/reference/routing-configuration/http/load-balancing/service/)).
- Caddy has one native JSON configuration; config adapters translate other formats into it, and the core treats the `apps` part of the document as opaque ([architecture](https://caddyserver.com/docs/architecture)).
- ingress-nginx builds an internal model from "Ingresses, Services, Endpoints, Secrets, and Configmaps", rebuilds it entirely on each change and compares it with the current one ([how it works](https://kubernetes.github.io/ingress-nginx/how-it-works/)).
- Kubernetes: "New Service features like dual-stack networking and traffic distribution are only supported via the EndpointSlice API" ([Endpoints deprecation, KEP-4974](https://kubernetes.io/blog/2025/04/24/endpoints-deprecation/)).

## Decision

`internal/model` is the intermediate model and imports only `fmt` and `time`. It holds `Snapshot{Routes, Backends}`, `Route`, `Backend`, `Endpoint{Address, Ready}`, `HeaderRule`, `RouteKey`, `BackendKey` and `PathType` with exactly two values, `PathPrefix` and `PathExact`. `Snapshot.Normalize()` and `Snapshot.Hash()` make two equal states hash identically.

`internal/controller` is the only package that imports `k8s.io/*`; `make check-boundary` fails otherwise. `controller.Translate(Input) (*model.Snapshot, []Warning)` is a pure function and the only code that reads Kubernetes objects. It maps `ImplementationSpecific` to `PathPrefix`, reads EndpointSlices and never `v1 Endpoints`, skips what it cannot express with a `Warning`, and never fails. `router.Compile(*model.Snapshot)` validates and builds a `Table`; `proxy.Handler` reads `*router.Table` and nothing upstream of it.

Gateway API (slice 8) will be a second translation into the same `model.Snapshot`. The static `-route` flags of slices 1 to 3 (`parseRoutes` in `cmd/sluice/main.go`) already produce one, which is the existence proof that the model does not depend on its source.

## Consequences

Positive:
- `router`, `proxy`, `upstream` and `placeholders` tests use plain structs, compile fast and need no client.
- API churn is absorbed in one function. The Endpoints to EndpointSlice move would have touched `Translate` only.
- Deterministic `Normalize` and `Hash` let `router.Store` skip no-op rebuilds (`changed=false`).

Negative:
- A translation layer to write and test, and two vocabularies for one thing (Ingress rule versus `model.Route`).
- Information the model cannot hold is dropped: `defaultBackend`, resource backends, Ingress status. Adding any of them is a model change.
- `RouteKey{Namespace, Name, Rule, Path}` is shaped after Ingress indices. Gateway API `HTTPRoute` rules and matches fit the same shape, but the field names will read oddly there.

## Alternatives considered

- **Compile straight from `networkingv1.Ingress`.** One layer fewer. The router would import `k8s.io`, every test would build API objects, and Gateway API would need a second compiler. Why not for sluice now: the boundary is the point, and `check-boundary` would have nothing to check.
- **A serializable general schema with adapters, Caddy style.** Elegant for a general server with many input formats. sluice has one source, and a serializable schema invites a config file, which ADR 0006 rejects. Why not for sluice now: no second input format exists.
- **Use Gateway API types as the internal model.** `HTTPRoute` is expressive enough to hold an Ingress, which is what `ingress2gateway` does offline. It would put `sigs.k8s.io/gateway-api` into the router. Why not for sluice now: the same boundary problem, and Gateway API arrives only in slice 8.

## Revisit when

- Slice 8 needs a concept the model cannot express (header or method matching, weighted `backendRefs`, filters) and the model starts to mirror `HTTPRoute`. Then decide whether to grow it or to adopt a richer shape deliberately.
- A second real source of configuration appears, which ADR 0006 currently rules out.

## References

- Traefik providers overview: https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/
- Traefik HTTP services: https://doc.traefik.io/traefik/reference/routing-configuration/http/load-balancing/service/
- Caddy architecture: https://caddyserver.com/docs/architecture
- ingress-nginx, how it works: https://kubernetes.github.io/ingress-nginx/how-it-works/
- Kubernetes v1.33 Endpoints deprecation: https://kubernetes.io/blog/2025/04/24/endpoints-deprecation/
- ingress2gateway: https://github.com/kubernetes-sigs/ingress2gateway
