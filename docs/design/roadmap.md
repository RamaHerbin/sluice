# Roadmap: vertical slices

The plan of record. A slice is done when its exit criteria hold and the quality gates pass (`make ci` and `make lint`). Slices are validated in order and never skipped. Status values: todo, in progress, done.

| Slice | Content | Exit criteria | Status |
|---|---|---|---|
| 0 Bootstrap | module, design docs, frozen contracts, devx scaffolding, ADRs | `go build ./...`, `make ci` green, first commits | done (2026-09-03) |
| 1 Static proxy | `-route host/path=addr[,addr]` flags, one backend, reverse proxy | `curl` through the proxy to an `httptest` backend, unit and integration tests | done (2026-09-03, `TestStaticRouteEndToEnd`) |
| 2 Routing | several hosts and paths, deterministic priority, round-robin, 404/502/503/504 | scenarios 1, 2, 3, 8, 9, 10 in `docs/testing.md` | done (2026-09-03) |
| 3 Atomic config | snapshot build, validation, atomic swap, last known good | scenarios 6, 7, hammer test green under `go test -race -count=3` | done (2026-09-03) |
| 4 Kubernetes | informers, Ingress + IngressClass + Service + EndpointSlice, add/update/delete, readiness after sync | scenarios 4, 5, 13, 14 with the fake clientset, `make check-boundary` | done (2026-09-03), not yet run against a real API server |
| 5 Placeholders | expansion in upstream and response headers, compile-time errors | scenarios 11, 12; an unknown placeholder in an annotation skips that Ingress with a warning and a metric | done (2026-09-03) |
| 6 Local cluster | image, kind, manifests, echo demo | from a clean machine: `colima start`, `make kind-up docker-build kind-load deploy`, `make demo` passes | manifests written; blocked on local tooling (kind, kubectl not installed, colima stopped) |
| 7 TLS | Secret informer, certificates per host, dynamic replacement, deterministic behavior when a secret is missing or invalid | after MVP validation | later |
| 8 Gateway API | Gateway, GatewayClass, HTTPRoute translated into the same `model.Snapshot` | after the model stabilizes | later |

## How the work is organised

- Wave 1 (done): design and scaffolding on disjoint files: data-plane prototype (deleted, findings in `dataplane.md`), placeholders design, ADRs, devx.
- Wave 2 (done 2026-09-03): seven implementation agents on disjoint directories, building toward slice 5 in parallel: model + router, proxy + upstream, controller, placeholders, packaging (`deploy/k8s`, `examples`), observability. The orchestrator owns `go.mod`, `go.sum`, `cmd/sluice`, `README.md`, `CLAUDE.md`, `docs/design`, and integrates.
- Slices 1 to 5 validated by the orchestrator on 2026-09-03 (package tests, the `cmd/sluice` end-to-end test, full gate). Slice 6 waits for `brew install kind kubernetes-cli golangci-lint` and `colima start`, then the checklist in `docs/design/devx.md` section 7.

## `cmd/sluice/main.go` per slice

- Slices 1 to 3: `-listen`, `-admin`, `-log-json`, `-log-level`, repeatable `-route`. Static snapshot applied at startup; tests drive swaps.
- Slice 4: `-kubeconfig` (empty means in-cluster), `-ingress-class` (default `sluice`), `-admin`. Clientset, store, metrics, controller, admin server, wait for `Synced()`, readiness on, proxy server, ordered shutdown. The `-route` flag is removed (ADR 0006).

## Out of scope for the MVP

TCP/UDP, ACME, configuration outside Kubernetes, service mesh, plugins, WebAssembly, dashboard, distributed tracing, distributed rate limiting, retries, circuit breakers, canary routing, custom CRDs, multi-cluster, persistence, HA control plane, Ingress status writes, `defaultBackend`, active health checks, trusted-proxy lists, Traefik or Caddy compatibility. Documented as possible extensions in the README, not as work.
