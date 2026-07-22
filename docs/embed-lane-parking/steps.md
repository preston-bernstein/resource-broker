# Steps: Embed-Lane Request Parking

## Prerequisites

**Verify git tracking for `internal/queue`** — the `.gitignore` collision is fixed upstream (commit `e565edc`):
```bash
git ls-files internal/queue | wc -l   # expect ≥ 9
```
If this count is lower, re-sync with `v2-go` (do not re-diagnose); the fix is already in place.

**WIP awareness** — the main checkout (`v2-go`) may carry uncommitted Plex-session detection work on `internal/config/config.go` and `cmd/broker/main.go`. This worktree's implementation rebases onto that before merge; overlap is limited to append-only diffs in those files (not existing-line changes). Remaining redesign impact on `internal/yield/yield.go`: **zero** (see Core redesign in plan).

---

## Step 1: Create `internal/queue/park.go` — parking types, lifecycle, and drain loop

**What**: New file `internal/queue/park.go` with all parking infrastructure: `parkResult` enum, `parkedReq` struct, `parker` struct with `mu`, queue slice, config fields; `(s *Scheduler) parkFor(ctx)` blocking wait; `(p *parker) remove(pr)` cleanup; `(p *parker) drainOneBurst(burst)` release batches; `(s *Scheduler) RunParkDrain(ctx, yielding func() bool)` polling drain loop; `(s *Scheduler) SetParkConfig(hold, maxQueue, drainBurst)` config setter; `(s *Scheduler) SetShutdownContext(ctx)` shutdown wiring; `(s *Scheduler) parkEnabled()` kill-switch check; `(s *Scheduler) shutdownDone()` helper.

**Files**: 
- `internal/queue/park.go` (new)

**Test**:
```bash
go build ./internal/queue/...  # Compiles once scheduler.go adds park field (Step 2)
# Semantic tests in Steps 5–7 (gate tests, not unit tests on park.go alone)
```

**Depends on**: None — this is self-contained infrastructure.

**Parallelizable**: Yes — no file overlap with other core steps; gate.go doesn't call into it until Step 5.

---

## Step 2: Update `internal/queue/scheduler.go` — add `park`, `shutdownCtx`, and accessor methods

**What**: Add two fields to `Scheduler` struct: `park parker` and `shutdownCtx context.Context` (defaulting to `context.Background()` in `New()`, so existing tests unaffected). Add three new methods: `SetParkConfig(hold time.Duration, maxQueue, drainBurst int)` (config setter), `SetShutdownContext(ctx context.Context)` (shutdown signal wiring), and update `Stats()` return struct to include `Parked int` field (reads `len(s.park.q)` under lock). Add lock-order comment on `Stats()` noting `park.mu` is taken nested inside `s.mu`, only place both are held together.

**Files**: 
- `internal/queue/scheduler.go`

**Test**:
```bash
go build ./internal/queue/...
go test -run TestSchedulerStats ./internal/queue/... -v
# Existing Stats test passes with new Parked field zeroed
```

**Depends on**: Step 1 (types `parker` and `parkResult` must exist).

**Parallelizable**: Yes — no overlap with Step 3, 4, 5; scheduler.go changes are isolated.

---

## Step 3: Rewrite `internal/queue/gate.go` — admission loop, no Admission interface change

**What**: **Admission interface remains unchanged** (no `YieldEnd()` method added). Rewrite `Gate`'s admission section from two straight-line `if` statements into a `for` loop with five-way switch on `parkFor` result: (1) `parkReleased` → continue, (2) `parkExpired` → 503/deferred, (3) `parkRejected` → 503/park_rejected, (4) `parkCanceled` → no response (client gone), (5) `parkShutdown` → 503/crash_failed. Add new `parkEnabled()` helper returning true iff `park.maxQueue != 0`. Track `wasParked` and `parkWait` for metrics. Widen `deferRequest(w, rec, cls, status, outcome, reason, wait, elapsed)` signature (separate `status` string for X-Broker-Status header value from `outcome` string for metrics tag, to permit "expired"/"park_rejected"/"crash_failed" outcomes with "deferred" or "crash_failed" status values). Call `rec.RecordPark(parkWait)` when serving a request that was parked.

**Files**: 
- `internal/queue/gate.go`

**Test**:
```bash
go test -run TestGateBasic ./internal/queue/... -v
# Existing gate tests compile and pass (deferRequest signatures updated in place)
```

**Depends on**: Step 1 (parkFor, parkResult types), Step 2 (Scheduler methods).

**Parallelizable**: No — blocks Step 5's test file changes.

---

## Step 4: Update `internal/metrics/metrics.go` — add `RecordPark`, park-wait counters, depth gauge

**What**: Add `parkWaitSumMs float64, parkWaitCount int64` fields to `Registry` struct. Add `RecordPark(wait time.Duration)` method to `Registry` (increments parkWaitSumMs by milliseconds, parkWaitCount by 1). Add `Parked int` field to `Gauges` struct. Add four `write()` calls in `write()` method: `broker_parked_depth` gauge (no class label, help text "batch-class park depth; Interactive never parks"), `broker_park_wait_seconds_sum` and `broker_park_wait_seconds_count` counter pair (mirrors `broker_wait_seconds_*` pattern). Add `RecordPark(wait time.Duration)` method to `Recorder` interface in `gate.go`.

**Files**: 
- `internal/metrics/metrics.go`
- `internal/queue/gate.go` (Recorder interface only — one new method)

**Test**:
```bash
go build ./internal/metrics/...
go test ./internal/metrics/... -v
# Existing metrics tests pass; new RecordPark call is validated in Step 5's gate tests
```

**Depends on**: Step 1 (context for outcome strings), Step 2 (Stats.Parked field).

**Parallelizable**: Yes — no overlap with Step 3 or Step 5; metrics is standalone.

---

## Step 5: Update `internal/config/config.go` — add `ParkHold`, `ParkMaxQueue`, `ParkDrainBurst` with loaders

**What**: Add three exported fields to `Config` struct: `ParkHold time.Duration`, `ParkMaxQueue int`, `ParkDrainBurst int`. Add `getdur("BROKER_PARK_HOLD", 600*time.Second)` call and two `getint` calls in `Load()` to read the env vars. **Critical:** `BROKER_PARK_MAX_QUEUE` must allow `0` as the documented kill-switch; the existing `getint` rejects any value `< 1`. Create a second loader helper (e.g., `getintMin(key string, def, min int)` or a dedicated `getParkMaxQueue(...)`) that permits `0` and rejects only `< 0`, call it out explicitly in the diff so it isn't silently misused. Defaults: `BROKER_PARK_HOLD=600s`, `BROKER_PARK_MAX_QUEUE=32`, `BROKER_PARK_DRAIN_BURST=8`.

**Files**: 
- `internal/config/config.go`

**Test**:
```bash
go build ./internal/config/...
# Sanity: parse defaults
echo BROKER_PARK_MAX_QUEUE=0 go test -run TestConfig ./internal/config/... -v
```

**Depends on**: None.

**Parallelizable**: Yes — can write in parallel with Step 4; config.go changes are isolated.

---

## Step 6: Update `internal/config/config_test.go` — defaults, overrides, invalid-value guards for all three vars

**What**: Add three test cases per field (default resolution, env override, invalid-value rejection) following the existing house style in `broker-config-and-flags`. **Separate test for `ParkMaxQueue`:** assert `0` loads successfully (kill-switch), negative values are rejected/warned. For `ParkHold` and `ParkDrainBurst`: assert `<= 0` values are rejected/warned. For `ParkDrainBurst`: assert `< 1` is rejected. All together: 9+ assertions covering valid/invalid paths per field.

**Files**: 
- `internal/config/config_test.go`

**Test**:
```bash
go test -run TestConfigPark ./internal/config/... -v
# All 9+ assertions pass; BROKER_PARK_MAX_QUEUE=0 specifically tested as valid
```

**Depends on**: Step 5 (fields and loaders exist).

**Parallelizable**: Yes — can write in parallel with Step 5; same package, different focus.

---

## Step 7: Add AC-1 through AC-9 tests to `internal/queue/gate_yield_test.go` and `scheduler_test.go`

**What**: Six new test functions in `gate_yield_test.go`: (1) `TestGateParksDuringYield` — Batch during yield parks, yield ends, request served (AC-1); (2) `TestGateParkExpires` — request exceeds hold bound, 503/expired, upstream not hit (AC-2); (3) `TestGateParkQueueCeiling` — at maxQueue, next arrival rejected with park_rejected, no block (AC-3); (4) `TestGateInteractiveNeverParksWhenConfigured` — Interactive class during yield always deferred, never parked (FR-10, distinct from AC-4); (5) `TestGateParkDrainBurst` — N requests exceeding drainBurst are released in paced batches, FIFO order (AC-5); (6) `TestGateParkClientDisconnect` — cancelling parked request's context exits immediately, outcome=canceled (AC-6); (7) `TestGateParkShutdown` — shutdown signal propagates to parked requests, all fail with crash_failed within 10s (AC-7). In `scheduler_test.go`: update existing `TestSchedulerStats` to verify `Parked` initializes to `0`. Three additional tests in `scheduler_test.go`: (1) `TestParkGhostCleanup` — let requests expire/cancel/shutdown, verify park queue depth returns to true live count (FR-15 ghost-entry fix); (2) `TestParkConcurrentCeilingRace` — many goroutines call `parkFor` at ceiling, run with `-race`, assert queue never exceeds maxQueue and exactly maxQueue are accepted (FR-14 atomicity); (3) `TestRunParkDrainPacing` — assert successive release bursts separated by ~1s intervals, not released all at once (pacing verification). In `metrics` package: add `TestBrokerParkedDepth` and `TestBrokerParkWaitMetrics` asserting gauge reflects live count and wait counters increment for each outcome (AC-8/AC-9).

**Files**: 
- `internal/queue/gate_yield_test.go`
- `internal/queue/scheduler_test.go`
- `internal/metrics/metrics_test.go`

**Test**:
```bash
go test -run 'TestGatePark|TestParkGhost|TestParkConcurrent|TestRunParkDrain' ./internal/queue/... -v -race
go test -run TestBrokerParked ./internal/metrics/... -v
# All new tests pass, no race detections
```

**Depends on**: Step 3 (Gate loop exists), Step 4 (Recorder.RecordPark exists), Step 5–6 (config types).

**Parallelizable**: No — test file depends on all gate.go, scheduler.go, config changes being in place.

---

## Step 8: Update `internal/queue/gate_test.go` and `gate_wait_test.go` test fakes — add `RecordPark` method

**What**: Every existing `Recorder` mock in these files gains a `RecordPark(time.Duration)` method (one-line no-op, just appending to an internal slice if tracking is needed, or empty body). No changes to `Admission` interface mocks — they remain unmodified from the current HEAD. Specifically: `manualRec` (if it exists as a test fake) gains `RecordPark`.

**Files**: 
- `internal/queue/gate_test.go`
- `internal/queue/gate_wait_test.go`

**Test**:
```bash
go test ./internal/queue/... -v
# All existing tests compile and pass; test fakes satisfy Recorder interface
```

**Depends on**: Step 4 (Recorder.RecordPark interface change).

**Parallelizable**: No — must follow interface widening.

---

## Step 9: Re-point `TestGateRefusesWhenYielding` with a comment (AC-4)

**What**: The existing test in `gate_yield_test.go` exercises Batch-class behavior when parking is **never configured** (`SetParkConfig` not called). Add a comment above the test clarifying it pins FR-13's fail-closed behavior: "This test verifies that a Batch request during yield behaves the same as today when parking is never configured (`BROKER_PARK_MAX_QUEUE=0` by default). It is the fail-closed pin for FR-13: reject immediately, no park." Do not modify any assertions — the test passes unchanged.

**Files**: 
- `internal/queue/gate_yield_test.go`

**Test**:
```bash
go test -run TestGateRefusesWhenYielding ./internal/queue/... -v
# Test passes unchanged; comment clarifies its new role as fail-closed pin
```

**Depends on**: Step 1–3 (parking infrastructure in place so the never-configured path is clear).

**Parallelizable**: No — part of test file updates.

---

## Step 10: Update `cmd/broker/main.go` — wire `SetParkConfig`, `SetShutdownContext`, `RunParkDrain`

**What**: After existing `sched.SetMaxWaiters(cfg.MaxWaiters)` and `sched.SetMaxInflight(cfg.MaxInflight)` calls, add two lines: `sched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)` and `sched.SetShutdownContext(ctx)`. After existing `go ctrl.Run(ctx)`, add adapter closure `yieldingFn := func() bool { y, _ := ctrl.Yielding(); return y }` and then `go sched.RunParkDrain(ctx, yieldingFn)`. Repeat both `SetParkConfig`, `SetShutdownContext`, and `go ... RunParkDrain(...)` for `embedSched` inside the `if cfg.InfinityURL != nil` block (same adapter closure, reused). Update `metricsHandler`'s `Gauges{...}` literal to include `Parked: st.Parked` (from `sched.Stats()`; **not** `embedSched.Stats()`—documented scope limit per Risk Areas in plan).

**Files**: 
- `cmd/broker/main.go`

**Test**:
```bash
go build ./cmd/broker/...
# Sanity smoke test: BROKER_PARK_MAX_QUEUE=5 ./broker &
# (kill with Ctrl-C; verify startup and shutdown are clean, no panic)
```

**Depends on**: Step 2 (SetParkConfig, SetShutdownContext methods), Step 5 (cfg.ParkHold, etc. fields).

**Parallelizable**: No — final wiring step, depends on implementation to be ready.

---

## Step 11: Update `internal/admin/admin.go` — add `"parked"` field to `/status` response

**What**: In the `/status` handler where the `"queue"` map is built, add one line to the existing map literal: `"parked": st.Parked` (the `st := stats.Stats()` call already happens at the top of the handler, and `Stats.Parked` was added in Step 2, so this is a one-line addition to the existing map).

**Files**: 
- `internal/admin/admin.go`

**Test**:
```bash
go build ./internal/admin/...
# Full admin tests skipped (pre-existing failure per plan); just verify build
```

**Depends on**: Step 2 (Stats.Parked field), Step 1–10 (full feature in place so the field has meaning).

**Parallelizable**: No — admin changes are final polish, after core logic is solid.

---

## Step 12: Write documentation — `docs/adr/0009-*.md`, amend `0002-*.md` status, `CONTEXT.md`, `README.md`

**What**: Create `docs/adr/0009-embed-lane-parking.md` (one page, house style): **Decision** — Batch-class requests parking during yield, FIFO release on 1s polling, hard ceilings. **Why now** — embed cascade, LightRAG research. **Answer ADR-0002's three objections**: fd exhaustion (hard ceiling per FR-5/NFR-3), timeouts (600s < 1200s, documented margin in NFR-1/NFR-2), reboot-safety (in-memory only per NFR-4, fail-fast on graceful shutdown per FR-9, hard crash is unobservable — recovery is LightRAG's rescan+cache). **Rejected alternatives**: unbounded, sync.Cond, `internal/park` package, token bucket, `yield.Controller.YieldEnd()` (the rejected signal-based design), separate `BROKER_PARK_ENABLED` toggle. **StartLimitBurst decision** — unchanged (`StartLimitBurst=5`, `StartLimitIntervalSec=60`), because parked requests are bounded and fail-fast on graceful shutdown; a park-path bug would panic (same as any handler bug), not merely block. **Release-order distinction** — FIFO governs *release-signal order*, not end-to-end service order (released requests re-enter `Scheduler.Acquire` where Interactive can still jump ahead). **Scope note** — yield transition, Job path, Interactive, Ollama unchanged. Amend `docs/adr/0002-stateless-http-bounded-wait.md` status line in place to note it is superseded *for Batch "arrives during yield" behavior specifically* (the rest of stateless HTTP remains). Add to `CONTEXT.md` two new entries after **Queue**: **Park** / **Parked request** (definition distinguishes from Queue, notes kill-switch), `_Avoid_` list (Hold, Buffer, Suspend, Queue); **Drain burst** (paced release), `_Avoid_` list (Flush, Replay-all, Burst alone). Add to `README.md` `### Configuration (env)` table three rows for `BROKER_PARK_HOLD` (default 600s, hold bound), `BROKER_PARK_MAX_QUEUE` (default 32, ceiling; 0=off), `BROKER_PARK_DRAIN_BURST` (default 8, per tick).

**Files**: 
- `docs/adr/0009-embed-lane-parking.md` (new)
- `docs/adr/0002-stateless-http-bounded-wait.md` (status line)
- `CONTEXT.md`
- `README.md`

**Test**:
```bash
ls docs/adr/0009-embed-lane-parking.md  # exists
grep "fd exhaustion" docs/adr/0009-embed-lane-parking.md  # answers C-3
grep "StartLimit" docs/adr/0009-embed-lane-parking.md  # answers NFR-7
grep "Park" CONTEXT.md  # glossary entry exists
grep "BROKER_PARK_HOLD" README.md  # configuration table row exists
```

**Depends on**: None (documentation can precede or follow code).

**Parallelizable**: Yes — documents can write in parallel with Steps 1–10.

---

## Step 13: Update `deploy/broker.service` — add three `Environment=` lines with explanatory comment

**What**: Add three lines (`Environment=BROKER_PARK_HOLD=600s`, `BROKER_PARK_MAX_QUEUE=32`, `BROKER_PARK_DRAIN_BURST=8`) with a comment block (matching the `ad07905`-style in-unit-comment precedent) explaining the defaults and explicitly naming `BROKER_PARK_MAX_QUEUE=0` as the documented first-line rollback: "If parking misbehaves, set `BROKER_PARK_MAX_QUEUE=0` and restart (kill-switch, no code change)." Verify `Restart=always` is already present (it is).

**Files**: 
- `deploy/broker.service`

**Test**:
```bash
systemd-analyze verify deploy/broker.service  # syntax pass
grep "BROKER_PARK" deploy/broker.service | wc -l  # 3
```

**Depends on**: None.

**Parallelizable**: Yes — deployment config, independent.

---

## Step 14: Update `.claude/skills/broker-config-and-flags/SKILL.md` — add config axis rows

**What**: Add three rows to the env var table for `BROKER_PARK_HOLD`, `BROKER_PARK_MAX_QUEUE`, `BROKER_PARK_DRAIN_BURST` with defaults (600s, 32, 8). Update the "how to add a config axis" precedent section if it names specific examples — add these three as a precedent example.

**Files**: 
- `.claude/skills/broker-config-and-flags/SKILL.md`

**Test**:
```bash
grep "BROKER_PARK_HOLD" .claude/skills/broker-config-and-flags/SKILL.md  # exists
```

**Depends on**: None.

**Parallelizable**: Yes — skill documentation, independent.

---

## Step 15: Update `.claude/skills/broker-architecture-contract/SKILL.md` — add parking invariant (optional, recommended)

**What**: Add one new row to the invariants table: "Parking never lets a Batch request hold a GPU slot while parked (NFR-3); parked requests are always resolved within the 10s shutdown window on graceful stop (FR-9); `RunParkDrain` is the one `Scheduler` method taking a plain closure instead of `Admission` (intentional per Core redesign—see parking ADR)."

**Files**: 
- `.claude/skills/broker-architecture-contract/SKILL.md`

**Test**:
```bash
grep "hold.*slot.*parked" .claude/skills/broker-architecture-contract/SKILL.md  # exists
```

**Depends on**: None.

**Parallelizable**: Yes — optional, can follow main steps; skill documentation.

---

## Step 16: Write `docs/runbooks/embed-parking-soak.md` — chaos/soak validation (AC-14)

**What**: Runbook for AC-14: (1) Start broker with parking enabled (BROKER_PARK_MAX_QUEUE=32, defaults). (2) Start fake gaming process (or use existing manual contention tooling) to force yield mid-embed-burst. (3) Inject LightRAG embed requests against broker while gaming. (4) Measure: zero LightRAG ingest failures in logs, all parked requests served within hold bound, `broker_parked_depth` returns to 0 post-drain. (5) Note: use existing `POST /control {"mode":"yield"} / {"mode":"auto"}` endpoints (ADR-0005) to force yield in test, not a fake process. Include curl command with `BROKER_CONTROL_TOKEN` header note. Mark as "deploy-deferred" (runs at drain-end of ship-it, not before merge).

**Files**: 
- `docs/runbooks/embed-parking-soak.md` (new)

**Test**:
```bash
ls docs/runbooks/embed-parking-soak.md  # exists
```

**Depends on**: Step 10 (broker wired and running).

**Parallelizable**: No (runbook is procedural, requires running broker).

---

## Step 17: Final gate — full `go test -race ./...` and `go vet ./...` before/after diff

**What**: Capture baseline test/vet output before Step 1. Run full test suite and vet suite after Step 10 (all code in place). Diff the failure sets. Assert only pre-existing `internal/admin` failure remains (no new failures). Document diff in PR/commit body per C-6.

**Files**: (all Go source files)

**Test**:
```bash
# Baseline (before Step 1):
go test ./... 2>&1 | tee /tmp/test-before.log
go vet ./... 2>&1 | tee /tmp/vet-before.log

# After Step 10:
go test -race ./... 2>&1 | tee /tmp/test-after.log
go vet ./... 2>&1 | tee /tmp/vet-after.log

# Diff:
diff /tmp/test-before.log /tmp/test-after.log  # only internal/admin diff
diff /tmp/vet-before.log /tmp/vet-after.log    # only internal/admin diff
```

**Depends on**: Step 10 (all code complete).

**Parallelizable**: No — final validation gate.

---

## Rollback plan

**Immediate** (deploy-time):
1. Set `BROKER_PARK_MAX_QUEUE=0` (kill-switch) and restart the broker — reverts to today's immediate-503 behavior for Batch during yield, no code change.
2. If that doesn't stabilize, revert commits in reverse order: `git revert` each commit SHA from Step 10 back to Step 1.

**Configuration-only rollback** (no rebuild):
- `BROKER_PARK_HOLD` or `BROKER_PARK_DRAIN_BURST` can be tuned via env var after restart if the defaults are found inadequate.
- `BROKER_PARK_MAX_QUEUE` can only be changed by env var; changing the ceiling in code requires recompile.
- `StartLimitBurst`/`StartLimitIntervalSec` decision in ADR-0009 can be revisited by amending the ADR in place, then updating `deploy/broker.service` and restarting (no code change).

**State recovery**:
- Park state is in-memory only (per NFR-4); stopping and restarting the broker clears all parked requests. No recovery mechanism needed (LightRAG's rescan+cache is the external recovery).

---

Steps written: **17 steps, 9 marked parallelizable.**
