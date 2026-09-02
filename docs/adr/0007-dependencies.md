# 0007: Third-party dependencies: client-go and client_golang only

Status: Accepted, 2026-09-03

## Context

`CLAUDE.md` requires an ADR for every third-party module. The bootstrap pinned two: the Kubernetes client libraries and the Prometheus client. Everything else is the Go standard library, including HTTP serving, the reverse proxy (`net/http/httputil`), logging (`log/slog`), atomics and tests (`testing`, `net/http/httptest`).

## Decision

- `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` at v0.37.0 (Kubernetes 1.37), always the same minor for the three. Used only by `internal/controller`. Informers, listers and the typed clientset are the learning goal, so `controller-runtime` and other frameworks that hide them are excluded.
- `github.com/prometheus/client_golang` at v1.24.1. Used only by `internal/observability`; other packages see two small consumer-side interfaces (`proxy.Metrics`, `controller.Metrics`). Chosen with the project owner over a hand-rolled text exposition: the format and histogram rules are busywork, not learning.
- No assertion library, no YAML library, no CLI framework, no logging library. `hack/check-boundary.sh` enforces the two import boundaries in CI.

## Consequences

Positive: one large dependency tree instead of several; the boundary makes the data plane readable without Kubernetes knowledge; swapping the metrics library later touches one package.

Negative: client-go pulls about 58 modules into `go.sum`; upgrades of the three Kubernetes modules must move together; Prometheus histograms need bucket choices we own.

## Alternatives considered

- `sigs.k8s.io/controller-runtime`: manager, caches and reconcilers ready-made. Why not for sluice now: it hides exactly the informer mechanics this project exists to learn.
- Hand-rolled Prometheus text exposition with `sync/atomic` counters: zero dependencies, about 150 lines. Why not for sluice now: the owner preferred the standard client; the boundary keeps the option open.
- `testify` or similar: nicer assertions. Why not for sluice now: table-driven tests with the standard library are enough and keep the dependency list honest.

## Revisit when

A slice needs a capability the standard library cannot provide cleanly (for example Gateway API types, which live in `sigs.k8s.io/gateway-api`), or a dependency upgrade forces a Go version bump the project does not want.
