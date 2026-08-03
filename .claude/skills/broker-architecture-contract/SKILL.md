---
name: broker-architecture-contract
description: >
  Load before changing, reviewing, or reasoning about the ollama-resource-broker's
  design: scheduler/queue behavior, yield/preemption semantics, the durable Job
  system, the embed lane, priority classes, or anything an ADR governs. Answers
  "why is it built this way", "can I change X without breaking the contract",
  "what invariant does this code enforce", and "what are the known weaknesses".
  Triggers: modifying internal/queue, internal/yield, internal/job, internal/proxy,
  cmd/broker; questions about preemption, quantum, yield, 503/Retry-After,
  Idempotency-Key, requeue-at-front, X-Broker-Status trailer, MaxInflight,
  embed lane / Infinity path rewrite; design review of any PR touching scheduling
  or durability. NOT for env-var values (broker-config-and-flags) or generic GPU
  theory (gpu-arbitration-reference).
---

# Broker architecture contract

The load-bearing decisions of the ollama-resource-broker (`/Users/prestonbernstein/dev/ollama-resource-broker`, branch `v2-go`), the invariants that must survive any change, and the known-weak points as of 2026-07-02.

The Broker is a single Go binary that fronts Ollama (and optionally an Infinity embedding server) so that one GPU can be shared, in strict priority order: **gaming/Plex > interactive inference > batch inference**. Vocabulary is enforced by `CONTEXT.md` at the repo root (Broker, Contention, Yield, Preemption, Job, Queue, Position, Fronting Proxy, Synchronous request, Embed lane, Consumer — each with an Avoid list). Use those terms exactly; a review will flag "gateway", "pause", "kill", "task", "client".

Read this skill top to bottom before proposing any change to `internal/queue`, `internal/yield`, `internal/job`, `internal/proxy`, or `cmd/broker/main.go`. If your change would violate a row in the invariants table, the change is wrong or it needs a new ADR first (see broker-change-control).

## Invariants

Every row is verified against source at the stated anchor (2026-07-02). If you touch the anchor, re-check the row.

| # | Invariant | Enforced where | ADR | What breaks if violated |
|---|-----------|----------------|-----|-------------------------|
| 1 | **Yield to gaming/Plex is the only law.** Concurrency-1 (`BROKER_MAX_INFLIGHT=1`) is a conservative *default*, not a law — ADR-0004 says so verbatim: "Concurrency-1 is a conservative default, not a law; the only law is yielding to gaming." | `internal/yield/yield.go` (`Controller`), `internal/queue/scheduler.go` (`SetMaxInflight`) | 0004 | Treating concurrency-1 as sacred blocks legitimate tuning; treating yield as tunable lets inference stutter games — the one outcome the whole project exists to prevent. |
| 2 | **Yield is hard: cancel in-flight AND force VRAM unload. Never graceful drain.** On the transition into yielding, `Controller.applyLocked` calls `c.serveCancel()` (aborts every in-flight upstream call via the serve context) and spawns `c.doUnload()` (the `Unloader`, i.e. `ollama.Client.Unload`, keep_alive=0). ADR-0003 explicitly rejected graceful drain ("letting a 70b generation finish could stall the game for minutes") and cancel-but-leave-loaded (VRAM stays occupied). | `internal/yield/yield.go` `applyLocked` (serveCancel + `go c.doUnload()`); `internal/ollama/client.go` `Unload` | 0003 | Any "let it finish" or "keep the model warm" optimization reintroduces multi-minute game stalls or VRAM contention. The few-seconds model reload after contention clears is the accepted cost. |
| 3 | **Priority order gaming/Plex > interactive > batch, and it is enforced at three points.** (1) `Scheduler.dequeue` drains `iq` (interactive) before `bq` (batch), FIFO within class. (2) An interactive request that parks pings the coalesced `interactiveWaiting` channel. (3) The Job worker's `monitor` goroutine honors that signal — batch never preempts anything; interactive preempts a running Job only past the quantum; gaming preempts everything instantly via the serve context. | `internal/queue/scheduler.go` (`dequeue`, `pingInteractiveLocked`, `InteractiveWaiting`); `internal/job/worker.go` (`monitor`, `shouldPreempt`) | 0004 | Reordering `dequeue`, dropping the ping, or checking `Interactive > 0` without the quantum breaks either interactive latency or batch progress. |
| 4 | **Batch min-run quantum ("min-run" framing is canonical).** A RUNNING Job is *protected from interactive preemption for its first `BROKER_BATCH_QUANTUM`* (default 10s), then preemptible: `shouldPreempt` returns `w.svc.now().Sub(start) >= w.quantum && w.gate.Stats().Interactive > 0`. ADR-0004's three load-bearing properties: bounds interactive added-latency to ≈ one quantum (worst case: interactive arrives just as a Job starts); guarantees batch *progress* (every Job gets at least a quantum of GPU, so steady interactive load cannot starve batch); prevents reload thrash from interactive bursts. An earlier ADR draft stated the rule in the OPPOSITE direction — the ADR itself documents this; the three properties are the intent. | `internal/job/worker.go` `shouldPreempt`, `monitor` | 0004 | Flip the direction ("run at most a quantum") and interactive latency is unbounded while batch can be starved to zero. Remove the quantum and interactive bursts thrash model reloads. Remove the ticker in `monitor` and a long-parked interactive request is never noticed. |
| 5 | **Durability write ordering.** A submitted Job is persisted (transaction committed in `SQLiteStore.Submit`) *before* its `job_id` is acked to the consumer (`Service.Submit` returns only after `store.Submit`); a result is persisted *before* the Job flips to SUCCEEDED — `SQLiteStore.Succeed` sets `state`, `result`, `finished_at` in ONE UPDATE statement, so no SUCCEEDED Job can lack its result. ADR-0007: "No ack is ever lost; no SUCCEEDED Job lacks its result." | `internal/job/sqlite.go` `Submit`, `Succeed`; `internal/job/service.go` `Submit` | 0007 | Ack-before-persist loses Jobs on crash between ack and write. Splitting `Succeed` into two statements creates a window where a SUCCEEDED Job has no result. |
| 6 | **Preempted Jobs requeue at the FRONT; clean preemption never burns an attempt.** `SQLiteStore.Preempt` sets `position_hint = MIN(position_hint)-1` (front of line) and does NOT touch `attempts`. Only restart-recovery (`RecoverRunning`, `attempts+1`) and genuine run errors (`FailOrRetry`, `attempts+1`) increment attempts, both capped by `max_attempts`. Explicit cancel stops without requeue. ADR-0007 note: "a clean preempt (gaming/interactive) or explicit cancel never burns an attempt, so preemption under heavy gaming can't drive a healthy Job to FAILED." | `internal/job/sqlite.go` `Preempt`, `FailOrRetry`, `RecoverRunning`; classification in `internal/job/worker.go` `runJob` switch | 0006, 0007 | Requeue at back = starvation of long Jobs under steady load. Counting preemptions as attempts = a healthy Job FAILED after three gaming sessions. |
| 7 | **`Idempotency-Key` is mandatory on submit.** `POST /jobs` returns 400 without the header (`internal/job/api.go`); `SQLiteStore.Submit` has `idempotency_key TEXT NOT NULL UNIQUE` and returns the existing Job for a repeated key instead of duplicating. Essential because a consumer may retry submit after a crash before it ever saw the `job_id`. | `internal/job/api.go` (400 check), `internal/job/sqlite.go` `Submit` + schema | 0007 | Making the key optional silently allows duplicate expensive inference runs on consumer retry. |
| 8 | **Mode (Synchronous vs Job) is orthogonal to priority class.** Synchronous = all interactive + short/cheap batch (embeddings), streamed, ephemeral, 503-and-retry. Job = long batch only, durable. Jobs are always the *batch* class; there is no interactive Job and no per-request `async` flag (rejected in ADR-0006). | port wiring in `cmd/broker/main.go` (Gate per class); `internal/job/worker.go` (`queue.Batch` only) | 0006 | An `async` flag pushes the mode choice onto every caller and muddies the API; making all batch async makes sub-second embeddings pay submit/poll/persist/prune overhead. |
| 9 | **Embed lane: OWN `queue.Scheduler`, SHARED yield `Controller`.** In `cmd/broker/main.go` the lane builds `embedSched := queue.New()` (its own MaxInflight=1 slot — Infinity saturates all CPU cores per request) but gates with the same `ctrl`. GPU inference and CPU embedding use different hardware and must not serialize on one slot; yield state must be shared so the lane backs off on Contention like everything else. Lane exists only when `INFINITY_URL` is set. | `cmd/broker/main.go` (embed-lane block) | 0008 | Sharing the GPU scheduler idles the GPU while the CPU embeds (halves throughput). Giving the lane its own yield controller lets embedding contend with games for the CPU/host. |
| 10 | **Consumers keep the stable wire contract.** Ollama's own API on `:11435` (interactive) and `:11436` (batch) — consumers integrate by repointing their Ollama host, zero code. The embed lane presents OpenAI `POST /embeddings` (and `/v1/embeddings`) on `:11438` and rewrites the path to Infinity's `/embeddings_image` in `proxy.NewEmbed` — bodies untouched. The rewrite exists because Infinity's unified `/embeddings` tokenizes a base64 `data:` URI as TEXT, returning a near-identical text-tower vector for every image (a real trap that was hit). | `internal/proxy/proxy.go` `New`, `NewEmbed`, `embedImagePath` | 0001, 0008 | Teaching consumers `/embeddings_image` leaks a serving-stack quirk into the stable contract; dropping the rewrite silently corrupts every image embedding. |
| 11 | **Detection fails OPEN.** On a process-listing error `Detector.Detect` returns `("", false)` — no contention, inference proceeds. Source comment: "fail open, never block inference because we couldn't read /proc." `ProcLister` returns no processes on non-Linux, so on macOS detection is silently disabled. | `internal/detect/detect.go` `Detect`, `ProcLister` | — (ported verbatim from Bash V3) | Fail-closed turns any transient /proc hiccup into a total inference outage. But note the flip side: on a platform without /proc the Broker never yields — do not "test yield" on macOS and conclude it is broken. |
| 12 | **The scheduler owns slot handoff: `Release` transfers, it does not free.** When a waiter exists, `Release` closes the waiter's channel and returns *without decrementing `inflight`* — slot ownership moves to the woken waiter directly, so no third party can steal it in between. Verbatim from `scheduler.go`: `close(w) // slot ownership transfers to the woken waiter; inflight unchanged`. The companion subtlety in `Acquire`: if ctx is cancelled but the waiter was *already granted* (removed from the queue under lock), the caller owns the slot and must hand it on via `s.Release()` so it is not leaked. | `internal/queue/scheduler.go` `Release`, `Acquire` (cancellation branch) | 0004 | "Fixing" `Release` to always decrement, or dropping the cancellation-race branch in `Acquire`, leaks or double-frees the slot — the classic bugs in hand-rolled semaphores. Any scheduler change must keep both paths and their tests. |
| 13 | **Batch-class parking during yield: bounded queue, FIFO drain, never holds a GPU slot.** A Batch request that arrives during yield enters the `parker` queue (bounded by `BROKER_PARK_MAX_QUEUE`, default 32; **never** acquires the GPU slot). The `RunParkDrain` goroutine polls `Controller.Yielding()` every ~1s and releases up to `BROKER_PARK_DRAIN_BURST` parked requests (default 8 per tick) back into `Scheduler.Acquire` when yield ends. Parked requests time out after `BROKER_PARK_HOLD` (default 600s) with 503/expired. Interactive class **never parks** — always gets 503/deferred during yield. `BROKER_PARK_MAX_QUEUE=0` disables parking, reverting to immediate 503 (fail-closed). `RunParkDrain` is the only `Scheduler` method taking a bare `yielding func() bool` closure instead of an `Admission` — intentional per ADR-0009 design (yield state ownership remains in `Controller`). Graceful shutdown propagates context cancellation to all parked requests within 10s; hard crashes lose parked state (in-memory only, no persistence — LightRAG rescan recovers). | `internal/queue/park.go` (`parker`, `parkFor`, `drainOneBurst`); `internal/queue/gate.go` admission loop; `cmd/broker/main.go` wiring; `internal/queue/scheduler.go` `RunParkDrain` | 0009 | Removing the queue ceiling enables unbounded memory growth under sustained embed traffic during yield. Removing FIFO starves long-parked requests. Letting parked requests hold GPU slots blocks Interactive and stalls the gaming machine. Removing the drain loop or yield-polling makes parking a timeout-only mechanism (defeats the purpose: low latency on yield end). Interactive parking would block responsive user actions during yield. |

Two response-signal facts that follow from the invariants and trip people up:

- `X-Broker-Status` **header** is optimistic (`served`) because headers are written before the upstream streams; the **HTTP trailer** `X-Broker-Status` carries the authoritative final outcome (`served`/`preempted`) on chunked responses (`internal/queue/gate.go`, `http.TrailerPrefix`). A preempted stream is cut with NO in-band marker (injecting one was rejected — cancelling mid-line would corrupt the NDJSON); detect via the trailer or the absence of Ollama's terminal `{"done":true}` line.
- The Job worker acquires the GPU slot **before** claiming from SQLite (`worker.go` `runNext`), so a Job only becomes RUNNING once it actually holds the GPU — never while merely waiting in line. Position reporting depends on this.

## Load-bearing decisions (one row per ADR)

Full text and rejected-alternative reasoning live in `docs/adr/`. Do not re-litigate a rejection without new evidence; reopening one requires a new ADR (see broker-change-control).

| ADR | Decision | Why | Rejected |
|-----|----------|-----|----------|
| [0001](../../../docs/adr/0001-http-fronting-proxy-in-go.md) | HTTP-fronting proxy, written in Go, single static binary | Real Consumers are HTTP daemons that call Ollama's API directly and will never invoke a CLI wrapper; repointing the Ollama host = zero per-service code. Go folds detection + proxy + queue into one systemd unit. | Bash V3 daemon (CLI-only coverage); Python/FastAPI Tier-3 direction. |
| [0002](../../../docs/adr/0002-stateless-http-bounded-wait.md) | Stateless HTTP path: bounded wait then 503 + Retry-After, consumers own retry. **Superseded for long batch by 0006/0007**; still in force for Synchronous requests. | A caller cannot hold a connection through a multi-hour gaming session; every Consumer already tolerates a deferred LLM. Broker holds nothing across restarts. | Holding connections until served (fd leaks, client timeouts fire anyway); durable SQLite *request* queue (an HTTP response cannot be replayed after the caller's connection is gone). |
| [0003](../../../docs/adr/0003-hard-yield-vram-unload.md) | Hard yield: cancel in-flight + force VRAM unload | Cancelling alone leaves the model resident in VRAM, still starving the game. Reload-on-resume costs seconds; a stalled game costs the project its reason to exist. | Graceful drain; cancel-but-leave-loaded. |
| [0004](../../../docs/adr/0004-gpu-scheduling-policy.md) | Configurable concurrency (`BROKER_MAX_INFLIGHT`), tiered preemption (gaming > interactive > batch), batch min-run quantum | The original design conflated "yield to gaming" with "one inference at a time"; a long batch call could block a latency-sensitive interactive request that only queue-jumped, never preempted. | Always-on concurrency-1; trusting Ollama's own FIFO (loses cross-consumer priority); strict fairness/aging (more machinery than a single-GPU home box needs). |
| [0005](../../../docs/adr/0005-control-plane-auth.md) | Open reads (`/metrics`, `/healthz`, `/status`), bearer-token-gated `POST /control` (`BROKER_CONTROL_TOKEN`); token unset → mutations loopback-only. **NOT IMPLEMENTED — see weak points.** | Any LAN device could `POST /control {"mode":"serve"}` and silently defeat yield, or `{"mode":"yield"}` for an inference DoS. Grafana must keep scraping without config. | Trusting the LAN; token auth on every endpoint (breaks Grafana scrape for no gain on read-only data). |
| [0006](../../../docs/adr/0006-durable-job-system.md) | Durable async Job system for long batch, hybrid with the sync proxy; mode orthogonal to class | Long, resilience-critical batch (scoring runs, vision) needs durable queueing, Position/status feedback, restart survival — stateless 503-and-retry proved too weak. Broker is data plane only; UX lives in the Consumer. | All-batch-async; per-request `async` flag. Cost accepted: long-batch Consumers stop being zero-code (an adapter each). |
| [0007](../../../docs/adr/0007-job-durability-and-restart.md) | SQLite (pure-Go, WAL); persist-before-ack; result-before-SUCCEEDED; RUNNING→QUEUED@front with `attempts++` on restart (cap 3); mandatory Idempotency-Key; retain-until-fetched (grace 1h) + hard cap (7d) | LLM generation has no checkpoint — re-run is the only restart option, safe because inference is idempotent compute (may orphan one in-flight generation, accepted). | Hard time-based TTL alone (a crashed consumer loses a result it never saw); resume-from-checkpoint (does not exist for LLM generation). |
| [0008](../../../docs/adr/0008-image-embedding-lane.md) | Embed lane: front Infinity SigLIP on `:11438`, own scheduler, shared yield, path rewrite to `/embeddings_image`, optional via `INFINITY_URL` | Ollama cannot serve SigLIP image embeddings; Infinity runs on CPU (its ROCm image supports MI200/MI300 only, not RDNA4) so it never contends for VRAM — but it does contend for the CPU/host with gaming/Plex, and the Broker already computes that one signal. | Routing through the Ollama batch port; sharing the GPU scheduler; teaching consumers `/embeddings_image`; a separate repo. |

## Known-weak points (as of 2026-07-02 — plainly)

State these in any design review; do not design around them silently.

1. **`POST /control` is unauthenticated.** ADR-0005 was accepted 2026-06-16 with "Code change pending" — `BROKER_CONTROL_TOKEN` appears nowhere in the code (grep returns zero hits). The control plane listens on all interfaces; any LAN device can force `serve` (games stutter, yield defeated) or `yield` (inference DoS).
2. **Raw Ollama `*:11434` listens on ALL interfaces on the desktop.** The "nothing talks to :11434" house rule is convention only, not enforced by firewall or bind address. Any Consumer misconfigured with the raw port bypasses the Broker entirely.
3. **`resource-manager.service` (the legacy Bash daemon) is STILL RUNNING live alongside the Broker** — verified 2026-07-02. `docs/DESIGN.md` explicitly forbids two uncoordinated GPU arbiters; the promised retire-after-soak never happened. Its independent yield actions can fight the Broker's. Retiring it is part of the cutover campaign (see broker-cutover-hardening-campaign).
4. **`go test ./...` and `go vet ./...` FAIL at HEAD**: `internal/admin/admin_test.go` (the `newMux` helper, ~line 29) calls `Mux` with 5 args; the signature grew a 6th (`TdarrStatusFn`) in the Tdarr commit dd39d20 (2026-06-29). All other packages pass. Expect this failure; do not treat it as your change's fault, and do not "drive-by fix" it inside an unrelated change.
5. **`internal/schedule` and `internal/tdarr` have NO tests.**
6. **Schedule windows are hardcoded in code** (`internal/schedule/schedule.go`: internal-scraper-service Fri 02:00 + 5h — the only window that pauses Tdarr (`SafeForBackgroundGPU` false) — plus a daily 02:00–09:00 `safe-batch` label marking the PREFERRED slot for background GPU work) — changing the window means editing Go source and redeploying, not config.
7. **`main` branch is stale** (3 early commits); all v2 work lives on `v2-go`, never merged. **No CI exists** (no `.github/`), which is how weak point 4 shipped.
8. **The trailer-based outcome signal requires HTTP/1.1 chunked responses and a trailer-aware client.** A Consumer whose HTTP library ignores trailers cannot see `preempted` on a cut stream; its only fallback is noticing the missing `{"done":true}` terminal line. Non-streamed preempted requests surface as plain 503, so the header is never wrong there — the gap is streamed-and-ignoring-trailers only.

## When NOT to use this skill

- **Env var names, defaults, live overrides, deploy drift** → `broker-config-and-flags`.
- **Generic single-GPU / preemption / VRAM / RDNA4 / Ollama-API theory** (not this repo's decisions) → `gpu-arbitration-reference`.
- **Live misbehavior triage** ("why is my request 503ing right now") → `broker-debugging-playbook`.
- **How to classify/gate/review a change**, whether it needs an ADR → `broker-change-control`.
- **The history of failed approaches** (V1/V2 circular detection, GPU-detection saga) → `broker-failure-archaeology`.
- **Executing the cutover/hardening work** (retire legacy daemon, implement ADR-0005, fix the broken test) → `broker-cutover-hardening-campaign`.
- **Future graded-yield design** → `broker-graded-yield-frontier`; this skill documents only the current binary-yield contract.

## Provenance and maintenance

Written 2026-07-02 against `v2-go` HEAD; every invariant verified in source that day. Re-verify an anchor before trusting a row:

```sh
cd /Users/prestonbernstein/dev/ollama-resource-broker

# Inv 1: "not a law" wording still in ADR-0004
grep -n "conservative default, not a law" docs/adr/0004-gpu-scheduling-policy.md

# Inv 2: hard yield = cancel + unload
grep -n "serveCancel()" internal/yield/yield.go && grep -n "go c.doUnload()" internal/yield/yield.go

# Inv 3: interactive dequeued before batch; waiting signal exists
grep -n "len(s.iq) > 0" internal/queue/scheduler.go && grep -n "InteractiveWaiting" internal/job/worker.go

# Inv 4: quantum rule direction (min-run: >= quantum AND interactive waiting)
grep -n "Sub(start) >= w.quantum" internal/job/worker.go

# Inv 5: result and SUCCEEDED in one UPDATE
grep -n "state = ?, result = ?" internal/job/sqlite.go

# Inv 6: requeue-at-front, attempts untouched on Preempt
grep -n "COALESCE(MIN(position_hint), 0) - 1" internal/job/sqlite.go

# Inv 7: Idempotency-Key still mandatory
grep -n "Idempotency-Key header is required" internal/job/api.go

# Inv 9: embed lane own scheduler, shared ctrl
grep -n "embedSched := queue.New()" cmd/broker/main.go

# Inv 10: path rewrite still present
grep -n "embeddings_image" internal/proxy/proxy.go

# Inv 11: fail-open comment
grep -n "fail open" internal/detect/detect.go

# Inv 12: slot handoff comment
grep -n "slot ownership transfers" internal/queue/scheduler.go

# Weak point 1: ADR-0005 still unimplemented (expect 0)
grep -rn "BROKER_CONTROL_TOKEN" --include="*.go" . | wc -l

# Weak point 4: suite still broken at HEAD (expect internal/admin failure only)
go test ./... 2>&1 | grep -v "^ok"

# Weak points 2, 3 are LIVE-desktop facts (dated 2026-07-02, from the authoring
# brief's read-only check) — re-verify only via an explicitly approved ssh session:
#   ss -tlnp | grep 11434          # raw Ollama bind
#   systemctl is-active resource-manager.service
```

If any grep above returns nothing (or the weak-point greps return the opposite), the code has drifted from this contract: update the affected row here AND check whether the change had an ADR.
