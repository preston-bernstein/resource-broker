# Tasks: vLLM/OpenAI-backend VRAM-yield Unloader

Generated from: docs/vllm-yield-unloader/ on 2026-08-15

## Status legend
- [ ] pending
- [>] in progress
- [x] done
- [!] blocked

## Tasks

### Task 1: Write ADR-0014 (symmetric stop/start decision)
**Status**: [x] done
**Files**: docs/adr/0014-vllm-yield-symmetric-stop-start.md
**Test**: Read the ADR and confirm it contains: (1) the stop/start decision with explicit references to ADR-0003 and ADR-0004; (2) rejected-alternative sections for both `systemctl restart` (VRAM recontention after eager reload) and vLLM `/sleep`+`/wake_up` (citing vllm-project/vllm#20627); (3) a security-scope recommendation for a unit-specific (never wildcard) sudoers/polkit grant; (4) named accepted costs (connection errors during stop/start window) and named deliberate scope cuts (no preflight check, no new Prometheus metric).
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 2: Add UpstreamUnitName to config.Load()
**Status**: [x] done
**Files**: internal/config/config.go
**Test**: `go build ./internal/config` compiles; `grep -n "UpstreamUnitName" internal/config/config.go` shows both the struct field and the `Load()` assignment using `strings.TrimSpace`.
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 3: Add config_test coverage for UPSTREAM_UNIT_NAME
**Status**: [x] done
**Files**: internal/config/config_test.go
**Test**: `go test ./internal/config -v -run TestLoad` passes, covering unset, whitespace-only, and set-with-surrounding-whitespace states.
**Depends on**: Task 2
**Parallelizable**: no
**Notes**:

### Task 4: Widen yield.Unloader to add Reload, and fix the yield package's test doubles in the same step
**Status**: [x] done
**Files**: internal/yield/yield.go, internal/yield/yield_test.go
**Test**: `go test ./internal/yield -v` passes with no compile errors.
**Depends on**: Task 1
**Parallelizable**: no
**Notes**:

### Task 5: Add no-op Reload to ollama.Client
**Status**: [x] done
**Files**: internal/ollama/client.go
**Test**: `go build ./...` compiles; new test asserts `client.Reload(ctx)` returns nil without a network call or command invocation.
**Depends on**: Task 4
**Parallelizable**: no
**Notes**:

### Task 6: Fix backend.go's Unloader() doc comment
**Status**: [x] done
**Files**: internal/backend/backend.go
**Test**: Read the updated comment; confirms typed-nil invariant without implying every backend always takes the nil path.
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 7: Define systemdUnitController and its Unload/Reload methods
**Status**: [x] done
**Files**: internal/backend/openai_backend.go
**Test**: `go build ./internal/backend` compiles; `grep -n "func (u \*systemdUnitController)" internal/backend/openai_backend.go` shows both Unload and Reload methods.
**Depends on**: Task 1
**Parallelizable**: no
**Notes**:

### Task 8: Wire systemdUnitController into newOpenAIBackend and fix Unloader()
**Status**: [x] done
**Files**: internal/backend/openai_backend.go
**Test**: `go build ./...` compiles; `grep -n "func (b \*openaiBackend) Unloader" -A3 internal/backend/openai_backend.go` shows `return b.unloader`.
**Depends on**: Task 2, Task 4, Task 7
**Parallelizable**: no
**Notes**:

### Task 9: Verify the existing nil-case Unloader tests still pass unchanged
**Status**: [x] done
**Files**: internal/backend/openai_backend_test.go (verification only)
**Test**: `go test ./internal/backend -v -run 'TestOpenAIBackendUnloaderIsTrueNilInterface|TestOpenAIBackendUnloaderDoesNotTriggerDoUnload'` passes with both tests green, no code changes needed.
**Depends on**: Task 8
**Parallelizable**: no
**Notes**:

### Task 10: Add systemdUnitController unit tests
**Status**: [x] done
**Files**: internal/backend/openai_backend_test.go
**Test**: `go test ./internal/backend -v -run 'TestOpenAIBackendUnloaderNonNilWhenUnitSet|TestSystemdUnitController'` passes all five new tests.
**Depends on**: Task 8
**Parallelizable**: no
**Notes**:

### Task 11: Add the factory-wiring regression test
**Status**: [x] done
**Files**: internal/backend/openai_backend_test.go
**Test**: `go test ./internal/backend -v -run TestOpenAIBackendFactoryWiresSystemdController` passes.
**Depends on**: Task 8
**Parallelizable**: no
**Notes**:

### Task 12: Update README, deploy/broker.service, and the broker-config-and-flags SKILL together
**Status**: [x] done
**Files**: README.md, deploy/broker.service, .claude/skills/broker-config-and-flags/SKILL.md
**Test**: `grep -n "UPSTREAM_UNIT_NAME" README.md deploy/broker.service .claude/skills/broker-config-and-flags/SKILL.md` shows a row/line in all three, each describing stop/start (not restart); SKILL.md references ADR-0014.
**Depends on**: Task 1, Task 8
**Parallelizable**: no
**Notes**:

### Task 13: Full-suite verification
**Status**: [x] done
**Files**: none (verification only)
**Test**: `go test ./...` passes except the pre-existing unrelated internal/admin failure; `go vet ./...` no new findings vs main; every FR1-FR20 and AC1-AC20 maps to a completed task.
**Depends on**: Task 2, Task 3, Task 4, Task 5, Task 6, Task 7, Task 8, Task 9, Task 10, Task 11, Task 12
**Parallelizable**: no
**Notes**:

## Blocked / open
(none yet)
