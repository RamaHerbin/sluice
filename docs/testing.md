# Test scenarios

The fourteen scenarios every MVP slice is measured against, with the package that owns the test and the test name once it exists. Filled after wave 2 (2026-09-03); the orchestrator keeps it current. Run everything with `make test-race`.

| # | Scenario | Owner package | Test name |
|---|---|---|---|
| 1 | A request reaches the right service according to its hostname | `internal/router` |  `TestScenario01HostSelectsService` (router), `TestHostRoutingThroughHandler` (proxy) |
| 2 | The most specific path wins | `internal/router` |  `TestScenario02MostSpecificPathWins` (router) |
| 3 | Two endpoints receive requests in round-robin | `internal/upstream`, `internal/proxy` |  `TestScenario03RoundRobinTwoAddresses` (upstream), `TestScenario03RoundRobinAlternates` (proxy) |
| 4 | A removed endpoint is no longer used | `internal/controller` (translation), `internal/proxy` (after swap) |  `TestScenario04RemovedEndpointAbsent` (controller), `TestScenario04RemovedEndpointNoLongerUsed` (proxy) |
| 5 | A new route becomes available without restart | `internal/controller` (fake clientset) |  `TestScenario05NewIngressAppliesRoute` (controller, fake clientset) |
| 6 | An invalid configuration does not replace the last valid one | `internal/router` (`Store.Apply`) |  `TestScenario06InvalidConfigKeepsLastValid` (router) |
| 7 | An in-flight request completes during an update | `internal/proxy` |  `TestScenario07SwapUnderLoad`, `TestSwapMidFlightKeepsOldBackend` (proxy) |
| 8 | No endpoint produces a 503 | `internal/proxy` |  `TestScenario08NoReadyEndpointIs503` (proxy) |
| 9 | An unreachable upstream produces a 502 | `internal/proxy` |  `TestScenario09UnreachableUpstreamIs502` (proxy) |
| 10 | Forwarded headers are correct (X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, Host preserved) | `internal/proxy` |  `TestScenario10ForwardedHeaders` (proxy) |
| 11 | Allowed placeholders are evaluated | `internal/placeholders`, `internal/proxy` |  `TestScenario11AllowedPlaceholdersEvaluated` (placeholders), `TestScenario11HeaderTemplatesEvaluated` (proxy) |
| 12 | An unknown placeholder causes a validation error | `internal/placeholders` (Parse), `internal/router` (Compile), `internal/controller` (Ingress skipped) |  `TestScenario12UnknownPlaceholderIsParseError` (placeholders), `TestScenario12UnknownPlaceholderRejected` (router), `TestScenario12UnknownPlaceholderSkipsOnlyThatIngress` (controller) |
| 13 | Deleting an Ingress removes its route | `internal/controller` (fake clientset) |  `TestScenario13DeleteIngressRemovesRoute` (controller, fake clientset) |
| 14 | The controller waits for the initial sync before being ready | `internal/controller`, `internal/observability` |  `TestScenario14SyncedAfterFirstApply` (controller), `TestScenario14ReadyzFollowsReadinessFlag` (observability) |

Slice 1 end to end: `TestStaticRouteEndToEnd` in `cmd/sluice` (static `-route` flag, real reverse proxy, 404 on unknown host).

Additional checks that are not numbered scenarios: race hammer with continuous swaps (`internal/proxy`), upstream timeout gives 504 (`internal/proxy`), client cancel writes nothing and records 499 (`internal/proxy`), header-rule limits and deny-list (`internal/placeholders`, `internal/model`), `make check-boundary`.
