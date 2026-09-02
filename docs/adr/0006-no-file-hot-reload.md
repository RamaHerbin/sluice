# ADR 0006: No configuration file and no file hot-reload

Status: Accepted, 2026-09-03

## Context

Most reverse proxies are configured from a file and reload it on a signal, an admin call or a file watch. sluice is Kubernetes-only: the objects the controller watches already describe every route, and the informers plus the atomic swap already implement "watch and reload". A file would be a second source of truth with merge rules, a parser, permission and mount pitfalls, and its own reload path. Process settings (addresses, log format) are still needed, but they change with a rolling restart, which is how Kubernetes changes any pod setting.

## Prior art

- Caddy loads a JSON document from a file, stdin or the admin API; `caddy reload` "has the same effect as POSTing a document to the /load endpoint"; `caddy run --watch` "will watch the config file and automatically reload it after it changes" and is "intended for use only in local development environments" ([command line](https://caddyserver.com/docs/command-line), [API](https://caddyserver.com/docs/api)).
- Traefik separates static configuration (file, flags, environment; a restart to change) from dynamic configuration delivered by providers: "when Traefik detects a change, it dynamically updates the routes" ([providers overview](https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/)). Its file provider watches with fsnotify when `watch` is true, and its docs warn that renaming or moving mounted files can break notifications, which is how Kubernetes updates ConfigMap mounts ([file provider](https://doc.traefik.io/traefik/reference/install-configuration/providers/others/file/)).
- Kubernetes informers keep a local cache in sync with the API server through list and watch, deliver event notifications to handlers that must not block, and replay the cache on resync ([client-go cache](https://pkg.go.dev/k8s.io/client-go/tools/cache)). Polling the API server instead would add latency and load for no gain.

## Decision

sluice has no configuration file format, no `-config` flag, no file watcher, no `SIGHUP` handler and no configuration endpoint on the admin port. `cmd/sluice.run()` parses a handful of process flags (`-listen`, `-admin`, `-log-json`, `-log-level`, and from slice 4 the kubeconfig and class flags) and nothing else. Routing configuration is Kubernetes objects only: `controller.Controller` informers kick a rebuild, `controller.Translate` produces a `model.Snapshot`, `router.Store.Apply` publishes it (ADR 0003). Changing a process flag means changing the Deployment and letting Kubernetes roll it. `observability.AdminMux` serves `/healthz`, `/readyz` and `/metrics` only.

The repeatable `-route host/path=addr[,addr]` flag (`routeFlags` and `parseRoutes` in `cmd/sluice/main.go`) exists so slices 1 to 3 can run the data plane without a cluster. It is removed at slice 4. `docs/design/architecture.md` lists a possible `-dev` mode as a reversible decision; if it returns, it returns as flags, not as a file.

## Consequences

Positive:
- One source of truth and one update path, exercised by every informer event.
- No parser, no schema versioning, no precedence rules between file and cluster, no ConfigMap-mount surprises.
- The admin port exposes nothing that changes behavior; there is no `/load` to protect.

Negative:
- After slice 4 sluice cannot run outside a cluster. Unit tests use a fake clientset; anything end to end needs kind.
- Tunables such as timeouts are compile-time defaults or flags. Changing one restarts the pod.
- No cluster-wide settings object like the ingress-nginx ConfigMap. If one becomes necessary it will be a Kubernetes object watched by an informer, which keeps this ADR intact.

## Alternatives considered

- **A JSON or YAML config file with adapters, Caddy style.** Right for a general-purpose server that runs anywhere. Why not for sluice now: sluice runs in one place and its configuration already lives in the API server.
- **A Traefik-style file provider with `watch`.** Would let an operator add static routes next to Ingress routes. Requires fsnotify, mount-aware handling and precedence rules between two sources. Why not for sluice now: a second source of truth is the thing this ADR avoids.
- **A ConfigMap for global settings.** Fits the "Kubernetes objects only" rule and could be added inside `internal/controller`. Why not for sluice now: no setting needs a runtime change yet.
- **`SIGHUP` to re-read flags.** Flags are process-level and a rolling restart does the same job in Kubernetes. Why not for sluice now: no benefit over a restart.
- **An admin API such as Caddy's `POST /load`.** Needs authentication and becomes a way around RBAC on routing. Why not for sluice now: attack surface with no user.

## Revisit when

- A setting must change at runtime without a restart, for example the log level during an incident. The answer would be a ConfigMap informer, not a file.
- Someone needs the data plane without a cluster after slice 4. The answer would be a `-dev` flag set, not a file.

## Dissent

Dropping `-route` at slice 4 removes the only way to exercise `proxy.Handler` end to end without kind. A tiny `-dev` mode that keeps `parseRoutes` alive would cost little and help contributors. The architecture already lists it as reversible; the decision to remove the flags stands.

## References

- Caddy command line: https://caddyserver.com/docs/command-line
- Caddy admin API: https://caddyserver.com/docs/api
- Traefik providers overview (the `/providers/overview/` URL in the brief redirects here): https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/
- Traefik file provider: https://doc.traefik.io/traefik/reference/install-configuration/providers/others/file/
- client-go tools/cache: https://pkg.go.dev/k8s.io/client-go/tools/cache
