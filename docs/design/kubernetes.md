# Kubernetes integration

Status: v0, facts verified 2026-09-03 against pkg.go.dev, kubernetes.io docs and upstream tags.

## 1. Dependencies

| Module | Version | Note |
|---|---|---|
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | v0.37.0 (Kubernetes 1.37.0, 2026-08-26) | require `go 1.26.0`; about 58 transitive modules; the single big dependency |
| `github.com/prometheus/client_golang` | v1.24.1 | confined to `internal/observability` |

Types: `Ingress`, `IngressClass` in `k8s.io/api/networking/v1` (`PathTypeExact/Prefix/ImplementationSpecific`, `ServiceBackendPort{Name; Number}`), `EndpointSlice` in `k8s.io/api/discovery/v1` (`LabelServiceName = "kubernetes.io/service-name"`, `AddressTypeIPv4/IPv6/FQDN`, `EndpointConditions{Ready, Serving, Terminating *bool}`, `EndpointPort{Name *string; Port *int32; Protocol *v1.Protocol}`).

`v1 Endpoints` is deprecated since Kubernetes 1.33 (KEP-4974): EndpointSlice only. `Ingress` is GA and frozen; Gateway API is the successor. No relevant API removals through 1.37.

## 2. Informers

- One `informers.NewSharedInformerFactoryWithOptions(client, 0, ...)` with resync 0 (resync replays the local cache without contacting the API server; a full-rebuild design gains nothing from it). Optional belt-and-braces: a 10 min ticker that marks dirty.
- Informers: `Networking().V1().Ingresses()`, `Networking().V1().IngressClasses()`, `Core().V1().Services()`, `Discovery().V1().EndpointSlices()`.
- EndpointSlice indexer: `AddIndexers(cache.Indexers{"service": ns + "/" + labels[LabelServiceName]})`, lookup via `ByIndex("service", key)`.
- Secrets (slice 7): a **second** factory with `WithTweakListOptions(o.FieldSelector = "type=kubernetes.io/tls")`. `type` is a supported field selector for Secret. Tweak options apply to every informer of a factory, hence the separate factory.
- Handlers: `cache.ResourceEventHandlerFuncs{AddFunc, UpdateFunc, DeleteFunc}` on all four informers; each only calls `kick()` (non-blocking send on a size-1 channel).
- Tombstones: `cache.DeletedFinalStateUnknown` arrives in `DeleteFunc` when a delete was missed during a watch disconnect. Because we rebuild from cache, `DeleteFunc` ignores the payload; a comment explains why no type assertion is needed.
- Readiness: `factory.Start(ctx.Done())`, then `factory.WaitForCacheSync(ctx.Done())` and fail if any informer reports false; first rebuild; then `/readyz` flips to 200.
- Coalescing: dirty-flag channel over `workqueue`. The typed workqueue adds keys, retries and rate limits that a full recompute does not need.

## 3. Class claiming

- Ship an `IngressClass` with `spec.controller: sluice.dev/ingress-controller` (domain-prefixed, immutable). Default class name `sluice`, user-chosen.
- Claim an Ingress when `spec.ingressClassName` names an IngressClass whose `spec.controller` equals ours.
- Fallback: `ingressClassName` empty and annotation `kubernetes.io/ingress.class` equal to the class name (deprecated; no constant in `k8s.io/api`, we define our own).
- `ingressclass.kubernetes.io/is-default-class`: the admission controller writes `ingressClassName` into new Ingresses when exactly one default class exists; Ingresses created earlier are not modified. MVP: do not claim unclassed Ingresses.

## 4. Ingress semantics implemented

- `rules[].host` empty = all hosts. `*.example.com` matches exactly one label (`bar.example.com`), not `example.com` nor `a.b.example.com`. Wildcard only as the leftmost label.
- `pathType: Exact` is case-sensitive, trailing slash significant. `Prefix` is element-wise on `/`: `/foo/bar` matches `/foo/bar/baz`, not `/foo/barbaz`; `/foo` matches `/foo/`. Longest match wins. `ImplementationSpecific` is treated as Prefix (documented). `path` must start with `/`; an empty path (allowed only for ImplementationSpecific) is treated as `/`.
- Precedence: exact host > wildcard host > empty host (no fallthrough); then the longest matching path, Exact beating Prefix at equal length (implemented as exact-path map first, then longest prefix, which is equivalent); then oldest Ingress, then namespace/name.
- `backend.service.port.name` XOR `.number`. Resource backends are skipped with a warning.
- `defaultBackend`: skipped in the MVP (unmatched request = 404 with a debug log). Ingress status (`status.loadBalancer`): skipped; it would need the `ingresses/status` `update` verb and a publish address.

## 5. Service and EndpointSlice resolution

1. Ingress backend `{name, port.name | port.number}` -> Service `ns/name` from the lister. `spec.type == ExternalName`: skip with an explicit warning (no EndpointSlices exist, nothing to proxy). Headless services (`clusterIP: None`) are fine: slices exist.
2. Find the `ServicePort` by `.name == port.name` or `.port == port.number`. `ports[].name` is required only when there is more than one port; a single unnamed port has name `""`.
3. Do **not** resolve `targetPort`. `EndpointSlice.ports[].name` corresponds to `Service.ports[].name` and `ports[].port` is already the resolved target port per pod. Named targetPorts hitting pods with different container port numbers simply produce distinct slices with distinct `ports[].port`; matching per slice by name handles it.
4. Slices: `ByIndex("service", ns/name)`, union of all slices. Keep `addressType` IPv4 and IPv6; `FQDN` is deprecated with no defined semantics and is ignored. Per slice, find the port with the matching name; skip if `port.port == nil`; protocol must be TCP.
5. Endpoints: `ready` nil means true (API reference: "A nil value should be interpreted as true"); `serving` nil means true; `terminating` nil means false. MVP: include an endpoint iff ready. Later option (kube-proxy behaviour): if zero ready, fall back to `serving && terminating`. Use `addresses[0]` only (the controller emits exactly one). Result sorted for a deterministic table.

## 6. Minimal RBAC

ServiceAccount `sluice` in namespace `sluice-system`, ClusterRoleBinding to ClusterRole:

| apiGroup | resources | verbs |
|---|---|---|
| `""` | `services` | get, list, watch |
| `discovery.k8s.io` | `endpointslices` | get, list, watch |
| `networking.k8s.io` | `ingresses`, `ingressclasses` | get, list, watch |
| `""` (slice 7) | `secrets` | get, list, watch |

Secrets access is a cluster-wide read of all secret types; RBAC cannot filter by field selector. Mitigation later: Role + RoleBinding per namespace with `WithNamespace(...)`, or accept and document. Not needed: `nodes`, `events`, `leases` (single replica, no leader election), `ingresses/status`.

## 7. Testing without a cluster

- Most tests: table-driven `Translate` tests on typed synthetic objects, no client at all.
- Two or three informer-driven tests with `fake.NewClientset(objs...)` + `informers.NewSharedInformerFactory(fakeClient, 0)`: initial sync produces a table; Ingress update produces a new table pointer; EndpointSlice delete removes an endpoint.
- Pitfalls: seed objects in the constructor or create them only after `WaitForCacheSync` (a create between List and Watch registration can be missed); events are asynchronous, assert with an `eventually(t, 2*time.Second, func() bool)` helper; the fake watcher channel is bounded (about 100), so avoid hundreds of objects without consumers; no admission or defaulting, so set `ingressClassName`, labels and `addressType` explicitly.
- envtest and kind are not used in `go test`.

## 8. Local development on macOS

```
brew install kind kubernetes-cli golangci-lint     # kind 0.33.0, kubectl 1.37.0, golangci-lint 2.13.2
colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs
```

- Colima is compatible with kind and forwards published ports to the host.
- `deploy/kind/cluster.yaml`: one control-plane node with `extraPortMappings` containerPort 30080 -> hostPort 8080 on 127.0.0.1. sluice runs as a Deployment behind a `NodePort` Service on 30080. No cloud-provider-kind (needs privileges).
- Image: `docker build -t sluice:dev .` then `kind load docker-image sluice:dev`; `imagePullPolicy: IfNotPresent`; never `:latest` (kind forces `Always` on it).
- Demo backend: `mendhak/http-https-echo:41` (arm64, echoes headers as JSON, listens on 8080). Two Deployments `echo-a` and `echo-b` behind ClusterIP Services and one Ingress.
- Dockerfile: builder `golang:1.26-alpine`, `CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w"`, runtime `gcr.io/distroless/static-debian13:nonroot` (Debian 12 reaches end of life in September 2026), `USER nonroot:nonroot`, ports 8080 (proxy) and 9090 (admin).
- golangci-lint v2 config: `version: "2"`, `run.timeout: 5m`, `linters.default: standard` plus `misspell`, `unconvert`; `formatters.enable: [gofmt, goimports]`.
- GitHub Actions: `actions/checkout@v7`, `actions/setup-go@v7` (`go-version-file: go.mod`, module cache on), `golangci/golangci-lint-action@v9` (`version: v2.13.2`), gofmt check, `go vet`, `go test -race -count=1 ./...`, `docker build` without push.
