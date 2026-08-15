# Tasks: Per-Model Upstream Backend Routing

Generated from: docs/per-model-backend-routing/ on 2026-08-15

## Status legend
- [ ] pending
- [>] in progress
- [x] done
- [!] blocked

## Tasks

### Task 1: Add route configuration struct and indexed env var parsing
**Status**: [x] done
**Files**: internal/config/config.go, internal/config/config_test.go
**Test**: Parse a three-route config, verify Config.Routes contains three entries with correct model lists, backend families, URLs, and lane scoping. Test empty/unset at index 1 disables routing entirely. Verify parse fails with malformed URL, empty model name with non-empty target, duplicate model across routes, invalid lane value, index gaps in the BROKER_ROUTE_<N>_* sequence, or two instances sharing the same resolved _UNIT_NAME or URL. Verify CR/LF rejection in API key.
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 2: Refactor backend.New() to extract newInstance() helper
**Status**: [x] done
**Files**: internal/backend/backend.go, internal/backend/backend_test.go, internal/backend/ollama_backend_test.go, internal/backend/openai_backend_test.go, internal/backend/parity_test.go
**Test**: backend.New(cfg) still works exactly as before. newInstance produces byte-for-byte identical proxy construction (parity_test.go's TestOllamaBackendProxyStructuralParity, pointer/reflect identity, must pass unmodified). backend_test.go, ollama_backend_test.go, openai_backend_test.go, parity_test.go all pass unmodified.
**Depends on**: Task 1
**Parallelizable**: no
**Notes**:

### Task 3a: Router struct definition and model-peeking dispatch
**Status**: [x] done
**Files**: internal/backend/router.go (new)
**Test**: Happy-path dispatch (routed model to alt backend, unrouted to default). Model-peek with large vision payload never fully buffers the body; forwarded model field byte-identical (FR-6). Proxy() (lane="") falls through to default for a lane-scoped route — explicit test, not untested dead code.
**Depends on**: Task 1
**Parallelizable**: yes
**Notes**:

### Task 3b: Router.Generate(), Reachable(), and Unloader() methods
**Status**: [x] done
**Files**: internal/backend/router.go
**Test**: Generate dispatches correctly by model. Reachable checks only default backend. Unloader() returns a true nil (not typed-nil).
**Depends on**: Task 3a
**Parallelizable**: no
**Notes**:

### Task 3c: Lane scoping, RoutingSummary, and error passthrough
**Status**: [x] done
**Files**: internal/backend/router.go
**Test**: Lane-scoped rule only fires on its lane. RoutingSummary() returns correct mapping. A routed backend's error response passes through unmodified.
**Depends on**: Task 3a
**Parallelizable**: no
**Notes**:

### Task 4: Generalize yield.Controller from scalar to slice of unloaders (highest risk)
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: Existing yield_test.go and parity_test.go pass unmodified (zero-routing form, using New/NewWithConfirm). internal/backend/openai_backend_test.go's existing typed-nil-Unloader regression test (~line 227) still passes unmodified.
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 5: Add multi-instance flap-ordering test to yield.Controller
**Status**: [x] done
**Files**: internal/yield/yield_test.go
**Test**: 2-instance flap test: unload/reload both fire; a permanently-erroring instance A does not block/reorder instance B's actions (FR-13 independence); per-instance order preserved via recorded call sequences.
**Depends on**: Task 4
**Parallelizable**: no
**Notes**:

### Task 6a: Construct default backend and route backends via index-aligned builder
**Status**: [x] done
**Files**: cmd/broker/main.go
**Test**: Zero routes → router construction skipped entirely (defaultBackend.Proxy() wired directly). N routes → router built, backends index-aligned with cfg.Routes.
**Depends on**: Task 1, Task 2, Task 3a, Task 5
**Parallelizable**: no
**Notes**: Implemented via buildRoutes() helper + the if/else block around ctrl/activeBackend/router construction in main().

### Task 6b: Build combined unloaders slice and initialize multi-instance yield
**Status**: [x] done
**Files**: cmd/broker/main.go
**Test**: Zero routes → unloaders slice has exactly one element. N routes → N+1 elements (typed-nil filtered).
**Depends on**: Task 6a, Task 5
**Parallelizable**: no
**Notes**: buildRoutes() returns unloaders/labels alongside backends; NewWithConfirmMulti wired for the routed branch, NewWithConfirm unchanged for the zero-route branch.

### Task 6c: Swap Interactive/Batch Gate call sites to use Router
**Status**: [x] done
**Files**: cmd/broker/main.go
**Test**: Zero routes → exact pre-feature code path, body never buffered. Route configured → Interactive/Batch/Job paths all route correctly. Note: queue.Class is int8, not string — must call .String() at both ProxyForLane call sites.
**Depends on**: Task 6a, Task 6b
**Parallelizable**: no
**Notes**: interactiveProxy/batchProxy conditional block calls router.ProxyForLane(queue.Interactive.String()) / .String() on Batch; falls back to activeBackend.Proxy() with zero routes.

### Task 6d: Add startup logging and per-instance labels
**Status**: [x] done
**Files**: cmd/broker/main.go, internal/yield/yield.go
**Test**: Startup log has routing summary with no API key/credential. Unload/reload WARN logs during a multi-instance flap include distinguishable instance identifiers.
**Depends on**: Task 6c
**Parallelizable**: no
**Notes**: yield.go's doUnload/doReload log per-instance labels (default "instance[i]"); RoutingSummary() feeds /status, not raw startup log — matches Task 7's routingStatus wiring.

### Task 6e: Update acceptance test wiring to match production
**Status**: [x] done
**Files**: internal/acceptance_test.go
**Test**: Full acceptance suite passes with zero routes (identical to before) and with one route configured (new coverage). Also add AC-12 coverage: during active contention with ≥2 configured instances, no request is admitted to ANY instance (Router + multi-instance Controller wired together, not yield.Controller in isolation).
**Depends on**: Task 6a, Task 6b, Task 6c
**Parallelizable**: no
**Notes**: newRig mirrors main.go's routing branch exactly; new test TestAcceptance_RoutingContentionBlocksAllConfiguredInstances covers the contention case end-to-end, passing under -race.

### Task 7: Add routing status to admin /status endpoint
**Status**: [x] done
**Files**: internal/admin/admin.go, cmd/broker/main.go
**Test**: /status with no routing → "routing" key absent. /status with routing configured → "routing" present, matches loaded config, no API keys.
**Depends on**: Task 3c, Task 6a
**Parallelizable**: no
**Notes**: Mux() gained routingStatus func() any as 9th param; nil (zero routes) omits the key entirely, preserving pre-feature /status shape byte-for-byte.

### Task 8: Document feature in README and create ADR-0015
**Status**: [x] done
**Files**: README.md, docs/adr/0015-per-model-backend-routing.md (new)
**Test**: README entries consistent with implemented env vars. ADR-0015 describes the architecture, links ADR-0001/0003/0004/0014, and records the Router.Unloader() nil-stub trade-off explicitly. Grep for BROKER_ROUTE_ confirms every env var has a README row.
**Depends on**: Task 1, Task 2, Task 3a, Task 3b, Task 3c, Task 4, Task 5, Task 6a, Task 6b, Task 6c, Task 6d, Task 6e, Task 7
**Parallelizable**: no
**Notes**: All 6 BROKER_ROUTE_<N>_* vars (MODELS/BACKEND/URL/API_KEY/UNIT_NAME/LANE) confirmed present as README rows via grep diff. ADR-0015 written in the established ADR-0014 style, links ADR-0001/0003/0004/0008/0014, records the Router.Unloader() nil-stub trade-off explicitly per plan.md's requirement.

### Task 9: Verify backward compatibility and run full test suite
**Status**: [x] done
**Files**: all test files (*_test.go)
**Test**: go test ./... passes. Zero-routing broker startup byte-for-byte identical to pre-feature. One-route integration test: routed and non-routed models on both lanes reach the correct backend.
**Depends on**: Task 1, Task 2, Task 3a, Task 3b, Task 3c, Task 4, Task 5, Task 6a, Task 6b, Task 6c, Task 6d, Task 6e, Task 7, Task 8
**Parallelizable**: no
**Notes**: Full suite green (go build/vet/gofmt/test -race, all packages ok). New TestAcceptance_OneRouteBothLanesReachCorrectBackend covers routed+unrouted models on both lanes. Live binary spot-check performed against two real mock HTTP upstreams: zero-route startup log/status matched pre-feature shape exactly (no "routing" key, no routing-summary log line); one-route config correctly rejected a same-URL route/default collision, then correctly routed "routed-model" to its configured upstream and everything else to the default, confirmed via /status's "routing" key and the "routing configured" startup log line. Removed the .bak files spec-challenge left behind.

## Blocked / open
(none yet)
