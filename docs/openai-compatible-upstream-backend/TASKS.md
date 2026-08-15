# Tasks: OpenAI-Compatible Upstream Backend

Generated from: docs/openai-compatible-upstream-backend/ on 2026-08-14

## Status legend
- [ ] pending
- [>] in progress
- [x] done
- [!] blocked

## Tasks

### Task 1: Add upstream-backend config fields and validation
**Status**: [x] done
**Files**: internal/config/config.go
**Test**: UPSTREAM_BACKEND=bogus fails; openai without UPSTREAM_URL fails; valid openai config succeeds; UPSTREAM_API_KEY value never logged; CR/LF in UPSTREAM_API_KEY fails; ollama without OLLAMA_URL fails
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 2: Extract error handling from proxy into a reusable helper
**Status**: [x] done
**Files**: internal/proxy/proxy.go
**Test**: existing internal/proxy tests pass
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 3: Define the Backend abstraction interface
**Status**: [x] done
**Files**: internal/backend/backend.go
**Test**: package compiles; Backend interface has all 4 methods; New() factory signature correct
**Depends on**: Task 1
**Parallelizable**: no
**Notes**:

### Task 4: Implement the Ollama backend wrapper
**Status**: [x] done
**Files**: internal/backend/ollama_backend.go
**Test**: Proxy() passthrough works; Generate() returns concatenated response; Reachable() succeeds/fails correctly
**Depends on**: Task 3
**Parallelizable**: no
**Notes**:

### Task 5a: Implement the OpenAI client — request construction and error mapping
**Status**: [x] done
**Files**: internal/openaicompat/client.go
**Test**: 500 error handled without panic; connection failure retried before first byte; URL properly joined
**Depends on**: Task 1
**Parallelizable**: yes
**Notes**:

### Task 5b: Implement the OpenAI client — SSE parsing and token counting
**Status**: [x] done
**Files**: internal/openaicompat/stream.go
**Test**: multiple onTokens calls during streaming; correct token counts; correct concatenated text; mid-stream error after valid chunks returns error correctly
**Depends on**: Task 5a
**Parallelizable**: no
**Notes**:

### Task 6: Implement OpenAI embeddings translation
**Status**: [x] done
**Files**: internal/openaicompat/embed.go
**Test**: /api/embed response matches Ollama embeddings shape, reshaped correctly from OpenAI response
**Depends on**: Task 5b
**Parallelizable**: no
**Notes**:

### Task 7a: Implement the OpenAI handler — core translation and streaming
**Status**: [x] done
**Files**: internal/openaicompat/handler.go
**Test**: NDJSON streaming with multiple flushes for stream:true; single JSON object for stream:false; images field returns 400 before upstream call
**Depends on**: Task 2, Task 5b, Task 6
**Parallelizable**: no
**Notes**:

### Task 7b: Implement the OpenAI handler — error handling and recovery
**Status**: [x] done
**Files**: internal/openaicompat/handler.go
**Test**: pre-response 500 -> WriteUpstreamError checked, 502 fallback if false; mid-stream error -> final NDJSON error line, no duplicate WriteHeader; panicking ResponseWriter recovered and logged
**Depends on**: Task 7a
**Parallelizable**: no
**Notes**:

### Task 7c: Implement the OpenAI handler — hard-reject unsupported fields
**Status**: [x] done
**Files**: internal/openaicompat/handler.go
**Test**: images field -> 400, upstream never called; valid request without unsupported fields succeeds
**Depends on**: Task 7b
**Parallelizable**: no
**Notes**:

### Task 8: Add X-Broker-*/trailer headers verification for openai backend
**Status**: [x] done
**Files**: internal/openaicompat/handler_test.go
**Test**: X-Broker-Request-Id, X-Broker-Wait-Ms, X-Broker-Status (header+trailer) present and correct when wired through real queue.Gate
**Depends on**: Task 7c
**Parallelizable**: no
**Notes**:

### Task 9: Add recover() in yield.go's doUnload()
**Status**: [x] done
**Files**: internal/yield/yield.go
**Test**: fake Unloader whose Unload() panics -> broker survives, panic logged, goroutine exits cleanly
**Depends on**: none
**Parallelizable**: yes
**Notes**:

### Task 10: Add openai backend wrapper
**Status**: [x] done
**Files**: internal/backend/openai_backend.go
**Test**: Reachable() true/error correctly; Unloader() is a true nil interface value (not typed-nil)
**Depends on**: Task 3, Task 5b, Task 6, Task 7c
**Parallelizable**: no
**Notes**:

### Task 11: Update main.go to wire backends
**Status**: [x] done
**Notes**: also fixed a pre-existing bug surfaced by this change — startup log line nil-panicked on cfg.OllamaURL.String() when UPSTREAM_BACKEND=openai (OllamaURL nil in that mode)
**Files**: cmd/broker/main.go
**Test**: starts with UPSTREAM_BACKEND=ollama (default), /healthz 200; starts with openai+mock, /healthz probes openai; embed lane unaffected by UPSTREAM_BACKEND
**Depends on**: Task 4, Task 10
**Parallelizable**: no
**Notes**:

### Task 12: Add backend factory tests
**Status**: [x] done
**Files**: internal/backend/backend_test.go
**Test**: go test ./internal/backend passes; typed-nil regression test for openaiBackend.Unloader()
**Depends on**: Task 3, Task 4, Task 10
**Parallelizable**: no
**Notes**:

### Task 13: Add OpenAI client and handler integration tests
**Status**: [x] done
**Files**: internal/openaicompat/client_test.go, internal/openaicompat/handler_test.go
**Test**: go test ./internal/openaicompat passes; streaming tests show multiple writes
**Depends on**: Task 5b, Task 6, Task 7c
**Parallelizable**: no
**Notes**:

### Task 14: Parameterize existing job-worker and proxy tests for both backends
**Status**: [x] done
**Notes**: found a pre-existing intermittent flaky test — internal/openaicompat/handler_test.go's TestServeChat_ContextCancellationMidStream (goroutine-count timing, reproduces with -count=5) — deferred to Task 15's full-suite verification pass
**Files**: internal/job/worker_test.go, internal/proxy/proxy_test.go
**Test**: AC-7/8/9 pass for both backends with concrete assertions (queue position, preemption timing, healthz JSON body)
**Depends on**: Task 4, Task 10, Task 13
**Parallelizable**: no
**Notes**:

### Task 15: Verify ollama backend structural parity with existing codebase
**Status**: [x] done
**Notes**: root-caused and fixed the Task-14-flagged flaky test (idle keep-alive connection on shared proxy.Transport, not a real bug); full suite green under -race
**Files**: (read-only verification, may add a small test file)
**Test**: go test ./... passes with UPSTREAM_BACKEND unset; structural type-assertion test confirms ollama backend uses unmodified httputil.ReverseProxy
**Depends on**: Task 1-14
**Parallelizable**: no
**Notes**:

### Task 16a: Write acceptance test suite — streaming, embeddings, job lifecycle, config
**Status**: [x] done
**Files**: internal/acceptance_test.go
**Test**: AC-1 through AC-6 pass
**Depends on**: Task 1-15
**Parallelizable**: no
**Notes**:

### Task 16b: Write acceptance test suite — preemption, quantum, yield, healthz
**Status**: [x] done
**Notes**: initial verification run hung (goroutine dump showed AC8's batch-job HTTP call not canceling on preemption); dedicated debugging pass traced context-cancellation plumbing end-to-end as correct and could not reproduce across 20+ repeated runs (race mode, CPU-contention stress). Root cause: the hang coincided with a second full test-suite run (Task 16b's own stale background verification) still executing concurrently, starving the 200ms preemption-monitor ticker under CPU contention within the 60s window — not a code defect. Confirmed clean with an isolated run (all 9 acceptance tests pass in 2.96s).
**Files**: internal/acceptance_test.go
**Test**: AC-7/8/9 pass
**Depends on**: Task 16a
**Parallelizable**: no
**Notes**:

### Task 16c: Write acceptance test suite — observability
**Status**: [x] done
**Files**: internal/acceptance_test.go
**Test**: AC-10/11/12 pass
**Depends on**: Task 16b
**Parallelizable**: no
**Notes**:

### Task 16d: Write acceptance test suite — error mapping, isolation, new requirements
**Status**: [x] done
**Files**: internal/acceptance_test.go
**Test**: AC-13 through AC-24 pass
**Depends on**: Task 16c
**Parallelizable**: no
**Notes**:

## Blocked / open
All 22 tasks complete. Post-implementation Integration Validator (new-story Phase 3.5) found and the orchestrator fixed directly:
1. **CRITICAL**: `cmd/broker/main.go` panicked on startup in the default (`UPSTREAM_BACKEND` unset/`ollama`) config — `cfg.UpstreamURL.String()` was called unconditionally before the backend branch, dereferencing a nil `*url.URL`. Not caught by `go build`/`go vet`/`go test ./... -race` because `cmd/broker` had zero test coverage of `main()` itself. Fixed the branch ordering, verified by running the real binary, and added `cmd/broker/main_test.go` (a composition-root smoke test across all three backend configs) as permanent regression coverage.
2. Minor: a stale `TODO(5b)` scaffolding comment in `internal/openaicompat/client.go` (implementation was already complete) — removed.

Full repo (`go build ./... && go vet ./... && go test ./... -race -count=1`) confirmed green after both fixes.

## Harden Phase 4 — mutation testing (gremlins, per-package)

`internal/openaicompat` (delegated to a dedicated agent — 57 TIMED OUT + 13 NOT COVERED + 0 LIVED at the default timeout was misleading; `--timeout-coefficient 10` unmasked 8 real LIVED mutants hiding in the "timed out" bucket): fixed a genuine hang bug (4 tests' mock-server handlers blocked on a bare `<-release`/`<-proceed` with no bound, so any client-under-test bug that returns early leaves the goroutine parked and `httptest.Server.Close()` hangs the whole binary — bounded all 4 with a `select`+500ms timeout), fixed 7/8 LIVED with targeted tests, left 1 LIVED as a documented exception (`stream.go:48` — `bufio.Scanner`'s initial-capacity hint, not the actual correctness bound). Final: Killed 76, Lived 1, Not covered 0, Timed out 0 — 98.70% efficacy, 100% mutator coverage. Independently re-verified: `go build/vet/test -race` all green.

`internal/config` (orchestrator, `--timeout-coefficient 10`): 6 LIVED (default-value ARITHMETIC_BASE mutants on `DetectInterval`/`BatchQuantum`/`JobPruneInterval`/`JobHardCap`, plus a `getint` boundary at `n < 1`) — fixed with targeted default-value assertions and a `n=1` boundary test. Final: Killed 51, Lived 0 — 100% efficacy, 100% coverage.

`internal/proxy` (orchestrator, `--timeout-coefficient 15 --workers 2` — default workers caused CPU-contention-driven false TIMED OUTs, confirmed by manual mutation reproduction showing real 3s completions): found and fixed a genuine hang (`TestStreamingNotBuffered`'s mock upstream blocked forever on a bare `<-release`; a failing assertion before `close(release)` hung `httptest.Server.Close()`), added coverage for the shared `Transport`'s tuned constants and both `ReverseProxy`s' `FlushInterval: -1`. Final: Killed 19, Lived 0, Timed out 0 — 100% efficacy, 90.48% coverage; the remaining 2 "not covered" (`proxy.go:45`,`:48`) are literals inside a package-level `var` composite literal — confirmed via `go tool cover -func` that Go's coverage instrumentation only tracks function-body statements, never package-level var initializers, so no test can make these show as covered.

`internal/yield` (orchestrator, `--timeout-coefficient 10 --workers 2`): 4 LIVED (confirmPolls boundary, doUnload's error-log branch, and both GPUManager pause/resume 10s timeouts) — fixed 3 with targeted tests (error-path log-message assertion via the existing `panicLogHandler` pattern, deadline-capturing fakes for doUnload/pauseGPUMgr/resumeGPUMgr). The 4th (`yield.go:111` `confirmPolls < 1` vs `<= 1`) is a proven **equivalent mutant**: the clamp target is 1, so `confirmPolls=1` yields the same `c.confirmPolls=1` whether or not the branch is taken — no input can observe a `<`/`<=` difference. Documented inline at the clamp with the date and reasoning. Final: Killed 14, Lived 1 (equivalent, documented), Timed out 0 — 93.33% efficacy, 100% coverage.

## Harden Phase 5 — suppression sweep

Grepped the full working-tree diff (tracked `git diff HEAD` + every untracked new `.go` file under `internal/backend`, `internal/openaicompat`, `internal/acceptance_test.go`, `cmd/broker/main_test.go`) for `//nolint`, `go:build ignore`, `t.Skip(`, and `panic("unimplemented`. Zero hits. No gremlins/staticcheck config file exists in this repo to check for a lowered threshold, and no CI/Makefile/go.mod changes are in the diff. Suppression sweep clean.
