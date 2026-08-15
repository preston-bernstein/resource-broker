# Steps: Idle-unload for openai-compatible (vLLM) backend instances

## Prerequisites

- Go 1.24+ installed (required for `sync/atomic` typed wrappers).
- Familiarity with the existing `yield.Controller`, `Unloader` interface, and `Router`/`Backend` architecture.
- Read-access to the deployed resource-broker codebase and the `.claude/worktrees/vllm-idle-unload/` branch.

## Implementation steps

### Step 1: Capture baseline `/status` output for the AC1 comparison
**What**: Before any code in this feature is touched, run the current (pre-feature) broker/admin test suite and capture the existing `/status` JSON shape into a committed fixture (e.g. `internal/admin/testdata/status_baseline.json`), captured with `UPSTREAM_IDLE_TIMEOUT` and every `BROKER_ROUTE_<N>_IDLE_TIMEOUT` unset. This exists purely so the later AC1 test (Step 16) has something concrete to diff against instead of an unanchored "compare to a baseline pre-feature run."
**Files**: `internal/admin/testdata/status_baseline.json` (new)
**Test**:
- Confirm the fixture file exists, is valid JSON, and contains no `"idle"` key.
- Confirm it was captured against the actual pre-feature `/status` handler (e.g. via the existing `internal/admin` test harness), not hand-written.
**Depends on**: none
**Parallelizable**: No — must run first, before Step 2 or Step 3 touch any code, so the fixture reflects true pre-feature behavior.

### Step 2: Add idle-duration config vars and validation to config.go
**What**: Parse `UPSTREAM_IDLE_TIMEOUT` and `BROKER_ROUTE_<N>_IDLE_TIMEOUT` environment variables with `getdur`, add them to the `Config` and `RouteBackend` structs. Read `UPSTREAM_UNIT_NAME` once, early, into a local var (matching the existing style already used for other cross-checked fields), and add the "Idle duration requires `_UNIT_NAME`" check against that local var immediately before the final `&Config{...}` literal is constructed — not scattered elsewhere in the function. Add the equivalent per-route check inside the existing per-route validation loop, alongside the existing `LANE`/`UNIT_NAME` checks. Also reject any negative `UPSTREAM_IDLE_TIMEOUT`/`BROKER_ROUTE_<N>_IDLE_TIMEOUT` value at `config.Load()` with a descriptive error; treat exactly zero identically to unset (FR1–FR6, AC9, AC10).
**Files**: `internal/config/config.go`
**Test**:
- `go test ./internal/config -v` passes.
- Parse a valid duration string (`"1h30m"`), confirm it's stored correctly.
- Set `UPSTREAM_IDLE_TIMEOUT=5m` with no `UPSTREAM_UNIT_NAME` and confirm `config.Load()` returns a descriptive error (AC9).
- Set `BROKER_ROUTE_1_IDLE_TIMEOUT=10m` with no `BROKER_ROUTE_1_UNIT_NAME` and confirm `config.Load()` returns a descriptive error (AC9).
- Set `UPSTREAM_IDLE_TIMEOUT=-5m` and confirm `config.Load()` returns a descriptive error (AC10); same for the per-route var.
- Set `UPSTREAM_IDLE_TIMEOUT=0` and confirm `Load()` succeeds and behaves as disabled.
- Leave both unset and confirm `Load()` succeeds (disabled state).
**Depends on**: none
**Parallelizable**: Yes — independent of Step 3 (the `internal/yield` track); the two tracks only converge at Step 12 (main.go wiring).

### Step 3: Add idle-tracking fields and ConfigureIdle to Controller
**What**: Add `idleTimeouts []time.Duration`, `lastDispatch []atomic.Int64`, `inFlight []atomic.Int32`, `idleUnloaded []atomic.Bool`, and `origIndex []int` fields to `Controller` in `internal/yield/yield.go`, built once inside `NewWithConfirmMulti` alongside the existing nil-unloader filter loop. `origIndex` maps ORIG-index (0 = default backend, 1 = route 0, 2 = route 1, ...) to POST-FILTER index; for any orig-index whose unloader was nil-filtered out (no `_UNIT_NAME`), store a sentinel (e.g. `-1`) instead of a real index. Add an exported `ConfigureIdle(idleTimeouts []time.Duration)` method that accepts an ORIG-index-ordered slice, maps each entry through `origIndex` with a bounds-check, skips/ignores entries whose `origIndex` is the sentinel, and stores the result into the post-filter `idleTimeouts` field (FR1, AC1).
**Files**: `internal/yield/yield.go`
**Test**:
- `go test ./internal/yield -v` passes (no behavior change yet; new fields are inert until later steps read them).
- Verify `origIndex` maps correctly: construct a Controller with 3 orig-positions where position 1 has a nil unloader; confirm `origIndex[0] == 0`, `origIndex[1] == sentinel`, `origIndex[2] == 1`.
- Call `ConfigureIdle` with a 3-entry orig-index-ordered slice (position 1 nil-filtered); verify the resulting post-filter `idleTimeouts` (length 2) holds position 0's and position 2's durations in the correct post-filter order, with no panic or index-out-of-bounds on the filtered slot.
- Call `ConfigureIdle` with an empty slice; verify no panic.
- Call `ConfigureIdle` with an orig-index value out of bounds for `origIndex`; verify it errors or is safely ignored, not a panic.
**Depends on**: none
**Parallelizable**: Yes — independent of Step 2; both converge only at Step 12.

### Step 4: Add typed unloadTrigger and thread it through doUnload/doReload
**What**: Define a typed `unloadTrigger` (e.g. `type unloadTrigger int` with `triggerYield`/`triggerIdle` consts, or a small `String()` method for log formatting) rather than a bare string. Add an `unloadTrigger` parameter to both `doUnload(i int, unloaderErr error, trigger unloadTrigger)` and `doReload(i int, unloaderErr error, trigger unloadTrigger)`, threading it into their existing log lines so output includes `trigger=yield` or `trigger=idle`. Update the two existing call sites in `applyLocked` to pass `triggerYield` (FR15, AC11).
**Files**: `internal/yield/yield.go`
**Test**:
- `go test ./internal/yield -v` passes.
- Trigger a Yield-sourced unload; grep the log output and confirm it contains `trigger=yield`.
- Confirm `unloadTrigger` is a distinct type (not a bare `string`) via a compile-time check (e.g. a call site passing a raw string literal fails to compile without an explicit conversion) — this only needs to be true at the type level, not exercised at runtime.
**Depends on**: Step 3
**Parallelizable**: No — same file as Step 3, and both `checkIdleLocked` (Step 5) and `TrackActivity`'s wake branch (Step 9) need this signature to exist first.

### Step 5: Add checkIdleLocked timer-check and wire into refresh()
**What**: Implement the new unexported `checkIdleLocked()` method on `Controller`: for each post-filter instance `i`, skip in O(1) with no atomic writes if `idleTimeouts[i] == 0` (disabled, FR17); otherwise compute `elapsed := now - lastDispatch[i]`, and when `elapsed >= idleTimeouts[i]`, CAS `idleUnloaded[i]` false→true, call `startAction(i)`, and spawn `doUnload(i, nil, triggerIdle)`. Add one line in `Controller.refresh()` calling `c.checkIdleLocked()` immediately after `c.applyLocked()`, still inside the `c.mu` lock, so Idle-checks always run strictly after Contention-checks each tick. Update `startAction`'s doc comment (it currently claims "only called from applyLocked") to also mention `checkIdleLocked` (FR7–FR10, AC2–AC4).
**Files**: `internal/yield/yield.go`
**Test**:
- `go test ./internal/yield -v` passes; existing tests for `refresh()` behavior unchanged.
- Mock time, advance past an idle duration with no other guard state set (defaults: not Yielding, no in-flight, not already idle-unloaded), and verify `doUnload` is called exactly once for that instance with `triggerIdle` (AC2).
- Dispatch a request (advance `lastDispatch[i]`) before the idle duration elapses; verify `doUnload` is never called at the original deadline and the timer effectively resets (AC3).
- Configure two instances with different idle durations; verify activity on one does not affect the other's timer or firing (AC4).
**Depends on**: Step 4
**Parallelizable**: No — extends the same method/file introduced by Step 4.

### Step 6: Add checkIdleLocked guards (Yield-active, in-flight, dedup)
**What**: Add explicit guard clauses to `checkIdleLocked()`, evaluated before the elapsed-time check from Step 5: skip instance `i` if `c.effective == true` (whole-Broker Yield already active, FR11/AC6); skip if `inFlight[i] > 0` (FR13/AC8); skip if `idleUnloaded[i]` is already `true` (dedup — avoid a duplicate `Unload` from the Idle path itself, FR11).
**Files**: `internal/yield/yield.go`
**Test**:
- Set `c.effective == true` (Yield active); verify `checkIdleLocked` skips that instance entirely and issues no `Unload` (FR11, AC6).
- Set `inFlight[i] > 0`; verify the instance is skipped even though its elapsed time exceeds the configured duration (FR13, AC8).
- Set `idleUnloaded[i] == true` before the tick runs; verify no duplicate `Unload` is issued for that instance.
**Depends on**: Step 5
**Parallelizable**: No — layers directly onto Step 5's method.

### Step 7: Update applyLocked yield-clear branch to reset idle state
**What**: In `Controller.applyLocked()`, inside the branch where `eff == false` (Yield is being cleared), add a line that resets `lastDispatch[i]` and `idleUnloaded[i]` for every instance, so a just-cleared instance is not immediately re-eligible for idle-unload before it has had a chance to serve anything (FR14).
**Files**: `internal/yield/yield.go`
**Test**:
- Idle-unload an instance (via Step 6's mechanism), then trigger a Yield-clear; verify both `lastDispatch[i]` and `idleUnloaded[i]` are reset and the instance is not immediately re-unloaded on the next `refresh()` tick.
**Depends on**: Step 6
**Parallelizable**: No — modifies the same critical section Step 5/6 read from.

### Step 8: Add TrackActivity plain-tracking decorator to Controller
**What**: Implement the request-bookkeeping half of exported `TrackActivity(origIdx int, next http.Handler) http.Handler` on `Controller` (FR7, FR8): on entry, increment `inFlight[idx]` and set `lastDispatch[idx] = time.Now()`; call `next.ServeHTTP`; on exit, decrement `inFlight[idx]` via a `defer`'d call (guaranteed to run even if the wrapped handler panics) and set `lastDispatch[idx] = time.Now()` again. This step does *not* yet include the idle-wake CAS/reload logic (Step 9).
**Files**: `internal/yield/yield.go`
**Test**:
- Dispatch a request through the decorated handler; verify `inFlight[idx]` increments during the call and decrements after, and `lastDispatch[idx]` moves.
- Make the wrapped `next` handler panic; verify (via `recover` in the test) that `inFlight[idx]` is still decremented afterward (proves the `defer`), even though the panic propagates.
**Depends on**: Step 3
**Parallelizable**: Yes relative to Steps 4–7 (pure bookkeeping, does not call `doUnload`/`doReload`/`startAction`); must land before Step 9.

### Step 9: Add TrackActivity wake-on-request branch
**What**: Extend `TrackActivity` (Step 8) with the idle-wake branch: on entry, after the bookkeeping in Step 8, if `idleUnloaded[idx]` is `true`, CAS it to `false`; on a successful CAS, call `startAction(idx)` and spawn `doReload(idx, nil, triggerIdle)` (fire-and-forget — the request itself proceeds once the reload completes via the existing cold-start behavior, not blocked on this goroutine directly). Update `startAction`'s doc comment again to name all three callers: `applyLocked`, `checkIdleLocked`, and `TrackActivity`'s wake branch (FR14, AC5).
**Files**: `internal/yield/yield.go`
**Test**:
- Dispatch a request to an instance with `idleUnloaded[idx] == true`; verify the CAS flips it to `false` and `doReload` is called with `triggerIdle` (AC5).
- Dispatch a request while the same instance is mid-idle-unload (unload's `actionDone` not yet closed); verify the reload's `startAction` call waits on the unload's predecessor-completion chain before running `Reload` — i.e. no start-before-stop ordering (AC7).
- Dispatch two concurrent requests to the same freshly-woken instance; verify only one `doReload` fires (the CAS loser sees `idleUnloaded` already `false` and takes no action).
**Depends on**: Step 8
**Parallelizable**: No — same method as Step 8.

### Step 10: Add IdleSummary exported method to Controller
**What**: Implement exported `IdleSummary() any` on `Controller` that returns a slice of JSON-serializable structs containing, for each instance with Idle configured, its label, `idle_timeout`, `idle_unloaded` bool, and `since_last_dispatch` duration (FR16, AC12). This is a pure reader over the fields introduced in Step 3; it does not require `checkIdleLocked` or `TrackActivity` to be implemented to be correct, only to be interesting to test end-to-end.
**Files**: `internal/yield/yield.go`
**Test**:
- Call `IdleSummary()` on a Controller with one instance that has Idle enabled (fields set directly in the test); verify the returned slice has exactly one entry with correct fields (AC12).
- Call `IdleSummary()` on a Controller with no Idle-configured instances; verify it returns nil or an empty slice (AC1 preservation).
- Verify the JSON serialization is correct by marshaling the result.
**Depends on**: Step 3
**Parallelizable**: Yes — reads fields only; independently parallelizable relative to Steps 4–9. Placed here for narrative grouping with the rest of the Controller API surface.

### Step 11: Create internal/backend/activity.go with WithActivityTracking
**What**: Create a new file `internal/backend/activity.go` implementing:
  - `activityBackend` struct: embeds `Backend`, holds a single pre-built `http.Handler` for `Proxy()`.
  - Exported `WithActivityTracking(b Backend, ctrl *yield.Controller, origIdx int) Backend`: calls `b.Proxy()` **once**, at wrap time (not per-request), wraps that single handler once via `ctrl.TrackActivity(origIdx, realHandler)`, and returns `&activityBackend{...}` — a **pointer**, not a value, so `Router.RoutingSummary()`'s use of `Backend` as a map key does not panic on an incomparable value type.
  - `(*activityBackend).Proxy()` returns the cached, pre-wrapped handler on every call (no rebuilding per request).
**Files**: `internal/backend/activity.go` (new)
**Test**:
- `go test ./internal/backend -v` passes.
- Dispatch a request through the decorated backend's `Proxy()`; verify the underlying `Controller`'s `inFlight`/`lastDispatch` for `origIdx` move (via a real or stub Controller).
- Call `Proxy()` twice on the same `*activityBackend`; verify both calls return the same handler instance (wrap-once, not per-request) — e.g. via a call counter on a stub `TrackActivity` showing it was invoked exactly once at wrap time.
- Construct a `map[Backend]string`, insert the result of `WithActivityTracking(...)` as a key; verify no panic (guards the pointer-not-value requirement directly).
**Depends on**: Step 9
**Parallelizable**: No — needs the full `TrackActivity` method (bookkeeping + wake branch) to wrap meaningfully.

### Step 12: Extract buildBroker() and fix main.go wiring order
**What**: In `cmd/broker/main.go`, extract the broker-construction wiring logic into a new, testable function (e.g. `buildBroker(cfg *config.Config) (mux http.Handler, ctrl *yield.Controller, err error)`, exact return shape driven by what `main()` currently needs) — today `main.go` has zero test seams, so a bare "`go test ./cmd/broker` passes" claim would be vacuous without this. Inside `buildBroker`'s **routes branch**, perform steps in this exact corrected order (the old plan had this backwards — `Router` captured undecorated backends by value before decoration happened, silently breaking idle-tracking for every routed instance in production):
  1. Construct backend(s) via `buildRoutes`/equivalent.
  2. Construct `ctrl` via `yield.NewWithConfirmMulti`.
  3. Call `ctrl.ConfigureIdle(...)` with the orig-index-ordered slice `[cfg.UpstreamIdleTimeout, route[0].IdleTimeout, ...]`.
  4. Decorate `be`/`routeBackends[i]` via `backend.WithActivityTracking(be, ctrl, origIdx)` wherever that instance's Idle duration is nonzero.
  5. **Only then** construct `backend.NewRouter(be)` and run the `AddRoute` loop, using the now-decorated `routeBackends[i]` values.
  Apply the same ordering (construct → `ConfigureIdle` → conditionally decorate → hand off) to the no-routes branch, where there is no `Router`/`AddRoute` step but the same before-handoff decoration requirement applies. Also compute `idleStatus func() any` (nil unless some instance has Idle configured, backed by `ctrl.IdleSummary()`) for Step 13 (FR16, AC1).
**Files**: `cmd/broker/main.go`
**Test**:
- `go test ./cmd/broker -v` passes.
- **New, real, automated test against `buildBroker` directly** (not just "existing tests still pass" — there are none today): construct a `*config.Config` with a route configured with a nonzero Idle duration and a `_UNIT_NAME`, call `buildBroker(cfg)`, obtain the resulting mux/`Router`, dispatch a request through it to the routed backend, and assert — via `ctrl.IdleSummary()` or an exported/test-visible accessor — that `inFlight`/`lastDispatch` for that instance's `origIdx` actually moved. This is the regression test for the ordering bug: it fails if `NewRouter` ever captures an undecorated backend by value again.
- Run the broker with `UPSTREAM_IDLE_TIMEOUT` unset; verify `/status` has no `"idle"` key (AC1).
- Run the broker with `UPSTREAM_IDLE_TIMEOUT=1m`; verify the default backend is decorated and `/status` includes the `"idle"` key.
**Depends on**: Step 2, Step 3, Step 10, Step 11
**Parallelizable**: No — converges the config track (Step 2) and the full `internal/yield`/`internal/backend` track (Steps 3–11).

### Step 13: Wire idleStatus into admin.Mux and update admin_test.go call sites
**What**: Modify `admin.Mux(...)` to accept a new parameter `idleStatus func() any`, and wire it into the `/status` endpoint the same way `routingStatus` and `tdarrStatus` are wired (FR16, AC12). Update every existing call to `admin.Mux(...)` — in `cmd/broker/main.go` (from Step 12's `buildBroker`, passing the real `idleStatus`) and in `internal/admin/admin_test.go` (passing `nil`) — for the new parameter.
**Files**: `internal/admin/admin.go`, `internal/admin/admin_test.go`, `cmd/broker/main.go`
**Test**:
- `go test ./internal/admin -v` passes.
- Verify `/status` includes an `"idle"` key when `idleStatus` is non-nil, and that the key is absent when `idleStatus` is nil (AC1, AC12).
**Depends on**: Step 12
**Parallelizable**: No — depends on `buildBroker` existing to know the real call site shape.

### Step 14: Update README.md config table and deploy/broker.service example
**What**: Add two rows to the config table in `README.md`:
  - `UPSTREAM_IDLE_TIMEOUT`: description, example value (e.g. `1h`), default (unset = disabled), noting it requires `UPSTREAM_UNIT_NAME`.
  - `BROKER_ROUTE_<N>_IDLE_TIMEOUT`: description, example value, default (unset = disabled), noting it requires `BROKER_ROUTE_<N>_UNIT_NAME`.
  Add a commented example line to `deploy/broker.service`, near the existing `# Environment=UPSTREAM_UNIT_NAME=...` line, showing `#Environment=UPSTREAM_IDLE_TIMEOUT=1h`.
**Files**: `README.md`, `deploy/broker.service`
**Test**: Visually confirm the new README rows are in the right place and match the existing format; confirm the `deploy/broker.service` line is syntactically correct for systemd (comment prefix, no stray whitespace).
**Depends on**: Step 2
**Parallelizable**: Yes — documentation only.

### Step 15: Update CONTEXT.md glossary and write ADR-0016
**What**: Add the **Idle** and **Idle-unload** glossary terms to `CONTEXT.md`, verbatim from `requirements.md`'s Terminology note. Write `docs/adr/0016-vllm-idle-unload.md`, matching the shape/tone of `docs/adr/0014-vllm-yield-symmetric-stop-start.md` and `docs/adr/0015-per-model-backend-routing.md` (do not inline their content — just match structure/voice), recording:
  - Same-goroutine composition: `checkIdleLocked()` runs inside `refresh()`'s existing `c.mu` tick, strictly after `applyLocked()`, not a second poller.
  - The `origIndex` orig-to-post-filter mapping design, including the sentinel for nil-filtered (no `_UNIT_NAME`) instances.
  - The FR11/AC7 two-`Unload` idempotent carve-out (Idle fires, then Contention's own unconditional per-instance unload loop fires again later on an already-unloaded instance — acceptable, not a bug).
  - The fire-and-forget-wake accepted cost (`TrackActivity`'s wake branch spawns `doReload` without the triggering request blocking on it directly).
  - The pointer-not-value `activityBackend` requirement (`Router.RoutingSummary()`'s use of `Backend` as a map key).
**Files**: `CONTEXT.md`, `docs/adr/0016-vllm-idle-unload.md` (new)
**Test**: Diff the new ADR's section headers against `docs/adr/0014-vllm-yield-symmetric-stop-start.md` and `docs/adr/0015-per-model-backend-routing.md` to confirm the same shape; visually confirm `CONTEXT.md`'s glossary section gained exactly the two new terms with no unrelated edits.
**Depends on**: Step 12, Step 2
**Parallelizable**: Yes — documentation, can run alongside Step 14 once Step 12 has landed (the ADR describes the shipped wiring design, so it needs that design finalized first).

### Step 16: Comprehensive AC-compliance test pass
**What**: Write dedicated tests (in `internal/yield/yield_test.go`, a new `internal/yield/idle_test.go`, and/or `internal/backend/router_test.go`) covering any FR/AC combinations not already exercised by an earlier step's own tests:
  - **AC1**: run the broker with all Idle vars unset, capture `/status` output, and diff it byte-for-byte against Step 1's `internal/admin/testdata/status_baseline.json`.
  - **AC7** (idle-Yield race, cross-component): using the real decorated `Router` + `Controller` from Step 12's `buildBroker` path, set up an instance with a short idle duration, let it idle-fire, then trigger a Contention/Yield transition in the same or a subsequent tick; verify the resulting `Unload`/`Reload` sequence is never a conflicting or out-of-order pair, and explicitly assert that a second, later, idempotent `Unload` from Contention's own loop on an already-idle-unloaded instance is treated as a pass, not a failure.
  - **AC13**: run the full existing `internal/yield` and `internal/backend` test suites and confirm no regressions to Contention-only scenarios.
  - Spot-check any remaining FR not yet covered by a dedicated assertion in Steps 2–13 (FR9, FR10, FR12 in particular, since they describe emergent behavior of the whole chain rather than one function).
**Files**: `internal/yield/yield_test.go`, `internal/yield/idle_test.go` (new, optional), `internal/backend/router_test.go`, `internal/admin/testdata/status_baseline.json` (read-only, from Step 1)
**Test**:
- `go test ./internal/yield -v` and `go test ./internal/backend -v` both pass with no new failures.
- Each AC has a dedicated, independently-verifiable test case (either here or cross-referenced to the step that already covers it).
- The AC7 race test asserts exactly the documented outcome (at most one *conflicting* pair; the idempotent double-`Unload` carve-out passes, not fails).
**Depends on**: Steps 1–15
**Parallelizable**: No — integration testing; must run after every component is in place.

## Rollback plan

**First-line rollback (no code revert needed):** Because FR4 guarantees unset/zero Idle-timeout vars are byte-for-byte identical to today's behavior, the fastest and safest rollback for a *deployed* instance is purely operational — unset `UPSTREAM_IDLE_TIMEOUT` and every `BROKER_ROUTE_<N>_IDLE_TIMEOUT` in the systemd environment file / service unit and restart the broker. This disables Idle-unload completely with zero code revert, zero redeploy, and zero risk of reintroducing an unrelated regression. Try this before anything below.

**Full code rollback (if the operational rollback is insufficient, e.g. a bug in code paths that run even with Idle disabled):**
- Steps 1–15: `git diff` to review the code changes and `git reset --hard` to discard.
- Step 16: Tests can be deleted or reverted.

Specific cautions:
- If admin.Mux's new parameter rollback leaves existing tests failing, re-run `go test ./internal/admin` to verify the revert was complete.
- If a deployment was made with the new config vars active, rolling back the code does not automatically unset the environment variables on the running instances; operator must manually remove `UPSTREAM_IDLE_TIMEOUT` / `BROKER_ROUTE_<N>_IDLE_TIMEOUT` from systemd service files or env files before redeploying the old code (otherwise `config.Load()` will fail on the revert) — this is the same manual step as the first-line operational rollback above, so in practice it should already be done before a full code rollback is ever needed.
