# Steps: Per-Model Upstream Backend Routing

## Prerequisites
An alternate backend instance (e.g. a vLLM process) must already be running and reachable at its configured URL before routing is enabled for it. This feature does not provision, health-check-at-startup, or manage that instance's lifecycle beyond the existing systemd stop/start-on-yield mechanism (ADR-0014) when BROKER_ROUTE_<N>_UNIT_NAME is set.

## Implementation steps

### Step 1: Add route configuration struct and indexed env var parsing
**What**: Define `RouteBackend` struct (including `Lane` field: optional 'interactive' or 'batch'; unset means both lanes), add `Config.Routes []RouteBackend` field, and implement indexed env var parsing (`BROKER_ROUTE_<N>_MODELS`, `BROKER_ROUTE_<N>_BACKEND`, `BROKER_ROUTE_<N>_URL`, `BROKER_ROUTE_<N>_LANE`, etc.) in `config.Load()`.
**Files**: `internal/config/config.go`, `internal/config/config_test.go`.
**Test**: Parse a three-route config (`BROKER_ROUTE_1_MODELS=qwen,llama BROKER_ROUTE_1_BACKEND=openai BROKER_ROUTE_1_URL=... BROKER_ROUTE_1_LANE=interactive`, etc.), verify `Config.Routes` contains three entries with correct model lists, backend families, URLs, and lane scoping. Test empty/unset at index 1 disables routing entirely. Verify parse fails with malformed URL, empty model name with non-empty target, duplicate model across routes, invalid lane value (must be empty, 'interactive', or 'batch'), index gaps in the BROKER_ROUTE_<N>_* sequence (e.g. index 3 configured while index 2 unset), or two instances (default + any route) sharing the same resolved _UNIT_NAME or URL. Verify CR/LF rejection in API key.
**Depends on**: None.
**Parallelizable**: Yes.

### Step 2: Refactor `backend.New()` to extract `newInstance()` helper
**What**: Extract the common logic for constructing an Ollama or OpenAI backend instance into a new unexported `newInstance(family string, u *url.URL, apiKey, unitName string) (Backend, error)` helper; update `backend.New(cfg)` to call `newInstance` for the default backend, keeping `New`'s signature unchanged.
**Files**: `internal/backend/backend.go`, `internal/backend/backend_test.go`, `internal/backend/ollama_backend_test.go`, `internal/backend/openai_backend_test.go`, `internal/backend/parity_test.go`.
**Test**: Verify `backend.New(cfg)` still works exactly as before (existing `backend_test.go` tests pass unmodified). Create a default-backend instance and a second instance via `newInstance` with different URL and verify the second instance's URL, backend family, and non-nil Proxy handler match newInstance's input exactly, and that newOllamaBackend/newOpenAIBackend's existing field-level behavior is unchanged (byte-for-byte identical proxy construction — see internal/backend/parity_test.go's TestOllamaBackendProxyStructuralParity, which asserts pointer/reflect identity and must still pass unmodified). Also verify internal/backend/backend_test.go, internal/backend/ollama_backend_test.go, internal/backend/openai_backend_test.go, and internal/backend/parity_test.go all pass unmodified — these type-assert on concrete backend types (be.(*ollamaBackend)/be.(*openaiBackend)) and are the actual regression detectors for this refactor.
**Depends on**: Step 1.
**Parallelizable**: No.

### Step 3a: Router struct definition and model-peeking dispatch
**What**: Create `internal/backend/router.go` with a `Router` struct that holds a default `Backend` and a map of routed models to `Backend` instances. Implement `ProxyForLane(lane string)` with bounded/size-capped model-peeking (read at most 64KB or use a streaming json.Decoder to extract just the "model" field, restoring the body for forwarding either way) that dispatches to the routed backend if model is configured for that lane, else falls back to default. Implement `Proxy()` as an alias for `ProxyForLane("")`, documented as a Backend-interface-satisfying stub not used by production wiring (which always calls `ProxyForLane` with a real lane) — with no lane context, it correctly falls through to the default backend for any lane-scoped route.
**Files**: `internal/backend/router.go` (new).
**Test**: Happy-path dispatch: route model `qwen` to backend B1, send a request for `qwen` and verify it reaches B1, send a request for `bert` and verify it reaches the default backend. Model-peek with large (multi-MB) vision payload: verify request body is never fully buffered, forwarded model field is byte-identical to what was received (FR-6 coverage). Test Proxy() alone (no lane) falls through to default for any lane-scoped route (verify as untested dead code with an explicit test marking its intentional stub status).
**Depends on**: Step 1 (needs Config.Routes' shape for the constructor signature).
**Parallelizable**: Yes (with Step 2; does not depend on Step 2's newInstance directly in the Router itself).

### Step 3b: Router.Generate(), Reachable(), and Unloader() methods
**What**: Implement `Generate(ctx, model, ...)` to dispatch on the `model` argument directly (no body peeking). Implement `Reachable(ctx)` to check only the default backend (per-route liveness lives in RoutingSummary, not here). Implement `Unloader()` to return `nil`, documented as intentionally wired outside this interface method in main.go.
**Files**: `internal/backend/router.go`.
**Test**: Generate dispatches correctly by model (routed model to alt backend, unrouted model to default). Reachable checks only default backend, ignoring per-route liveness. Unloader() returns a true nil (not a typed-nil).
**Depends on**: Step 3a.
**Parallelizable**: No.

### Step 3c: Lane scoping, RoutingSummary, and error passthrough
**What**: Add lane-scoped routing: rules in Config.Routes with a _LANE field only apply to Interactive or Batch requests; unset lane means both. Add `RoutingSummary()` method returning the routing table for admin output. Document error passthrough: a routed backend's error response (4xx, 5xx, invalid JSON) reaches the Consumer unchanged.
**Files**: `internal/backend/router.go`.
**Test**: Lane-scoped rule only fires on its lane (send Batch request for a model routed only on Interactive, verify it reaches default). RoutingSummary() returns correct mapping. Error passthrough: route a model to a backend that rejects it, verify the backend's error response passes through unmodified.
**Depends on**: Step 3a.
**Parallelizable**: No.

### Step 4: Generalize `yield.Controller` from scalar to slice of unloaders (highest risk)
**What**: Change `internal/yield/yield.go`'s internal state from `unloader Unloader` + `actionDone chan struct{}` to `unloaders []Unloader` (non-nil only, filtered at construction) + `actionDone []chan struct{}` (one per unloader). Add new exported constructors `NewMulti(det Detector, unloaders []Unloader, interval time.Duration) *Controller` and `NewWithConfirmMulti(det Detector, unloaders []Unloader, interval time.Duration, confirmPolls int) *Controller`. Implement `applyLocked` to loop `for i := range c.unloaders { go c.doUnload/doReload(i, c.startAction(i)) }` so each instance gets its own independent ADR-0014 ordering chain. Keep existing `New` and `NewWithConfirm` (scalar `Unloader` argument) unchanged, implemented as one-element wrappers around the multi form. Apply the direct-interface-nil check from ADR-0014 per-element during construction to filter typed nils.
**Files**: `internal/yield/yield.go`.
**Test**: Verify existing `yield_test.go` and `internal/backend/parity_test.go` tests pass unmodified (zero-routing form, using `New`/`NewWithConfirm`). Do NOT write multi-instance tests yet; that is step 5. Also verify internal/backend/openai_backend_test.go's existing typed-nil-Unloader regression test (around line 227) still passes unmodified — it depends on applyLocked's nil-check behavior being preserved exactly under the new slice-filtering-at-construction logic.
**Depends on**: None — internal/yield/yield.go has zero compile-time dependency on internal/config; this generalization is purely internal to the yield package.
**Parallelizable**: Yes (fully independent of all other steps).

### Step 5: Add multi-instance flap-ordering test to `yield.Controller`
**What**: Add a table-driven test in `internal/yield/yield_test.go` that mirrors the existing ADR-0014 flap-ordering test but with 2+ simultaneously configured `Unloader` instances. Assert that a fast unload→reload→unload sequence does not race actions out of order for any individual instance, and that a permanently-erroring `Unload`/`Reload` on instance A does not delay or reorder instance B's actions (FR-13 independence).
**Files**: `internal/yield/yield_test.go`.
**Test**: Run the flap test with 2 instances: simulate a contention spike (unload), verify both instances' `doUnload` methods are called. Simulate contention clear (reload), verify both instances' `doReload` methods are called. Run a "instance A jams" scenario: have instance A's `Unload` return an error, verify instance B's `Unload` still completes and is not blocked by A's error. Verify action order is preserved per-instance using recorded call sequences.
**Depends on**: Step 4.
**Parallelizable**: No.

### Step 6a: Construct default backend and route backends via index-aligned builder
**What**: Build a single index-aligned builder function that constructs defaultBackend via `backend.New(cfg)` and one Backend per `cfg.Routes[i]` via the refactored `newInstance` helper, returning both the constructed `[]Backend` and their corresponding `[]yield.Unloader` together (index-aligned, with typed nils already filtered). Call `router := backend.NewRouter(defaultBackend, cfg.Routes, routeBackends)`.
**Files**: `cmd/broker/main.go`.
**Test**: With zero routes, router construction is skipped entirely (defaultBackend.Proxy() wired directly — the zero-route fast path). With N routes, router is built and its backends are correctly index-aligned with cfg.Routes.
**Depends on**: Steps 1, 2, 3a. Step 5 (flap-ordering test must pass before multi-instance yield is wired in production).
**Parallelizable**: No.

### Step 6b: Build combined unloaders slice and initialize multi-instance yield
**What**: Collect unloaders from Step 6a's output: `unloaders := append([]yield.Unloader{defaultBackend.Unloader()}, routeUnloaders...)` (preserving order, filtering nils via construction). Call `yield.NewWithConfirmMulti(detector, unloaders, cfg.DetectInterval, cfg.YieldConfirmPolls)` unconditionally (same code path whether 0 or N routes configured).
**Files**: `cmd/broker/main.go`.
**Test**: With zero routes, unloaders slice has exactly one element (defaultBackend.Unloader()). With N routes, it has N+1 (filtered for non-nil per ADR-0014's typed-nil discipline).
**Depends on**: Step 6a, Step 5 (flap-ordering test must pass first).
**Parallelizable**: No.

### Step 6c: Swap Interactive/Batch Gate call sites to use Router
**What**: Swap the two Interactive/Batch Gate call sites — when `len(cfg.Routes)==0`, wire `defaultBackend.Proxy()` directly (unchanged from today); when routes exist, wire `router.ProxyForLane(queue.Interactive.String())` and `router.ProxyForLane(queue.Batch.String())` (note the `.String()` conversion — `queue.Class` is an int8 type, not a string, so this conversion is required). Pass `router` (or `defaultBackend` when no routes) into `job.NewWorker` and `healthCheck`.
**Files**: `cmd/broker/main.go`.
**Test**: With zero routes, requests flow through the exact pre-feature code path (verify via test that body is never buffered). With a route configured, Interactive/Batch/Job paths all route correctly.
**Depends on**: Step 6a, Step 6b.
**Parallelizable**: No.

### Step 6d: Add startup logging and per-instance labels
**What**: Add a startup log line summarizing the resolved routing configuration (credential-free) at info level (FR-16/AC-11). Ensure per-instance unload/reload log lines (from Step 4/5's yield.go changes) include an instance label (unit name, URL, or route index) so an operator can tell which instance failed during an incident.
**Files**: `cmd/broker/main.go`, `internal/yield/yield.go`.
**Test**: Startup log contains routing summary with no API key or credential value. Unload/reload WARN logs during a multi-instance flap include distinguishable instance identifiers.
**Depends on**: Step 6c.
**Parallelizable**: No.

### Step 6e: Update acceptance test wiring to match production
**What**: Update `internal/acceptance_test.go`'s newRig helper to build Gates via Router (or defaultBackend.Proxy() directly, matching the zero-route fast path) exactly as the new main.go does, so the acceptance suite keeps testing the real production code path. The test's own doc comment claims to mirror real production wiring, which becomes false once Step 6 lands.
**Files**: `internal/acceptance_test.go`.
**Test**: Run the full acceptance suite (`go test ./internal/... -run TestAcceptance` or equivalent) with zero routes configured (must pass identically to before) and with one route configured (new coverage).
**Depends on**: Step 6a, Step 6b, Step 6c.
**Parallelizable**: No.
**AC-12 Coverage**: During active gaming/Plex contention with ≥2 configured backend instances, no request is admitted to ANY instance — add an integration test combining the admission gate, Router, and the multi-instance yield.Controller together (Step 5 tests yield.Controller in isolation only).

### Step 7: Add routing status to admin `/status` endpoint
**What**: Update `internal/admin/admin.go`'s `Mux` function to accept an optional `routingStatus func() any` parameter. Include the `"routing"` key in `/status`'s JSON response map only when `routingStatus` is non-nil, mirroring the existing conditional pattern for `tdarrStatus` and `jobStatus`. In `main.go`, pass a closure that calls `router.RoutingSummary()` (or `nil` if no routes are configured). Update the response struct tags/documentation to reflect the new optional key.
**Files**: `internal/admin/admin.go`, `cmd/broker/main.go` (update the `admin.Mux` call).
**Test**: Query `/status` with no routing configured; verify `"routing"` key is absent. Query `/status` with routing configured; verify `"routing"` is present and matches the loaded configuration (models, backend URLs, lanes, no API keys).
**Depends on**: Step 3c (for RoutingSummary()) AND Step 6a (the `router` variable Step 7's main.go closure references doesn't exist until Step 6a constructs it).
**Parallelizable**: No.

### Step 8: Document feature in README and create ADR-0015
**What**: Add the `BROKER_ROUTE_<N>_*` env vars to the configuration table in `README.md`, matching the existing `UPSTREAM_*` row style, with a note that routing is optional and defaults to disabled. Create `docs/adr/0015-per-model-backend-routing.md` capturing the design decision to use a routing table (not a second port), the slice generalization in `yield.Controller`, and the per-instance ordering guarantee (ADR-0014 extension). Note the risk areas: yield.Controller multi-instance independence, model-peek buffering verification, startup validation atomicity.
**Files**: `README.md`, `docs/adr/0015-per-model-backend-routing.md` (new).
**Test**: Verify README entries are consistent with implemented env var names and defaults. Verify ADR-0015 correctly describes the architecture and links to ADR-0001/0003/0004/0014. Grep the codebase for BROKER_ROUTE_ and confirm every distinct env var name has a corresponding row in README.md's config table.
**Depends on**: Steps 1-7.
**Parallelizable**: No.



### Step 9: Verify backward compatibility and run full test suite
**What**: Run the full test suite (`go test ./internal/... ./cmd/...`) with zero routing configured; verify all existing tests pass unmodified, including `internal/backend/parity_test.go` (AC-13). Spot-check a live broker startup with no routing (default behavior must be byte-for-byte identical to pre-feature). Run a full integration test: start broker with one route, send requests to both routed and non-routed models on both lanes, verify correct backend receives each.
**Files**: All test files (`*_test.go`).
**Test**: `go test ./...` passes. Launch broker with `UPSTREAM_BACKEND=ollama OLLAMA_URL=http://localhost:11434`, no routing; verify `/healthz` returns 200 and `/status` has no `"routing"` key. Launch broker with `BROKER_ROUTE_1_MODELS=qwen BROKER_ROUTE_1_BACKEND=openai BROKER_ROUTE_1_URL=http://localhost:8000`, verify `/status` includes `"routing"` array with one entry.
**Depends on**: Steps 1-8, 3a, 3b, 3c, 6a, 6b, 6c, 6d, 6e.
**Parallelizable**: No.

## Rollback plan

**Steps 1-3, 8**: All changes are additive to configuration and code; rollback via `git reset --hard HEAD` before the feature branch was created.

**Steps 6a-6e** (main.go wiring): These steps flip the live Proxy call sites for the Interactive/Batch gates — a bug here affects ALL traffic, not just routed models. Rollback: reverting `cmd/broker/main.go` alone (without reverting Steps 1-3's additive config/backend/router code) restores `defaultBackend.Proxy()` directly and is a safe, isolated rollback — the newly-added Router/config code stays inert (unused) if main.go reverts to not calling it.

**Step 4** (yield.Controller generalization): Highest-risk change. If flap-ordering tests (step 5) reveal a race condition, or if multi-instance integration (steps 6a-6e) crashes:
1. Revert `internal/yield/yield.go` to the pre-feature version.
2. Keep the refactored `backend.New` (step 2) and `Router` (step 3) — they don't depend on multi-instance yield.
3. Modify `main.go` to construct only one `Controller` instance (revert to `yield.NewWithConfirm(detector, defaultBackend.Unloader(), ...)`) and ignore `cfg.Routes` for yield purposes.
4. Routing will still work for request dispatch (via Router) but will not invoke `Unload`/`Reload` on routed backends — acceptable as a degraded mode pending a yield.Controller fix.

**Steps 5, 7, 9**: Test rollback is implicit in step 4.
