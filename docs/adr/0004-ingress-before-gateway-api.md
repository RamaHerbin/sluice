# ADR 0004: Ingress first, Gateway API later on the same model

Status: Accepted, 2026-09-03

## Context

sluice needs a Kubernetes API to read routing intent from. Two candidates exist: `networking.k8s.io/v1` Ingress, built into every cluster but frozen, and Gateway API, the designated successor, which ships as a separate project with its own releases and resources. The MVP goal is to teach the controller-to-table pipeline, not to cover the richest API. Whatever is chosen must claim only the objects meant for sluice, since clusters often run several controllers.

## Prior art

- Kubernetes docs: the Ingress API is frozen and the project recommends Gateway API for new work; `IngressClass.spec.controller` "contains a domain-prefixed path and cannot be changed once the resource has been created"; the `kubernetes.io/ingress.class` annotation is deprecated in favor of `spec.ingressClassName`; on multiple matches the longest path wins and Exact beats Prefix at equal length; a wildcard host matches exactly one label and not the bare domain ([Ingress](https://kubernetes.io/docs/concepts/services-networking/ingress/)).
- Gateway API: "the next generation of Kubernetes Ingress, Load Balancing, and Service Mesh APIs", role-oriented, portable, expressive and extensible; `Gateway`, `GatewayClass` and `HTTPRoute` are Standard Channel since v0.5.0 ([Gateway API](https://gateway-api.sigs.k8s.io/)). A controller "does so by providing an associated GatewayClass resource", which is "similar to IngressClass for Ingress", and routes attach through `parentRefs` ([API overview](https://gateway-api.sigs.k8s.io/docs/concepts/api-overview/)). The "migrating from Ingress" guide could not be fetched on 2026-09-03 (404 at the two paths tried); the Ingress-to-Gateway mapping is taken from `ingress2gateway`, which "helps translate Ingress and provider-specific resources (CRDs) to Gateway API resources" ([ingress2gateway](https://github.com/kubernetes-sigs/ingress2gateway)).
- Traefik ships three Kubernetes providers (Ingress, IngressRoute CRD, Gateway API) that feed the same dynamic configuration. Its Ingress provider states "The Kubernetes Ingress API is frozen and will not receive further changes or updates", claims through an `IngressClass` with `controller: traefik.io/ingress-controller`, and since v3.1 reads EndpointSlices instead of Endpoints ([Ingress provider](https://doc.traefik.io/traefik/reference/install-configuration/providers/kubernetes/kubernetes-ingress/), [v3.0 provider](https://doc.traefik.io/traefik/v3.0/providers/kubernetes-ingress/), [v3 migration](https://doc.traefik.io/traefik/v3.3/migration/v3/)).
- Kubernetes: `v1 Endpoints` is deprecated since 1.33 (KEP-4974) and EndpointSlice is the only API for new Service features ([blog](https://kubernetes.io/blog/2025/04/24/endpoints-deprecation/)).

## Decision

Slice 4 implements `networking.k8s.io/v1` `Ingress` and `IngressClass` with four informers (Ingresses, IngressClasses, Services, EndpointSlices). Claiming follows the IngressClass mechanism: sluice ships an `IngressClass` whose `spec.controller` is `controller.DefaultControllerName` (`sluice.dev/ingress-controller`), and `controller.Translate` claims an Ingress when `spec.ingressClassName` names such a class. As a fallback it accepts an empty `ingressClassName` together with `controller.LegacyClassAnnotation` (`kubernetes.io/ingress.class`) equal to the class name. Unclassed Ingresses are not claimed. Endpoints come from `discovery.k8s.io/v1` EndpointSlice only. Matching semantics follow the Ingress spec as listed in `docs/design/kubernetes.md` section 4, with `ImplementationSpecific` treated as Prefix, and `defaultBackend` and status publication skipped. The only extension is the annotation pair `controller.AnnotationRequestHeaders` and `controller.AnnotationResponseHeaders`.

Gateway API is slice 8: `GatewayClass` (claimed by `controllerName`), `Gateway` and `HTTPRoute` are translated into the same `model.Snapshot` by a sibling of `Translate`. `router`, `proxy` and `upstream` do not change.

## Consequences

Positive:
- Works on any conformant cluster, including a bare kind cluster, with no CRD installation.
- Small RBAC and a small informer set; the Ingress spec examples become a table-driven `Translate` test.
- The frozen API will not move while the core is being built.

Negative:
- Annotations are the only extension point, and Ingress has no typed way to express header rules, weights or header matches. Gateway API exists to fix exactly this.
- Two translators to maintain after slice 8, and the model will have to grow to carry Gateway API concepts (see ADR 0002, revisit).
- Users on clusters that only expose Gateway API get nothing until slice 8.

## Alternatives considered

- **Gateway API first.** A richer, typed model from day one and no annotation parsing. Needs the Gateway API resources installed in every test cluster, more informers (`GatewayClass`, `Gateway`, `HTTPRoute`, `ReferenceGrant`) and status conditions that route authors rely on to know a route was accepted. Why not for sluice now: more surface before the pipeline lesson lands.
- **An own CRD, Traefik `IngressRoute` style.** Full control of the schema. Needs CRD design, generated clients, and offers no portability. Why not for sluice now: sluice should not invent an API when two standard ones exist.
- **Both in slice 4.** Doubles the first Kubernetes slice. Why not for sluice now: sequencing risk with no pedagogical gain.

## Revisit when

- Slice 8 starts, at which point the model gap is measured against `HTTPRoute`.
- The target clusters ship Gateway API resources by default and users ask for `HTTPRoute` before slice 8.
- The Kubernetes project announces a removal date for Ingress, which its docs currently do not.

## References

- Kubernetes Ingress: https://kubernetes.io/docs/concepts/services-networking/ingress/
- Gateway API (the root URL redirects to `/docs/`): https://gateway-api.sigs.k8s.io/
- Gateway API overview: https://gateway-api.sigs.k8s.io/docs/concepts/api-overview/
- ingress2gateway: https://github.com/kubernetes-sigs/ingress2gateway
- Traefik Kubernetes Ingress provider (the `/providers/kubernetes-ingress/` URL in the brief redirects here): https://doc.traefik.io/traefik/reference/install-configuration/providers/kubernetes/kubernetes-ingress/
- Traefik v3.0 Kubernetes Ingress provider: https://doc.traefik.io/traefik/v3.0/providers/kubernetes-ingress/
- Traefik v3 migration notes: https://doc.traefik.io/traefik/v3.3/migration/v3/
- Kubernetes v1.33 Endpoints deprecation: https://kubernetes.io/blog/2025/04/24/endpoints-deprecation/
