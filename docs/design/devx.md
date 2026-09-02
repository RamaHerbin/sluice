# Developer experience

Status: v0, written 2026-09-03 with the Go scaffolding in place and the package
bodies still stubbed. Everything here was exercised on the development Mac
except the parts that need a Docker daemon or a kind cluster; section 6 lists
those precisely and section 7 says how to validate them later.

## 1. Tool versions

Versions below are the targets verified on 2026-09-03. Go, Make, gofmt and the Docker CLI are installed on the dev machine; kind, kubectl and golangci-lint are not yet (see section 6).

| Tool | Version | Where it is used |
|---|---|---|
| Go | 1.26.4 locally; `go 1.26.0` in `go.mod`; `golang:1.26-alpine` in the Dockerfile; `actions/setup-go@v7` with `go-version-file: go.mod` in CI | everything |
| GNU Make | 3.81 (the macOS default) | `Makefile` is written for 3.81: no `.ONESHELL`, no `::=`, no `$(file ...)` |
| gofmt | ships with Go | `make fmt`, `make fmt-check` |
| golangci-lint | 2.13.2 (v2 config format) | `make lint` locally, `golangci/golangci-lint-action@v9` in CI |
| Docker CLI | present; daemon is colima, stopped by default | `make docker-build`, `make kind-load` |
| kind | 0.33.0 (`brew install kind`) | `make kind-up`, `make kind-load`, `make kind-down` |
| kubectl | 1.37.0 (`brew install kubernetes-cli`) | `make deploy`, `make undeploy`, `make demo` |
| GitHub Actions | `actions/checkout@v7`, `actions/setup-go@v7`, `golangci/golangci-lint-action@v9` | `.github/workflows/ci.yml` |
| Images | builder `golang:1.26-alpine`, runtime `gcr.io/distroless/static-debian13:nonroot`, demo echo `mendhak/http-https-echo:41` | Dockerfile, demo manifests (written in wave 2 under `examples/echo/`) |

Nothing in this repository installs tools. `make lint` prints how to install
golangci-lint and exits non-zero when it is absent; `hack/kind-up.sh` and
`hack/demo.sh` check for their tools and stop with a message.

## 2. Make targets

`make` with no argument prints this list. Descriptions live in the `## `
comment after each target, so the list never drifts from the file.

| Target | What it runs | Needs |
|---|---|---|
| `help` | lists targets (default goal) | nothing |
| `fmt` | `gofmt -l -w cmd internal` | Go |
| `fmt-check` | `gofmt -l cmd internal`, fails and lists files when the output is not empty | Go |
| `vet` | `go vet ./...` | Go |
| `lint` | `golangci-lint run ./...`; install hint and exit 1 when missing | golangci-lint |
| `test` | `go test ./...` | Go |
| `test-race` | `go test -race -count=1 ./...` | Go |
| `check-boundary` | `hack/check-boundary.sh` (section 4) | Go |
| `ci` | `fmt-check vet check-boundary test-race`, the same sequence as the CI test job | Go |
| `build` | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/sluice ./cmd/sluice` | Go, git for `VERSION` |
| `clean` | `rm -rf bin` | nothing |
| `docker-build` | `docker build --build-arg VERSION=$(VERSION) -t sluice:dev .` | Docker daemon |
| `kind-up` | `hack/kind-up.sh`: create cluster `sluice` from `deploy/kind/cluster.yaml` if absent, wait for the node | kind, kubectl, Docker daemon |
| `kind-load` | `kind load docker-image sluice:dev --name sluice` | kind, Docker daemon |
| `deploy` | `kubectl apply -f deploy/k8s/` | kubectl, a cluster |
| `undeploy` | `kubectl delete -f deploy/k8s/ --ignore-not-found` | kubectl, a cluster |
| `demo` | `hack/demo.sh` (section 5) | kubectl, curl, a deployed sluice and the demo manifests |
| `kind-down` | `kind delete cluster --name sluice` | kind |

Variables you can override on the command line: `GO` (default `go`), `IMAGE`
(default `sluice:dev`), `KIND_CLUSTER` (default `sluice`), `VERSION` (default
`git describe --tags --always --dirty`, falling back to `dev` outside git).

`fmt` and `fmt-check` cover the whole repository, matching the `gofmt -l .`
gate in `CLAUDE.md`. Agents working in parallel must not run `make fmt`; they
format only their own files with `gofmt -w <file>`.

## 3. CI

`.github/workflows/ci.yml` runs on pushes to `main` and on pull requests with
`permissions: contents: read` and one concurrent run per ref. Three
independent jobs:

- `test`: checkout, setup-go (version from `go.mod`, module cache on), then
  `make fmt-check`, `make vet`, `make check-boundary`, `make test-race`. This
  is exactly `make ci`, split into steps so a failure is visible per step.
- `lint`: `golangci/golangci-lint-action@v9` pinned to `v2.13.2`, reading
  `.golangci.yml`.
- `docker`: `docker build -t sluice:ci .` with no registry login and no push.
  It proves the Dockerfile and `.dockerignore` are consistent with the tree.

`git describe` in CI sees a shallow clone without tags, so `VERSION` is the
short SHA. Only `make build` and `make docker-build` read it, and CI calls
neither (the docker job invokes `docker build` directly, so the image carries
`VERSION=dev`).

## 4. Import boundaries

`hack/check-boundary.sh` implements the two rules from `CLAUDE.md`:

1. Only `internal/controller` may depend on `k8s.io/*`. The script runs
   `go list -deps` on every package under `internal/` except the controller
   and fails if any transitive dependency starts with `k8s.io/`. `cmd/sluice`
   is not checked for this rule because it wires the controller in and
   therefore depends on `k8s.io` by construction.
2. Only `internal/observability` and `cmd/sluice` may depend on
   `github.com/prometheus/*`. Every other package is checked.

`go list -deps` follows the whole transitive graph, so a leak through a
helper package is caught as well as a direct import. On failure the script
prints the package, the number of offending import paths and the first ten
of them, and exits 1. It iterates over `go list ./...`, so any new package (for example under `prototypes/`) is covered automatically. It passes today on every package in the module.

The check covers non-test code only. Test files that import `k8s.io` from a
non-controller package would not be caught; `go list -deps -test` would
extend the rule to tests if the orchestrator wants that later.

## 5. Local loop on macOS

```
colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs
make kind-up            # cluster "sluice", 127.0.0.1:8080 -> NodePort 30080
make docker-build       # sluice:dev, never :latest (kind forces pull Always on it)
make kind-load          # copies the image into the kind node
make deploy             # deploy/k8s/: namespace sluice-system, RBAC, Deployment sluice, NodePort Service, IngressClass
kubectl apply -f <demo manifests>   # namespace demo, deployments echo-a and echo-b, ingress demo
make demo
make kind-down
```

`hack/demo.sh` waits for `deployment/sluice` in `sluice-system` and for
`echo-a` and `echo-b` in `demo`, shows the `demo` Ingress, then asserts:

- `GET http://127.0.0.1:8080/a` with `Host: demo.localtest.me` is 200 and
  prints the echo body (the JSON shows the headers the backend received, so
  `X-Forwarded-*` and any `sluice.dev/request-headers` rules are visible);
- the same for `/b`;
- `GET /` with `Host: unknown.example.invalid` is 404;
- through `kubectl port-forward deployment/sluice 9090:9090` in
  `sluice-system`, `/readyz` and `/metrics` are 200, and the `sluice_*` lines
  are printed.

The script uses `127.0.0.1` rather than `localhost` because the kind port
mapping listens on `127.0.0.1` only and `localhost` may resolve to `::1`
first. `PROXY_ADDR`, `DEMO_HOST`, `ADMIN_PORT`, `SLUICE_NS` and `DEMO_NS` are
overridable through the environment. The port-forward is killed by an EXIT
trap.

The demo manifests do not exist yet. The script is written against the names
agreed in the design: namespace `demo`, Deployments `echo-a` and `echo-b`,
Ingress `demo` for host `demo.localtest.me` with `/a` and `/b`. `examples/echo/`
is the expected home for them.

## 6. What was and was not verified today

Verified on the development Mac (GNU Make 3.81, Go 1.26.4):

- `make help`, `make fmt-check`, `make vet`, `make check-boundary`,
  `make test`, `make test-race`, `make build` (a 22 MB static binary in
  `bin/sluice`, `VERSION` resolved to the short SHA by `git describe`);
- `make -n` on every target that needs Docker, kind or kubectl, to confirm the
  exact commands;
- `bash -n hack/*.sh`;
- `make lint` without golangci-lint installed prints the install hint and
  fails, as intended;
- `hack/check-boundary.sh` on a scratch copy of the module with a deliberate
  `k8s.io` import in `internal/router` and a Prometheus import in
  `internal/model` reports both violations and exits 1.

Not verifiable today and therefore untested:

- `make docker-build` and the CI `docker` job: the Docker daemon (colima) is
  stopped. The Dockerfile was reviewed by hand only.
- `make kind-up`, `make kind-load`, `make deploy`, `make undeploy`, `make demo`,
  `make kind-down`: kind and kubectl are not installed, and `deploy/k8s/` plus
  the demo manifests do not exist yet.
- `make lint` with golangci-lint present, and the `.golangci.yml` v2 schema
  against a real binary.
- The workflow itself has not run on GitHub yet.

## 7. Validating slice 6 later

Slice 6 is the first slice where sluice runs in a cluster. The orchestrator
should validate in this order, each step being cheap to repeat:

1. Tools: `brew install kind kubernetes-cli golangci-lint`, then
   `colima start ...` (section 5). Check `kind version` (0.33.0),
   `kubectl version --client` (1.37.0), `golangci-lint version` (2.13.2),
   `docker info` succeeds.
2. Lint: `make lint` runs with no config error. If golangci-lint rejects a
   key in `.golangci.yml`, fix the key; the intent is standard linters plus
   `misspell` and `unconvert`, and `gofmt` plus `goimports` with
   `github.com/RamaHerbin/sluice` as the local prefix.
3. Image: `make docker-build`; expect a multi-stage build ending in
   `gcr.io/distroless/static-debian13:nonroot`. `docker run --rm sluice:dev -h`
   must print the flag help and exit, which proves the binary is static and
   runnable as non-root.
4. Cluster: `make kind-up` twice; the second run must say the cluster exists
   and still wait for Ready. `make kind-load` must report the image loaded
   into node `sluice-control-plane`.
5. Deployment: `make deploy` against the `deploy/k8s/` manifests written in
   slice 6. `kubectl -n sluice-system rollout status deployment/sluice` must
   succeed; the Service must be `NodePort` 30080 on the proxy port, and the
   pod must run as non-root (`imagePullPolicy: IfNotPresent`, image
   `sluice:dev`).
6. Demo: apply the echo manifests, run `make demo`. Every `ok:` line must
   appear, `/a` and `/b` bodies must come from different pods (the echo JSON
   shows the hostname), the unknown host must be 404, and the `sluice_*`
   metrics must include `sluice_http_requests_total` with route labels only.
7. Teardown: `make undeploy`, `make kind-down`. Re-run `make kind-up` to
   confirm the flow is repeatable.
8. CI: push a branch and open a pull request; all three jobs must be green.
   The `docker` job is the first time the Dockerfile runs anywhere if step 3
   was skipped.

## 8. Follow-ups for the orchestrator

- `cmd/sluice/main.go` does not declare a `version` variable yet. The
  `-X main.version=$(VERSION)` linker flag is silently ignored until one
  exists (verified: `make build` succeeds). Adding `var version = "dev"` and
  logging it at startup makes the flag useful; no Makefile change needed.
- `deploy/k8s/` is empty. `make deploy` and `make undeploy` will fail with
  "no objects passed to apply" until the manifests land.
- The demo manifests (`examples/echo/` presumably) do not exist. `hack/demo.sh`
  stops with a clear message when namespace `demo` is missing.
- `hack/check-boundary.sh` checks non-test code only (section 4).
- `.dockerignore` excludes `**/*.md`, `docs`, `deploy`, `examples`, `hack`,
  `bin`, `.git` and the tooling files, so the build context is go.mod, go.sum
  and the Go sources. Add new non-Go directories there as they appear.
