---
name: broker-validation-and-qa
description: What counts as evidence in the ollama-resource-broker repo — the validation commands (go test/vet/race), the full per-package test inventory with the techniques each uses, how to write a new test in house style, which scheduler/Job invariants are pinned by which tests, known gaps (internal/admin broken at HEAD, internal/schedule and internal/tdarr untested), and soak criteria for operational changes. Load when writing or reviewing tests, before claiming "this change is safe", when asked "how do I test this here", "is there a test for preemption/requeue/idempotency", "why does go test fail", or when adding a package that needs its first tests. NOT for running the deployed service (broker-run-and-operate) or measuring it (broker-diagnostics-and-tooling).
---

# Broker validation and QA

"It looks right" is not evidence in this repo. The review culture grades findings H/M/L and fixes them in dedicated commits (`git log --oneline | grep -i review`). This skill defines the evidence bar and maps every invariant to the test that pins it.

## 1. The evidence bar

A change is validated when ALL that apply pass:

| Requirement | Command | When |
|---|---|---|
| Unit suite green | `go test ./...` | Every change, before every commit |
| Vet clean | `go vet ./...` | Every change (vet compiles tests — it catches the class of breakage live at HEAD) |
| Race detector | `go test -race ./...` | Anything touching `internal/queue`, `internal/yield`, `internal/job` — all are goroutine+mutex heavy (scheduler slot handoff, serve-context swap, worker/monitor goroutines) |
| New discriminating test | fails before your change, passes after | Every behavior change |
| Soak | multi-day live observation | Operational/deploy changes — see §6 |

Known state as of 2026-07-02: `go test ./...` and `go vet ./...` FAIL at HEAD in `internal/admin` — `admin_test.go:29-31` calls `Mux` with 5 args; the signature grew to 6 (`Mux(Controller, StatsProvider, http.Handler, http.Handler, func() any, TdarrStatusFn)`) in commit dd39d20 without a test update. Every other package passes. Fixing this is Phase 1 of `broker-cutover-hardening-campaign`; until then, "suite green" means "green except the known admin failure" — never let a NEW failure hide behind it.

## 2. Test inventory (what exists, what technique to copy)

Read the file before copying its pattern. As of 2026-07-02:

| Package | Files | Covers | Techniques worth copying |
|---|---|---|---|
| `internal/config` | config_test.go | defaults, overrides, invalid URL/duration/int, INFINITY_URL optionality | `t.Setenv` style env manipulation; one test per failure class |
| `internal/detect` | detect_test.go | each detection rule; lister error fails OPEN | fake `Lister` returning canned `[]Process` |
| `internal/yield` | yield_test.go | mode overrides, polling loop, transition cancels serve-context + calls Unloader | fake Detector/Unloader structs; asserting on context cancellation |
| `internal/queue` | scheduler, scheduler_inflight, scheduler_cap, gate, gate_wait, gate_yield, gate_cancel tests | concurrency-1 serialization, interactive priority, waiter removal on ctx cancel, MaxInflight, InteractiveWaiting signal, queue-full 503, wait-budget 503, yield refusal, in-flight cancel on yield | `httptest.NewServer` around `s.Gate(...)` with `alwaysServe{}`/`alwaysYield{}` fake Admissions; blocking upstream handlers gated on channels |
| `internal/job` | sqlite, worker, api, events (via api) tests | idempotent submit, claim order, **preempt→front position 1 + attempts==0**, retry cap, **restart recovery (RUNNING→QUEUED, attempts++, cap→FAILED)**, cancel, retain-until-fetched prune, counts; worker success/retry/cancel/**gaming preempt (no attempt burned)**/**interactive preempt past quantum**; API status codes incl. missing Idempotency-Key→400, result 409 before ready | `OpenSQLite(":memory:")` via `newStore(t)` helper with `t.Cleanup`; `newRig(t, gen, attempts, quantum)` worker harness with fake Generator (`blockGen`); `waitState` polling helper; a shared `do()` HTTP helper for API tests |
| `internal/ollama` | client, generate tests | model list+unload calls, NDJSON stream accumulation, upstream error, context cancel | `httptest.NewServer` fake Ollama emitting NDJSON |
| `internal/proxy` | proxy_test.go | streaming is NOT buffered (flush-observed), request forwarding, **embed path rewrite /embeddings→/embeddings_image** | flusher-aware upstream; asserting per-chunk arrival timing |
| `internal/metrics` | metrics_test.go | counters + gauges render | recorder → `/metrics` scrape → substring asserts |
| `internal/admin` | admin_test.go | healthz, POST /control valid/invalid/bad-JSON, status | BROKEN at HEAD (see §1) |
| `internal/schedule` | — | **NOTHING** | first test should pin window boundaries (Fri 01:59/02:00/06:59/07:00 for estate-scraper) — read `contains()` first; mind `t.Local()` |
| `internal/tdarr` | — | **NOTHING** | first test: `httptest` fake Tdarr node API asserting PauseGPU/ResumeGPU request bodies and WorkerLimits parsing |
| `cmd/broker` | — | no test files (wiring only) | integration coverage comes from package tests |

## 3. Invariant → pinning test map

From `broker-architecture-contract`; verify a change against these before touching scheduler/Job code:

| Invariant | Pinned by |
|---|---|
| Concurrency cap / one-at-a-time default | `TestSingleConcurrency`, `TestMaxInflightConcurrency` |
| Interactive before batch, FIFO within class | `TestInteractivePriority` |
| Yield refuses new + cancels in-flight | `TestGateRefusesWhenYielding`, `TestGateCancelsInFlightOnYield`, `TestYieldTransitionCancelsServeAndUnloads` |
| Wait budget → 503 | `TestGateWaitBudgetExceeded` |
| Waiter-cap fast 503 | `TestQueueFull` |
| Preempted Job requeues at FRONT, resume-first | `TestPreemptGoesToFront` |
| Clean preempt never burns an attempt | `TestPreemptGoesToFront` (store), `TestWorkerGamingPreempt` (worker) |
| Restart: RUNNING→QUEUED@front, attempts++, cap→FAILED | `TestRecoverRunning`, `TestRecoverRunningCap` |
| Quantum: interactive preempts a Job only past min-run | `TestWorkerInteractivePreemptPastQuantum` |
| Submit idempotency | `TestSubmitIdempotent`, `TestAPISubmitIdempotent` |
| Retain-until-fetched pruning | `TestStampFetchedAndPrune` |
| Streaming never buffered | `TestStreamingNotBuffered` |
| Embed path rewrite | `TestEmbedRewritesEmbeddingsPath` |
| Detection fails open | `TestDetectListerErrorFailsOpen` |

Gaps (no direct pinning test as of 2026-07-02): the `X-Broker-Status` TRAILER value on preemption (gate sets it; no test asserts the trailer reaches a client), embed-lane requests recording under `class="batch"` in metrics, schedule window boundaries, Tdarr pause/resume wire format. Adding any of these is a welcome, low-risk contribution.

## 4. How to add a test (house style)

Observed conventions — match them:

- Same package (white-box: `package queue`, not `queue_test`), stdlib + `httptest` only; go.mod has NO test-only dependencies. Do not add testify or mocks libraries.
- Fakes are tiny inline types in the test file (`fakeCtrl`, `alwaysYield{}`, `blockGen`), not generated mocks.
- Helpers take `t *testing.T`, call `t.Helper()`, fail with `t.Fatalf` (e.g. `newStore`, `submit`, `do`, `waitState`).
- SQLite in tests: `OpenSQLite(":memory:")` + `t.Cleanup(close)`.
- Timing: tests are channel-synchronized where correctness matters (see `TestWorkerInteractivePreemptPastQuantum` — a goroutine holds the slot until the assertion lands, precisely to avoid a race with the worker). Small `time.Sleep` (1–50ms) appears only as a scheduling nudge in polling helpers (`waitState`) and to let waiters park — acceptable for nudges, NOT for correctness assertions. Do not add sleeps that a channel or context could replace.
- Naming: `Test<Component><Behavior>` (`TestGateRefusesWhenYielding`), one behavior per test.

Minimal example for a new config var, matching `config_test.go` style:

```go
func TestLoadMyNewVar(t *testing.T) {
    t.Setenv("BROKER_MY_NEW_VAR", "42")
    cfg, err := Load()
    if err != nil { t.Fatalf("load: %v", err) }
    if cfg.MyNewVar != 42 { t.Fatalf("MyNewVar = %d, want 42", cfg.MyNewVar) }
}
```

## 5. What evidence a claim requires

| Claim | Required evidence |
|---|---|
| "This scheduler change is safe" | invariant map tests green + `-race` + a new test for the changed behavior |
| "This fixes the bug" | a test that reproduces the bug and fails on the parent commit |
| "This config default is right" | rationale in the commit/ADR + README/deploy updated (see `broker-config-and-flags` checklist) — the 5s batch-wait default was "right" until real ingestion hit it |
| "Performance improved" | numbers from `broker-diagnostics-and-tooling` instruments, before AND after, same workload |
| "Ready to deploy" | all of the above + soak plan (§6) |

## 6. Soak criteria (operational changes)

A deploy-affecting change (binary upgrade, unit change, retiring the legacy daemon) needs a live soak, watching via `broker-diagnostics-and-tooling`:

- Duration: multi-day, MUST include at least one gaming session, one Plex transcode, and one Friday 02:00–07:00 estate-scraper window.
- Green means: `yield start`/`yield stop` pairs present in journalctl for each contention event; detection-to-yield latency within ~2× `BROKER_DETECT_INTERVAL`; no unexplained growth in `broker_requests_total{outcome="deferred"}` during idle hours; `broker_jobs{state="failed"}` stable; no restart loops (`systemctl status` restart count).
- The v2 cutover itself never completed its soak (legacy daemon still live as of 2026-07-02) — do not repeat that; a soak is finished when its exit criteria are written down BEFORE it starts and then met.

## When NOT to use this skill

- Running or deploying the service → `broker-run-and-operate`.
- Reading gauges/metrics → `broker-diagnostics-and-tooling`.
- Whether a change needs an ADR or review → `broker-change-control`.
- Why an invariant exists → `broker-architecture-contract`.

## Provenance and maintenance

Verified 2026-07-02 on branch `v2-go` by reading every `*_test.go` and running the suite. Re-verify:

```sh
ls /Users/prestonbernstein/dev/ollama-resource-broker/internal/*/*_test.go
cd /Users/prestonbernstein/dev/ollama-resource-broker && go test ./... 2>&1 | tail -5   # admin failure known until campaign P1
grep -rn "func Test" /Users/prestonbernstein/dev/ollama-resource-broker/internal | wc -l
grep -c require /Users/prestonbernstein/dev/ollama-resource-broker/go.mod              # still no test-only deps
```
