# ADR 0001: One binary, control plane and data plane in one process

Status: Accepted, 2026-09-03

## Context

sluice is a pedagogical Kubernetes-only reverse proxy meant to run as a single replica inside a cluster. It has a control plane (watch Kubernetes objects, translate, validate, publish) and a data plane (match a request, pick an endpoint, proxy it). The two planes could live in one process or in two, and that choice decides how configuration crosses between them: as a Go pointer, or as bytes over a file or a socket. The teaching goal is the in-memory publication of an immutable routing table, so the boundary should stay visible in code but cost nothing at runtime.

## Prior art

- Caddy is "a single, self-contained, static binary with zero external dependencies", with a small core that owns `Run()` and `Stop()` and modules that do everything else, the reverse proxy included ([architecture](https://caddyserver.com/docs/architecture)).
- Traefik is one Go binary in which providers (Docker, Kubernetes Ingress, Kubernetes CRD, Gateway API, File, key-value stores) feed one dynamic configuration to one router ([providers overview](https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/)).
- ingress-nginx is two components: a Go controller that builds a model from Ingresses, Services and Endpoints, writes `nginx.conf` and reloads NGINX, plus a Lua layer that pushes endpoint changes into the running NGINX so that "we don't reload Nginx on changes that impact only an upstream configuration" ([how it works](https://kubernetes.github.io/ingress-nginx/how-it-works/)). The split buys a battle-tested data plane at the price of a serialization format and a reload protocol.

## Decision

`cmd/sluice` is one process. `run()` builds one `router.Store`, one `proxy.Handler`, one `controller.Controller` (wired in at slice 4) and two `http.Server` values: the proxy on `-listen` and `observability.AdminMux` (`/healthz`, `/readyz`, `/metrics`) on `-admin`. The only object that crosses from control plane to data plane is `*router.Table`, obtained through `router.Store.Current()`; there is no wire format. Separation is enforced at the package level instead of the process level: only `internal/controller` imports `k8s.io/*` (checked by `make check-boundary`), `internal/proxy` reads tables and nothing else, and `internal/observability` is imported by `main` only. Shutdown is ordered inside the one process: readiness false, `Shutdown` on both servers, cancel the root context so informers stop, wait, then `Handler.CloseIdleConnections()`.

sluice splits into two processes only if a concrete need appears. None is known today.

## Consequences

Positive:
- One image, one Deployment, one RBAC subject, one log stream, one shutdown sequence.
- Tests drive `Store.Apply` and `Handler.ServeHTTP` in-process; the race test in `docs/design/architecture.md` section 3 needs no IPC.
- No configuration serialization to version or to secure. The publication mechanism (ADR 0003) is the whole story.

Negative:
- A control-plane fault (a panic, memory growth from a large informer cache) takes the data plane down with it.
- Scaling the data plane to N replicas means N sets of informers and N watches on the API server.
- NGINX or Envoy cannot be swapped in as the data plane; the proxy is `net/http/httputil.ReverseProxy` and stays so.
- From slice 7, the TLS Secrets the control plane reads sit in the same address space as request handling.

## Alternatives considered

- **Controller plus separate proxy, ingress-nginx style.** Gains process isolation and a replaceable data plane. Needs a serialized form of the table or snapshot, a reload protocol and two lifecycles to test. Why not for sluice now: at one replica there is nothing to isolate, and the pointer swap is the lesson.
- **Control plane feeding Envoy over xDS, Contour or Emissary style.** Production-grade data plane and a rich feature set. Pulls in gRPC and the xDS protobuf packages, against the stdlib-first rule. Why not for sluice now: the dependency footprint would dwarf the project.
- **Library only, to embed in someone else's binary.** Exported APIs would have to be stable; the `internal/` tree says the opposite. Why not for sluice now: sluice is an executable, not an API.

## Revisit when

- Several data-plane replicas must share one controller, because API server watch load or Secret access has to be centralized.
- A non-Go or hardened data plane becomes a requirement.
- Slice 7 needs Secrets isolated from the request path.

## Dissent

One process is right for the MVP, but the Secrets point is real: once TLS lands, a bug in header templating and a bug in Secret handling live one goroutine apart. Recorded so the split is not forgotten when slice 7 is designed. The decision stands.

## References

- Caddy architecture: https://caddyserver.com/docs/architecture
- Traefik providers overview (the `/providers/overview/` URL in the brief redirects here): https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/
- ingress-nginx, how it works: https://kubernetes.github.io/ingress-nginx/how-it-works/
