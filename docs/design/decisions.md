# Decisions log

Cross-cutting decisions taken by the orchestrator. Newest first. Each entry says what was decided, why, and what it changed. ADRs in `docs/adr/` cover the foundational choices; this file covers the smaller ones that came out of the design and implementation waves.

## 2026-09-03: integration after wave 2

All seven verifier verdicts were ok with no fix round. Items folded in by the orchestrator:

| Decision | Why | Changed |
|---|---|---|
| `ValidateHeaderRules` rejects whitespace-only values | `ParseHeaderRules` trims, so hand-built snapshots were held to a weaker rule | `internal/model/model.go` |
| `Hash` documents that `Created` is part of the hash | deleting and recreating an identical Ingress rebuilds once; intended, not a bug | `internal/model/model.go` |
| `Match` runs on `r.URL.Path` (decoded); logs and `{http.request.path}` use `EscapedPath()` | Ingress paths are written decoded; encoded CR LF must never reach a log line or header | comment in `internal/proxy/proxy.go` |
| `Table.Len` counts shadowed routes | kept for logging; `sluice_routes` therefore means compiled routes | doc comment in `internal/router/router.go` |
| Race hammer floor stays at 50 swaps (thousands observed) | CI safety; the intent is continuous swapping, not a count | docs aligned |
| The EndpointSlice indexer stays registered although `Translate` regroups slices itself | contract fidelity; cheap; candidate for removal or use in a later slice | noted |
| A misspelled backend port name yields a 503 route without a warning | matches Ingress semantics (route exists, no endpoints); a warning kind for it is a possible follow-up | noted |
| Static `-route` flags removed at slice 4; the slice 1 end-to-end test builds its snapshot inline | ADR 0006 | `cmd/sluice` |
| client-go logs (klog) go to stderr unformatted next to slog output | acceptable for the MVP; routing klog into slog is a follow-up | noted |
| Slice 6 not run | kind, kubectl, golangci-lint are not installed and colima is stopped; installing tools and starting a VM change the machine and wait for the user | roadmap |

## 2026-09-03: sync point after wave 1

Inputs: `docs/design/dataplane.md` (prototype findings), `docs/design/placeholders.md`, `docs/design/devx.md`, ADRs 0001 to 0006, and the four verifier reports.

| Decision | Why | Changed |
|---|---|---|
| Add `placeholders.ErrTemplateTooLong` and `placeholders.MaxValueLength = 8192` | third parse failure class had no sentinel; the evaluated-length cap is a security limit tests must reference | `internal/placeholders/placeholders.go` |
| Header-rule annotation parsing lives in `model.ParseHeaderRules`; structural checks in `model.ValidateHeaderRules`; limits `MaxHeaderRules = 32`, `MaxHeaderNameLength = 256` | Translate and Compile both need the same checks; one function prevents drift | `internal/model/model.go` |
| `{http.request.host}` strips the port and keeps case; `{http.request.path}` is `URL.EscapedPath()`; header variables join repeated lines with `", "`; no escape syntax; empty annotation value rejected; Proxy-Connection added to the deny-list; X-Forwarded-* and X-Request-ID stay settable | placeholders design, verified against Go 1.26 (decoded paths can carry CR LF that the transport rejects) | documented in `placeholders.md`, to implement in wave 2 |
| `Cookie` is not special-cased when joined | HTTP/1.1 keeps repeated Cookie lines apart; edge case, documented | `placeholders.md` |
| Wildcard hosts in a map keyed by `*.suffix`; exact-path map first, then longest prefix | prototype proved the candidate key is unique and the rule equivalent to "longest, Exact wins ties" | `router.go` comments, `architecture.md` |
| `Match` normalization: strip port via `net.SplitHostPort`, strip one trailing dot, lowercase; IPv6 literals keep brackets; paths matched as received | deterministic and cheap; an Ingress host is never an IP | `router.go` comments |
| ErrorHandler order: `context.Canceled`, then `Timeout()`, then dial `*net.OpError`, then other. Cancelled-before-headers recorded as status 499 (`proxy.StatusClientClosedRequest`) | Go 1.26 header-timeout error satisfies `errors.Is(err, context.DeadlineExceeded)`; 499 keeps label cardinality tiny and is a known convention | `proxy.go` |
| Status wrapper ignores 1xx except 101; mid-body client abort tolerated as `http.ErrAbortHandler` panic | observed ReverseProxy behavior | `proxy.go` comments |
| Request context carries the inbound `*http.Request` for placeholders | `resp.Request` in `ModifyResponse` is the rewritten clone | `proxy.go` comments |
| `NewRoundRobin` copies its input | table immutability | `picker.go` |
| `observability.NewMetrics()` owns a private registry; `AdminMux(readiness, metrics)`; `main` no longer imports Prometheus | CLAUDE.md boundary; the verifier caught `main.go` importing `prometheus` directly | `observability.go`, `main.go`, `hack/check-boundary.sh` (direct-import check on `cmd/sluice`) |
| `make fmt` and `fmt-check` cover the whole repo | the CLAUDE.md gate is `gofmt -l .` | `Makefile`, `devx.md` |
| `check-boundary` allows `internal/controller/*` subpackages | future split of the controller package | `hack/check-boundary.sh` |
| `demo.sh` installs its trap before `mktemp` | leak on early failure | `hack/demo.sh` |
| Prototype `prototypes/dataplane` deleted | its purpose was to validate; lessons are in `dataplane.md`; keeping it would duplicate `internal/` names | removed |
| Race hammer in real tests keeps swapping for the duration of traffic (loop with a short sleep until workers finish; asserted floor 50 swaps, thousands observed), not a CPU spin and not 1000 back-to-back stores | prototype showed 1000 stores finish before the first request; a spin loop burns a CPU in the permanent suite | `architecture.md` section 7 |
| `main.version` variable added, logged at startup | `-X main.version` in Makefile and Dockerfile was a no-op | `cmd/sluice/main.go` |
| ADR 0007 records the two third-party dependencies | CLAUDE.md requires an ADR per module | `docs/adr/0007-dependencies.md` |
| ADR quotes fetched through summaries are treated as approximate | verifier could not confirm verbatim text | noted here, not in the ADRs |

Not changed, deliberately: CI cancels in-flight runs on `main` pushes (fine for a one-person project); numeric limits are constants without field data behind them and can move.
