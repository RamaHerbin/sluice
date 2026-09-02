# Test scenarios

The fourteen scenarios every MVP slice is measured against, with the package that owns the test and the test name once it exists. Implementation agents fill the last column; the orchestrator keeps it current. Run everything with `make test-race`.

| # | Scenario | Owner package | Test name |
|---|---|---|---|
| 1 | A request reaches the right service according to its hostname | `internal/router` | |
| 2 | The most specific path wins | `internal/router` | |
| 3 | Two endpoints receive requests in round-robin | `internal/upstream`, `internal/proxy` | |
| 4 | A removed endpoint is no longer used | `internal/controller` (translation), `internal/proxy` (after swap) | |
| 5 | A new route becomes available without restart | `internal/controller` (fake clientset) | |
| 6 | An invalid configuration does not replace the last valid one | `internal/router` (`Store.Apply`) | |
| 7 | An in-flight request completes during an update | `internal/proxy` | |
| 8 | No endpoint produces a 503 | `internal/proxy` | |
| 9 | An unreachable upstream produces a 502 | `internal/proxy` | |
| 10 | Forwarded headers are correct (X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, Host preserved) | `internal/proxy` | |
| 11 | Allowed placeholders are evaluated | `internal/placeholders`, `internal/proxy` | |
| 12 | An unknown placeholder causes a validation error | `internal/placeholders` (Parse), `internal/router` (Compile), `internal/controller` (Ingress skipped) | |
| 13 | Deleting an Ingress removes its route | `internal/controller` (fake clientset) | |
| 14 | The controller waits for the initial sync before being ready | `internal/controller`, `internal/observability` | |

Additional checks that are not numbered scenarios: race hammer with continuous swaps (`internal/proxy`), upstream timeout gives 504 (`internal/proxy`), client cancel writes nothing and records 499 (`internal/proxy`), header-rule limits and deny-list (`internal/placeholders`, `internal/model`), `make check-boundary`.
