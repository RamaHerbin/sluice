# sluice: repository rules

Read `docs/design/architecture.md` and `docs/design/kubernetes.md` before touching code. The plan of record is `docs/design/roadmap.md`.

## Language and style

- Code, comments, commit messages and technical docs are in English.
- Standard library first. New third-party modules need an ADR in `docs/adr/` and orchestrator approval.
- Small interfaces, explicit goroutine lifecycles, contextual errors (`fmt.Errorf("...: %w", err)`), no `panic` for configuration errors.
- Names, APIs and abstractions may change during the experiment; do not over-engineer.

## Hard boundaries

- Only `internal/controller` may import `k8s.io/*`. `make check-boundary` enforces it.
- Only `internal/observability` may import `github.com/prometheus/*`.
- HTTP handlers never read Kubernetes objects. They read the current `*router.Table` only.
- A `router.Table` is immutable after `Compile`, except the round-robin counters inside pickers.
- `Store.Apply` swaps only when `Compile` succeeds. An invalid snapshot never replaces the last valid table.

## Working in parallel (agents)

- Each agent owns whole directories listed in its brief. Never edit outside them.
- Never run `go get`, `go mod tidy`, or repo-wide `gofmt -w`. `go.mod` and `go.sum` belong to the orchestrator.
- Exported signatures in contract files are frozen. To change one, message the orchestrator first.
- Never commit. The orchestrator commits.
- Before reporting: `go build ./... && go vet ./... && gofmt -l . && go test -race -count=1 ./<your packages>` must be clean.
- End every report with "Decisions" and "Risks" sections.

## Quality gates (every slice)

`gofmt -l .` empty, `go vet ./...`, `go test ./...`, `go test -race -count=1 ./...`, `make check-boundary`, `golangci-lint run`.

## Security and privacy

Logs and metrics never contain header values, bodies, query strings or secrets. Metric labels are route key and status code only.
