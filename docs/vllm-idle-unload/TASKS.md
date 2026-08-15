# Tasks: Idle-unload for openai-compatible (vLLM) backend instances

Generated from: docs/vllm-idle-unload/ on 2026-08-15

## Status legend
- [ ] pending
- [>] in progress
- [x] done
- [!] blocked

## Tasks

### Task 1: Capture baseline /status output for the AC1 comparison
**Status**: [x] done
**Files**: internal/admin/testdata/status_baseline.json (new)
**Test**: Fixture exists, valid JSON, no "idle" key, captured against the real pre-feature /status handler.
**Depends on**: none
**Parallelizable**: No — must run first
**Notes**:

### Task 2: Add idle-duration config vars and validation to config.go
**Status**: [x] done
**Files**: internal/config/config.go
**Test**: go test ./internal/config -v passes; valid duration parses; missing _UNIT_NAME fails Load(); negative duration fails Load(); explicit 0 = disabled; unset = disabled.
**Depends on**: none
**Parallelizable**: Yes (relative to Task 3)
**Notes**:

### Task 3: Add idle-tracking fields and ConfigureIdle to Controller
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: go test ./internal/yield -v passes; origIndex/origToFiltered maps correctly with sentinel for nil-filtered positions; ConfigureIdle maps orig->post-filter correctly, no panic on empty slice, errors on out-of-bounds.
**Depends on**: none
**Parallelizable**: Yes (relative to Task 2)
**Notes**:

### Task 4: Add typed unloadTrigger and thread through doUnload/doReload
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: go test ./internal/yield -v passes; Yield-sourced unload logs trigger=yield; unloadTrigger is a distinct type.
**Depends on**: Task 3
**Parallelizable**: No
**Notes**:

### Task 5: Add checkIdleLocked timer-check and wire into refresh()
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: go test ./internal/yield -v passes; idle elapses -> doUnload fires once with triggerIdle; request before deadline resets timer; two instances with different durations are independent.
**Depends on**: Task 4
**Parallelizable**: No
**Notes**:

### Task 6: Add checkIdleLocked guards (Yield-active, in-flight, dedup)
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: Yield-active skips instance; in-flight>0 skips instance; already-idleUnloaded skips (no duplicate Unload).
**Depends on**: Task 5
**Parallelizable**: No
**Notes**:

### Task 7: Update applyLocked yield-clear branch to reset idle state
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: Idle-unload then Yield-clear resets lastDispatch/idleUnloaded; not immediately re-unloaded next tick.
**Depends on**: Task 6
**Parallelizable**: No
**Notes**:

### Task 8: Add TrackActivity plain-tracking decorator to Controller
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: inFlight increments/decrements correctly around a request; lastDispatch moves; deferred decrement still fires if wrapped handler panics.
**Depends on**: Task 3
**Parallelizable**: Yes (relative to Tasks 4-7)
**Notes**:

### Task 9: Add TrackActivity wake-on-request branch
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: idleUnloaded CAS true->false triggers doReload with triggerIdle; reload waits on unload's actionDone chain (no start-before-stop); concurrent requests only fire one doReload.
**Depends on**: Task 8
**Parallelizable**: No
**Notes**:

### Task 10: Add IdleSummary exported method to Controller
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: One idle-enabled instance -> slice with one correct entry; no idle-configured instances -> nil; JSON serialization correct.
**Depends on**: Task 3
**Parallelizable**: Yes (relative to Tasks 4-9)
**Notes**:

### Task 11: Create internal/backend/activity.go with WithActivityTracking
**Status**: [x] done
**Files**: internal/backend/activity.go (new)
**Test**: go test ./internal/backend -v passes; request through decorated Proxy() moves Controller's inFlight/lastDispatch; Proxy() called twice returns same handler (wrap-once); inserting into map[Backend]string as key does not panic (pointer-not-value check).
**Depends on**: Task 9
**Parallelizable**: No
**Notes**:

### Task 12: Extract buildBroker() and fix main.go wiring order
**Status**: [x] done
**Files**: cmd/broker/main.go, cmd/broker/main_test.go (new)
**Test**: go test ./cmd/broker -v passes; new automated test proves a routed backend with idle configured is actually tracked (regression test for the ordering bug — ctrl/ConfigureIdle/decoration happen BEFORE NewRouter/AddRoute); /status has no "idle" key when unset; has the key when UPSTREAM_IDLE_TIMEOUT=1m.
**Depends on**: Task 2, Task 3, Task 10, Task 11
**Parallelizable**: No
**Notes**:

### Task 13: Wire idleStatus into admin.Mux and update admin_test.go call sites
**Status**: [x] done
**Files**: internal/admin/admin.go, internal/admin/admin_test.go, cmd/broker/main.go
**Test**: go test ./internal/admin -v passes; /status includes "idle" key iff idleStatus non-nil.
**Depends on**: Task 12
**Parallelizable**: No
**Notes**:

### Task 14: Update README.md config table and deploy/broker.service example
**Status**: [x] done
**Files**: README.md, deploy/broker.service
**Test**: Visually confirm new rows/lines match existing format and are syntactically correct.
**Depends on**: Task 2
**Parallelizable**: Yes
**Notes**:

### Task 15: Update CONTEXT.md glossary and write ADR-0016
**Status**: [x] done
**Files**: CONTEXT.md, docs/adr/0016-vllm-idle-unload.md (new)
**Test**: CONTEXT.md gains exactly the Idle/Idle-unload terms; new ADR's section headers match the shape of ADR-0014/0015.
**Depends on**: Task 12, Task 2
**Parallelizable**: Yes (alongside Task 14, once Task 12 lands)
**Notes**:

### Task 16: Comprehensive AC-compliance test pass
**Status**: [x] done
**Files**: internal/yield/yield_test.go, internal/yield/idle_test.go (new, optional), internal/backend/router_test.go, internal/admin/testdata/status_baseline.json (read-only)
**Test**: go test ./internal/yield -v and go test ./internal/backend -v pass with no new failures; AC1 diffed against Task 1's baseline; AC7 race test asserts idempotent double-Unload passes, conflicting pair fails; full existing suites pass unmodified (AC13).
**Depends on**: Tasks 1-15
**Parallelizable**: No
**Notes**:

## Blocked / open
(none yet)
