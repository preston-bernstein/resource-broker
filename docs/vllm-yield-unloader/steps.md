# Steps: vLLM/OpenAI-backend VRAM-yield Unloader

## Prerequisites
None. The feature is standalone; no prior feature, external access, or live vLLM/systemd host is required. Provisioning the actual vLLM systemd unit and the sudoers/polkit rule on desktop (10.0.0.243) is out-of-repo operator work and not a dependency for merging this code (see requirements.md "Out of scope").

## Implementation steps

### Step 1: Write ADR-0014 (symmetric stop/start decision)
**What**: Write the one-page ADR documenting the `systemctl stop`/`systemctl start` decision, referencing ADR-0003 and ADR-0004, the rejected `systemctl restart` mechanism (and why), the rejected vLLM `/sleep`+`/wake_up` API (citing vllm-project/vllm#20627), the recommended unit-specific sudoers/polkit scoping, and the named accepted costs / deliberately-rejected scope items (connection-error window, no startup preflight, no new Prometheus metric).
**Files**: `docs/adr/0014-vllm-yield-symmetric-stop-start.md`
**Test**: Read the ADR and confirm it contains all of: (1) the stop/start decision with explicit references to ADR-0003 and ADR-0004; (2) rejected-alternative sections for both `systemctl restart` (VRAM recontention after eager reload) and vLLM `/sleep`+`/wake_up` (citing #20627); (3) a security-scope recommendation for a unit-specific (never wildcard) sudoers/polkit grant; (4) named accepted costs (connection errors during stop/start window) and named deliberate scope cuts (no preflight check, no new Prometheus metric).
**Depends on**: none.
**Parallelizable**: Yes.

### Step 2: Add UpstreamUnitName to config.Load()
**What**: Add an `UpstreamUnitName string` field to `Config` in `internal/config/config.go` (doc comment placed alongside `UpstreamURL`/`UpstreamAPIKey`), and in `Load()` set it via `strings.TrimSpace(getenv("UPSTREAM_UNIT_NAME", ""))` so a whitespace-only value collapses to `""` before any downstream empty-check.
**Files**: `internal/config/config.go`
**Test**: `go build ./internal/config` compiles; `grep -n "UpstreamUnitName" internal/config/config.go` shows both the struct field and the `Load()` assignment using `strings.TrimSpace`.
**Depends on**: none.
**Parallelizable**: Yes.

### Step 3: Add config_test coverage for UPSTREAM_UNIT_NAME
**What**: Extend `internal/config/config_test.go` with three cases: unset → `cfg.UpstreamUnitName == ""`; whitespace-only (`t.Setenv("UPSTREAM_UNIT_NAME", "   ")`) → `""`; set with surrounding whitespace (`"  vllm  "`) → `"vllm"`.
**Files**: `internal/config/config_test.go`
**Test**: `go test ./internal/config -v -run TestLoad` passes, covering all three states (satisfies FR17 / AC11).
**Depends on**: Step 2 (field must exist to compile against).
**Parallelizable**: No.

### Step 4: Widen yield.Unloader to add Reload, and fix the yield package's test doubles in the same step
**What**: In `internal/yield/yield.go`: widen the `Unloader` interface to `{ Unload(ctx) error; Reload(ctx) error }`; add a package-level `unloadReloadTimeout = 30 * time.Second` constant (replacing the two inline `10*time.Second` literals used by `doUnload`/the new `doReload` only — `pauseGPUMgr`/`resumeGPUMgr`'s own 10s literals are untouched, out of scope) with a code comment documenting that the timeout is a client-side give-up bound only (killing `exec.CommandContext` does not cancel the systemd job already queued via D-Bus); add `doReload()` mirroring `doUnload()` exactly (`recover()` + `slog.Error("panic in vram reload", ...)`, `context.WithTimeout(..., unloadReloadTimeout)`, `slog.Warn("vram reload failed", ...)` / `slog.Info("vram reload requested")`); in `applyLocked`'s `eff == false` branch add `if c.unloader != nil { go c.doReload() }`, symmetric to the existing `eff == true` branch. In the same step, in `internal/yield/yield_test.go`: add a no-op `Reload` to all four existing test doubles (`fakeUnloader`, `panicUnloader`, `erroringUnloader`, `deadlineCapturingUnloader`) so the package keeps compiling; extend `fakeUnloader` with reload-call tracking and extend `TestYieldTransitionCancelsServeAndUnloads` (or add a sibling test) to drive a full yield-then-clear cycle asserting `Reload` fires exactly once on clear; add `doReload` counterparts of the existing `doUnload` hardening tests (panic-recovery asserting `"panic in vram reload"`, error-logged-not-panicked asserting `"vram reload failed"`); update `TestDoUnloadUsesTenSecondTimeout`'s window math to the new `unloadReloadTimeout` value and add the equivalent deadline-pinning test for `doReload`; leave `TestGPUManagerPausedOnYieldAndResumedOnClear`'s 9s/11s window untouched.
**Files**: `internal/yield/yield.go`, `internal/yield/yield_test.go`
**Test**: `go test ./internal/yield -v` passes with no compile errors — this is the check that the package never sits in a broken-compile-state as committed, since the interface widening and the test-double fix land together.
**Depends on**: Step 1 (ADR must land before-or-with this core mechanism change, per plan.md's Integration points ordering constraint).
**Parallelizable**: No.

### Step 5: Add no-op Reload to ollama.Client
**What**: Add a `Reload(ctx context.Context) error` method to `*ollama.Client` in `internal/ollama/client.go` that returns `nil` immediately, with a comment explaining Ollama's reload is implicit (the next `/api/generate` call lazily reloads whatever `keep_alive=0` unloaded) — this exists purely so `*ollama.Client` keeps satisfying the widened `yield.Unloader` interface that `ollamaBackend.Unloader()` (returning `b.c`) relies on.
**Files**: `internal/ollama/client.go`
**Test**: `go build ./...` compiles (proves `internal/backend`'s `ollamaBackend.Unloader()` still satisfies the widened interface); new test asserts `client.Reload(ctx)` returns `nil` without making any network call or invoking any command (satisfies AC20).
**Depends on**: Step 4 (the interface isn't widened, and nothing requires a second method, until then; `internal/backend` won't compile against a 2-method `yield.Unloader` until this lands).
**Parallelizable**: No.

### Step 6: Fix backend.go's Unloader() doc comment
**What**: Update the doc comment on `Backend.Unloader()` in `internal/backend/backend.go` so it no longer implies every backend always returns nil, while still requiring that nil-when-applicable be a literal nil interface value (never a typed-nil concrete pointer).
**Files**: `internal/backend/backend.go`
**Test**: Read the updated comment and confirm it states the typed-nil invariant without asserting or implying every backend always takes the nil path (satisfies FR20 / AC19).
**Depends on**: none.
**Parallelizable**: Yes.

### Step 7: Define systemdUnitController and its Unload/Reload methods
**What**: In `internal/backend/openai_backend.go`, add the `systemdUnitController` type (`unit string`, `run func(ctx context.Context, verb string) error`, `mu sync.Mutex`) with `Unload(ctx) error` (locks `mu`, calls `run(ctx, "stop")`, wraps errors as `fmt.Errorf("systemctl stop %s: %w", u.unit, err)`) and `Reload(ctx) error` (locks `mu`, calls `run(ctx, "start")`, wrapped analogously) — each holds `mu` only for the duration of its own call, so `Unload` and `Reload` on the same instance can never overlap. Add `newSystemdUnitController(unit string) *systemdUnitController`, whose `run` closure calls `exec.CommandContext(ctx, "systemctl", verb, unit).Run()` — the only place `os/exec` is imported in this file.
**Files**: `internal/backend/openai_backend.go`
**Test**: `go build ./internal/backend` compiles; `grep -n "func (u \*systemdUnitController)" internal/backend/openai_backend.go` shows both `Unload` and `Reload` methods.
**Depends on**: Step 1 (ADR ordering constraint — this is the core mechanism code the ADR governs).
**Parallelizable**: No.

### Step 8: Wire systemdUnitController into newOpenAIBackend and fix Unloader()
**What**: Add an `unloader yield.Unloader` field to `openaiBackend`. In `newOpenAIBackend`, set it to `nil` (interface zero value) when `cfg.UpstreamUnitName == ""`, else `newSystemdUnitController(cfg.UpstreamUnitName)`. Change `Unloader()` from a hardcoded `return nil` to `return b.unloader`, and update its doc comment to describe both branches and reference ADR-0014 instead of the stale plan-doc path the current comment cites.
**Files**: `internal/backend/openai_backend.go`
**Test**: `go build ./...` compiles; `grep -n "func (b \*openaiBackend) Unloader" -A3 internal/backend/openai_backend.go` shows `return b.unloader`.
**Depends on**: Step 2 (needs `cfg.UpstreamUnitName`), Step 4 (needs the widened `yield.Unloader` interface), Step 7 (needs `systemdUnitController` to exist).
**Parallelizable**: No.

### Step 9: Verify the existing nil-case Unloader tests still pass unchanged
**What**: Run the two existing tests that already cover the unset/empty/whitespace-only case — `TestOpenAIBackendUnloaderIsTrueNilInterface` and `TestOpenAIBackendUnloaderDoesNotTriggerDoUnload` in `internal/backend/openai_backend_test.go` — and confirm they pass unmodified against the new two-branch `Unloader()`. Do not write new tests for this case; these two already satisfy FR13/AC1/AC2.
**Files**: `internal/backend/openai_backend_test.go` (verification only — no edits expected)
**Test**: `go test ./internal/backend -v -run 'TestOpenAIBackendUnloaderIsTrueNilInterface|TestOpenAIBackendUnloaderDoesNotTriggerDoUnload'` passes with both tests green and no code changes needed to make them pass.
**Depends on**: Step 8 (the real two-branch `Unloader()` must exist to verify against).
**Parallelizable**: No.

### Step 10: Add systemdUnitController unit tests
**What**: Add to `internal/backend/openai_backend_test.go`: `TestOpenAIBackendUnloaderNonNilWhenUnitSet` (`cfg.UpstreamUnitName = "vllm"` → `Unloader()` non-nil); `TestSystemdUnitControllerUnloadRunsStop` and `TestSystemdUnitControllerReloadRunsStart` (stub `run` recording the verb, asserting `"stop"`/`"start"` — never `"restart"`); `TestSystemdUnitControllerErrorPropagates` (stub `run` returning an error, asserting `Unload`/`Reload` return a non-nil wrapped error without panicking); `TestSystemdUnitControllerSerializesUnloadAndReload` (stub `run` that blocks until signaled, drive concurrent `Unload`/`Reload` from two goroutines, assert they never execute inside the stub simultaneously). No test spawns a real `systemctl` process or calls `/sleep`/`/wake_up`.
**Files**: `internal/backend/openai_backend_test.go`
**Test**: `go test ./internal/backend -v -run 'TestOpenAIBackendUnloaderNonNilWhenUnitSet|TestSystemdUnitController'` passes all five tests (satisfies AC3–AC8, AC10).
**Depends on**: Step 8 (implementation must be complete).
**Parallelizable**: No.

### Step 11: Add the factory-wiring regression test
**What**: Add `TestOpenAIBackendFactoryWiresSystemdController` to `internal/backend/openai_backend_test.go`: build a `Config` with `UpstreamUnitName` set, construct the backend via the real `newOpenAIBackend(cfg)` factory (not a hand-built `&openaiBackend{...}` literal), type-assert `Unloader()` to `*systemdUnitController`, and prove its `run` field is actually populated (not nil/zero-valued) — e.g. by substituting `run` post-construction and invoking `Unload`/`Reload` through the object the factory returned. This is the only test shape that catches a forgotten/nil command-runner field left unwired in the real constructor.
**Files**: `internal/backend/openai_backend_test.go`
**Test**: `go test ./internal/backend -v -run TestOpenAIBackendFactoryWiresSystemdController` passes (satisfies AC9). Confirm by temporarily reverting Step 8's wiring line locally and observing this specific test fail while the Step 10 stub-based tests would not — then restore the fix.
**Depends on**: Step 8 (real factory wiring must exist to test against).
**Parallelizable**: No.

### Step 12: Update README, deploy/broker.service, and the broker-config-and-flags SKILL together
**What**: Add `UPSTREAM_UNIT_NAME` to all three docs in one pass, since each is a small, mechanical, single-row change: (1) `README.md`'s "### Configuration (env)" table, directly after `UPSTREAM_API_KEY`, describing the symmetric stop-on-yield/start-on-clear behavior (not "restart") and the disabled-by-default state; (2) `deploy/broker.service`, a commented-out `#Environment=UPSTREAM_UNIT_NAME=<systemd unit name>` line near the upstream/openai-backend block, following the same commented-out-until-provisioned convention as `PLEX_URL`/`PLEX_TOKEN`; (3) `.claude/skills/broker-config-and-flags/SKILL.md`'s env var table, describing the stop-on-yield/start-on-clear behavior and referencing ADR-0014.
**Files**: `README.md`, `deploy/broker.service`, `.claude/skills/broker-config-and-flags/SKILL.md`
**Test**: `grep -n "UPSTREAM_UNIT_NAME" README.md deploy/broker.service .claude/skills/broker-config-and-flags/SKILL.md` shows a row/line in all three files, each describing stop/start (not restart); visual check confirms the SKILL.md row references ADR-0014 (satisfies FR14–16 / AC15–17).
**Depends on**: Step 1 (ADR number 0014 must exist to reference), Step 8 (final stop/start behavior must be implemented as described).
**Parallelizable**: No.

### Step 13: Full-suite verification
**What**: Run the complete test and vet suite and confirm no regressions versus `main`.
**Files**: None (verification only).
**Test**: `go test ./...` passes for every package except the pre-existing, unrelated `internal/admin` failure (unchanged, out of scope — do not touch it); `go vet ./...` reports no new findings versus `main`. Cross-check every requirements.md functional requirement (FR1–FR20) and acceptance criterion (AC1–AC20) against the steps above; every one must map to at least one completed step.
**Depends on**: Steps 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12 (everything must be in place).
**Parallelizable**: No.

## Rollback plan
All steps are additive (new fields, new methods, new files, new test cases, new doc rows) and reversible via `git revert`/`git checkout` on the specific files touched.

The riskiest step is the combination of **Step 7 + Step 8** (the `internal/yield/yield.go` interface widening in Step 4 plus the `openai_backend.go` core change): this is the step that changes real runtime behavior for any deployment where `UPSTREAM_UNIT_NAME` is set. If this ships and misbehaves on a live desktop — e.g. `systemctl stop`/`start` firing at the wrong times, or a hung/flapping unit — the immediate **operator mitigation, no code rollback required**, is to unset `UPSTREAM_UNIT_NAME` in the deployed environment and restart the broker service; `Unloader()` then returns a literal nil again and the openai backend reverts to today's always-nil no-op behavior. A full code rollback (`git revert` of Steps 4/5/7/8) is only needed if the bug also affects the nil/unset path (e.g. a typed-nil regression), which the Step 9 and Step 11 tests are specifically designed to catch before merge.
