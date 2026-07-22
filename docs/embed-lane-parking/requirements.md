# Requirements: Embed-lane request parking during GPU yield

Status: draft (spec-gather output, no user Q&A). Target repo: `ollama-resource-broker`, branch `v2-go` (code truth below is cited against that branch's live source, not this worktree's stale `main`-derived snapshot — see "Branch note").

## Branch note (must read before implementing)

This worktree (`ship-embed-parking`) was created off `main` (`729d462`), which predates the entire Go v2 broker (`internal/proxy`, `internal/queue`, `internal/yield`, `internal/config`, `internal/metrics`, ADR-0004–0008 do not exist on this branch). The real, current implementation — including the `BROKER_BATCH_WAIT` 300s change (commit `ad07905`) this feature is scoped against — lives on `v2-go` at `/Users/prestonbernstein/dev/ollama-resource-broker` (main checkout). **Implementation must branch from `v2-go`, not from this worktree's current HEAD**, or the diff will re-delete the entire Go broker. All code-truth citations below are verified against `v2-go`.

A second consequence: the ship-it brief instructs "new ADR-0004". On `v2-go`, ADR-0004 already exists and is unrelated (`0004-gpu-scheduling-policy.md` — configurable concurrency, tiered preemption, batch quantum, accepted+implemented 2026-06-16). ADRs 0001–0008 are all taken. Writing a new ADR as "0004" would silently overwrite/collide with an accepted decision, which is exactly what change-control non-negotiable #4 forbids ("superseded ADRs get a status line, never deletion"). **This spec uses ADR-0009** for the new decision. This is a correction, not an open question — flagging it here so it isn't silently "fixed" by picking whatever number is convenient at build time.

Verification: the previously-tracked `.gitignore`/`internal/queue` package-existence prerequisite is already resolved upstream (commit `e565edc`) — no scaffolding prerequisite remains before this feature's implementation begins.

## Problem statement

### Current behavior (code truth)

The Broker's Fronting Proxy gates every Batch-class Synchronous request through `Scheduler.Gate` (`internal/queue/gate.go:34-94`). On each request:

1. `internal/queue/gate.go:39-42` — before anything else, `adm.Yielding()` is checked. If the Broker is yielding, `deferRequest` fires immediately: `Retry-After`, `X-Broker-Status: deferred`, HTTP 503. The scheduler slot is never touched; the `wait` budget (`cfg.BatchWait`, `BROKER_BATCH_WAIT`) is **not consulted at all** in this path.
2. Only if not yielding does the handler call `s.Acquire(actx, class)` bounded by `wait` (`internal/queue/gate.go:44-46`) — this is the GPU-slot contention wait (e.g. an interactive request holding the single slot), raised from 5s to 300s in commit `ad07905` ("Batch is non-real-time... it must WAIT for the GPU under contention, not fast-fail") specifically to stop bulk LightRAG ingestion from erroring on transient slot contention.
3. After the slot is acquired, `Yielding()` is checked **again** (`internal/queue/gate.go:54-57`): if yielding began while queued for the slot, the request is deferred just the same — immediate 503, no use of the wait budget.

`TestGateRefusesWhenYielding` (`internal/queue/gate_yield_test.go`) is the existing proof of this contract: a request against an always-yielding `Admission` gets 503 in well under its configured wait budget, the upstream is never hit, and the slot is never held. (Note: this test exercises `Batch` class — see AC-21 for why a separate Interactive-class test is required to prove FR-10.)

Yield itself is hard by design (ADR-0003, invariant #2 in `broker-architecture-contract`): the instant contention is detected, `yield.Controller.applyLocked` (`internal/yield/yield.go:190-212`) cancels the shared `serveCtx` (aborting every in-flight upstream call) and spawns `doUnload` (forces `keep_alive=0`). This is correct and **out of scope to change** — the gap is entirely in what happens to a request that *arrives* (or is *waiting for a slot*) during that window, not in the yield transition itself. This feature does not add any new caller of `yield.Controller`, callback, or interface method — see FR-6 for how release is actually signaled.

### The cascade (why 503 is worse than it looks here)

ADR-0002's premise — "every consumer already tolerates a deferred LLM... LightRAG... retry" — is disproven in production. Per the 2026-07-22 research sweep (`gpu-broker-embed-queueing.md`, NAS vault):
- LightRAG's maintainers confirm no retry/backpressure semantics at the embedding layer (LightRAG discussion #1591: their fix for slow embeds is `TIMEOUT=None`, not retry).
- Multiple LightRAG issues (#2387, #2300, #2257, #2495) show an embed-layer failure aborting the whole ingest batch, not just one document.
- No shipping embedding proxy (Ollama, TEI, Infinity, llama.cpp) absorbs a transient upstream-unavailable window transparently; the closest prior art (TEI's `--max-concurrent-requests`) is bounded reject-fast, not park-and-replay — this capability does not exist anywhere in the stack today and must be built here.

So: gaming starts → yield begins → the in-flight LightRAG embed call is cancelled (ADR-0003, correct) → LightRAG's *next* embed call 503s instantly (this gap) → LightRAG cancels the entire ingest batch, not just the interrupted call. Parking absorbs exactly the second step.

## Users / Consumers

| Consumer | Lane | Affected | Notes |
|---|---|---|---|
| LightRAG (RAG + embeddings) | `:11436` Batch (text embeddings via Ollama) | **Primary** — this is the embed-cascade this feature exists to fix | `EMBEDDING_FUNC_MAX_ASYNC=10` bounds LightRAG's own concurrent caller count against the Broker |
| internal-scraper-service (vision embeddings) | `:11438` embed lane (Infinity, image embeddings) | Incidentally covered — same `Gate` call site, same `Batch` class | Not the driver for this feature; benefits as a side effect of fixing `Gate` once. No internal-scraper-service-specific requirement here. |
| internal-monitor-app | `:11436` Batch (scoring, via durable Job path per ADR-0006, not Synchronous) | **Not applicable** — Jobs don't go through `Gate`'s Synchronous path at all | Confirms out-of-scope: Job path is untouched. |
| Interactive chat / `llm` CLI / `gcm` | `:11435` Interactive | **Explicitly unaffected** — must keep today's immediate-503 semantics | Constraint, not a beneficiary. |
| Operator (Preston) | `deploy/broker.service`, Grafana/Prometheus | Needs the alert signal + restart-hardening decision | See NFR/AC on observability and shutdown. |

## Functional requirements

FR-1. When a Batch-class request arrives at `Gate` (`internal/queue/gate.go:39`) while `adm.Yielding()` is true, the Broker MUST place the request in a bounded **park** instead of returning 503 immediately, unless the park-queue ceiling (FR-5) is already exceeded.

FR-2. When a Batch-class request has already begun waiting for a GPU slot (past the check at `internal/queue/gate.go:44`) and yielding becomes true before or at the point the slot would be granted (the check at `internal/queue/gate.go:54`), the Broker MUST route it into the same park mechanism as FR-1, not an immediate 503. (This closes the second existing yield-check, not just the first.)

FR-3. A parked request MUST remain parked until exactly one of: (a) yielding ends and the request is released per FR-6/FR-7, (b) the configured park hold bound (FR-4) elapses, (c) the client disconnects (request context cancelled), or (d) the Broker begins graceful shutdown (FR-9). No other exit path is permitted (never an unbounded park).

FR-4. The park hold bound is configurable (`BROKER_PARK_HOLD`, duration, default `600s`, see C-8) and enforced per parked request. A request parked longer than this bound is released as an expiry, surfaced on two explicitly distinct channels that MUST NOT be conflated:
  - **HTTP surface** (client-facing): the same response the Broker gives today for a deferred request — 503, `Retry-After`, `X-Broker-Status: deferred`. A client cannot and need not distinguish "never got a slot" from "was parked, then timed out" — both are the existing "come back later" contract, unchanged on the wire.
  - **Metrics surface** (operator-facing): a new `broker_requests_total` outcome label `expired` (FR-11), distinct from `deferred` (the pre-existing outcome recorded for requests rejected before ever being parked, FR-1's ceiling case aside) and from `park_rejected` (FR-5) — so an operator reading Prometheus can tell "expired in the park" apart from "rejected before parking" and "rejected while waiting for the GPU slot," even though the HTTP response looks identical (503/`deferred`) in all three cases.

FR-5. The park queue has a hard ceiling, configurable (`BROKER_PARK_MAX_QUEUE`, default **32**, see C-8), enforced independently per Batch-class `Gate` instance (i.e. `:11436` and `:11438` each get their own ceiling, matching the existing per-`Scheduler` `MaxWaiters` pattern in `internal/queue/scheduler.go`). A request arriving when the park queue is at the ceiling gets an immediate 503 with a distinct outcome tag `park_rejected` — reject-fast, never unbounded, matching ADR-0002's own rejected-alternative reasoning ("fd exhaustion") and TEI's `--max-concurrent-requests` prior art cited in the research doc. The ceiling check itself is specified atomically in FR-14; `maxQueue == 0` is an explicit operator kill-switch per FR-13, not just "a small number."
  - Default reasoning (decided here, not left open): LightRAG issues embeds through `EMBEDDING_FUNC_MAX_ASYNC=10` concurrent callers. `proxy.retryTransport` (`internal/proxy/proxy.go`) can hold a request through up to 2 additional connection-level retries, and other Batch-class traffic can interleave. 32 gives roughly 3x headroom over LightRAG's steady-state concurrent-caller count without approaching a fd budget that matters on a single-purpose home broker process (ulimit defaults are in the thousands). This value is decided as the default here; ADR-0009 records the reasoning as the durable rationale, not as an open question.

FR-6. Release is driven by **polling, not an event callback**. A drain supervisor goroutine (one per Batch-class `Gate` instance, i.e. `:11436` and `:11438` each get their own) runs on the existing 1s drain ticker and, on each tick, checks `adm.Yielding() == false`. When yielding has ended, parked requests are released in **FIFO order of park-entry time**, not arrival order at `Gate` if those differ, and not all at once — release is bounded by the drain-burst cap (FR-7). `yield.Controller` is **NOT modified** for this feature: no `YieldEnd()` callback, no `Admission` interface widening, no change to any existing test fake. Rationale: polling avoids an entire class of callback-ordering and busy-loop bugs, keeps `yield.Controller` as the clean seam already earmarked for the planned graded-yield work (`broker-graded-yield-frontier`), and costs at most ~1s of extra release latency versus an event push — negligible against the 600s hold bound (FR-4).
  - This FIFO guarantee applies to *release-signal order* only — the order in which parked goroutines are woken and re-enter slot-acquire. End-to-end GPU service order remains approximate (goroutine wake-scheduling order, plus any new Batch requests arriving concurrently and racing into the same `Scheduler.Acquire` queue). ADR-0009 documents this distinction explicitly so release-order FIFO is never mistaken for a stronger, end-to-end-service-order guarantee than the code actually provides.

FR-7. Release from the park queue is rate-bounded by a configurable **drain-burst cap** (`BROKER_PARK_DRAIN_BURST`, default **8** — i.e. at most 8 parked requests released per 1s drain-ticker interval, see C-8), reasoned from the same order of magnitude as `BROKER_MAX_WAITERS`'s existing per-class practice and from Ollama's own `OLLAMA_MAX_QUEUE` default of 512 and its FIFO-then-503 backpressure, so that a full park queue draining does not present Ollama with a burst larger than it would tolerate from a single caller. This default is decided here; ADR-0009 records the reasoning. Each released request re-enters the existing slot-acquire flow (`Scheduler.Acquire`, unchanged) and is subject to the existing `BatchWait` budget for slot contention exactly as it is today — parking and slot-queueing remain two distinct, sequential bounded waits.

FR-8. If the request's own context is cancelled while parked (client disconnected), the park is abandoned immediately: the goroutine returns without consuming a slot-acquire attempt and without incrementing `served`, `expired`, or `park_rejected` — a new outcome tag `canceled` covers it, consistent with the Broker's existing pattern of never leaving a slot or a goroutine dangling (invariant #12 in `broker-architecture-contract`, "the scheduler owns slot handoff"). Per FR-15, this exit path also removes the entry from the park queue exactly once.

FR-9. Broker shutdown is signaled in two stages that this feature must respect in order:
  1. The **app-lifetime context is cancelled** (the existing `cancel()` call in `cmd/broker/main.go`, which fires at the *start* of shutdown, strictly before `srv.Shutdown` is even called, and strictly earlier than `shutCtx`'s 10s window begins). Every currently parked request MUST observe this cancellation and begin unwinding immediately — it is the bound this feature is actually built against, not `shutCtx`.
  2. The existing 10-second graceful-shutdown window (`shutCtx` in `cmd/broker/main.go`) is the outer bound within which that unwind must *complete* for every parked request — never held past it, never left for the OS to reset the connection silently.

  The response for every parked request unwound this way is 503 with a distinct outcome tag `crash_failed`, and `X-Broker-Status` on that response is `crash_failed` (a new value distinct from `served`/`deferred`/`preempted`, see C-8), so a consumer or log scrape can tell "the Broker went away mid-park" apart from ordinary contention. Per FR-15, this exit path also removes the entry from the park queue exactly once.

FR-10. Interactive-class requests are unmodified: the checks at `internal/queue/gate.go:39` and `:54` continue to call `deferRequest` immediately for `Interactive` class, exactly as `TestGateRefusesWhenYielding` proves today for `Batch` class (see AC-21 for the Interactive-specific proof this FR actually requires). Parking applies to `Batch` class only — this must be enforced by the `Class` value passed into `Gate`, not a separate code path that could drift.

FR-11. Prometheus observability, extending `internal/metrics/metrics.go`'s existing conventions rather than inventing a new format (concrete names decided in C-8):
  - `broker_parked_depth{class="batch"}` gauge — live park-queue depth (mirrors the existing `broker_queue_depth{class=...}` pattern). Per FR-15, this gauge's increment/decrement is paired one-to-one with park-queue entry/exit, so it cannot drift from the true live count.
  - `broker_park_wait_seconds_sum` / `broker_park_wait_seconds_count` — time spent parked, for requests that were eventually served (mirrors the existing `broker_wait_seconds_sum`/`_count` pattern).
  - `broker_requests_total{class="batch",outcome="expired"|"park_rejected"|"canceled"|"crash_failed"}` — extends the existing `broker_requests_total{class,outcome}` counter (currently `served`/`deferred`/`preempted`) with the four new outcomes defined in FR-4/5/8/9, through the existing `Registry.Record(class, outcome, wait)` call shape.
  - The `expired` outcome counter MUST be structured so a standard `rate(broker_requests_total{outcome="expired"}[5m]) > 0` Prometheus alert expression is sufficient — no additional derived metric needed. Documenting this exact expression in ADR-0009 and the README is itself a deliverable of this FR; provisioning the Alertmanager/Grafana rule that consumes it is cross-repo (internal-infra owns shared Prometheus/Grafana config per the repo-architecture convention) and is not this repo's deliverable.
  - The `/status` admin endpoint also exposes live parked depth per class (AC-20), for an operator checking state synchronously without Prometheus.

FR-12. The drain supervisor goroutine (FR-6) is context-bound to the Broker's app-lifetime context and MUST stop cleanly on shutdown — either joined (awaited via `sync.WaitGroup` or equivalent) before the process exits, or bound to a context that is cancelled no later than the start of shutdown (the same `cancel()` call cited in FR-9), so it never fires a tick, and never touches the park queue, once shutdown has begun. No orphaned ticker goroutine may outlive `main()`.

FR-13. Fail-closed by design, not by accident:
  - A park state that is zero-value or never explicitly configured (e.g. a `Gate` instance constructed without park config, or park config left at Go's zero value) MUST reject-fast (503, no park) rather than silently accepting requests or parking with an unbounded/undefined ceiling. This is a stated design decision, not an incidental consequence of Go zero values, and is pinned by `TestGateRefusesWhenYielding` (re-pointed per AC-21).
  - `SetParkConfig` (or the equivalent config-setter) treats `maxQueue == 0` as an explicit operator kill-switch: "parking off, immediate 503 while yielding" — the same behavior the Broker has today. This is the documented first-line rollback lever if parking misbehaves in production: set `BROKER_PARK_MAX_QUEUE=0` (or the runtime equivalent) and restart/reload, no code change required.

FR-14. The park-queue ceiling check (FR-5) and the enqueue of a newly-parked request are a single atomic check-then-append performed under the same park-queue mutex — never a check followed by a separately-locked append. This closes the TOCTOU window where two concurrent arrivals could each observe `depth < ceiling` and both enqueue, pushing live depth past `BROKER_PARK_MAX_QUEUE`. Verified under `-race` with concurrent arrivals at exactly the ceiling (AC-17).

FR-15. Every park exit path — released (served), expired (FR-4), caller-disconnected (FR-8), rejected (FR-5, note: a rejected request never actually entered the queue, so this is a non-entry, not an exit, but is included here for outcome-metric completeness), and shutdown (FR-9) — removes the request's entry from the park queue and emits exactly one outcome metric, with no path that does one but not the other and no path that does either twice. This is the invariant that keeps `broker_parked_depth` (FR-11) from ever drifting away from the true live count: depth-decrement and outcome-emission happen together, on every exit, exactly once. Verified by the ghost-cleanup test (AC-16).

## Non-functional requirements

NFR-1. **Hold bound stays under the caller's timeout.** `BROKER_PARK_HOLD` default (600s) must remain, with margin, below LightRAG's `EMBEDDING_TIMEOUT` (1200s, wraps the entire call per the research doc's litellm-Router finding — "the 1200s caller budget maps to one wrapping timeout, not per-attempt"). This is a documentation + config-default requirement (the Broker cannot read LightRAG's config), not a runtime check against an unreachable value.

NFR-2. **Cumulative worst-case latency is bounded and testably asserted.** A request can incur park time (up to `BROKER_PARK_HOLD`) followed by slot-contention wait (up to `BROKER_BATCH_WAIT`) followed by actual embed-call serve time. With current defaults, `BROKER_PARK_HOLD + BROKER_BATCH_WAIT = 600s + 300s = 900s`; this is a *wait-time-only* budget, not the whole picture — the remaining ~300s of headroom under LightRAG's 1200s wrapping `EMBEDDING_TIMEOUT` is separate serve-plus-retry budget that parking and slot-contention waiting do not consume. Stated so it is checkable: defaults must satisfy `BROKER_PARK_HOLD + BROKER_BATCH_WAIT + p99 serve time « 1200s` (comfortable margin, not a near-exact fit). Because the Broker cannot observe LightRAG's p99 serve time at build time, the mechanically-enforced piece is narrower and lives in `internal/config/config_test.go`: a test on the constants that fails if `BROKER_PARK_HOLD + BROKER_BATCH_WAIT` stops leaving enough margin under 1200s (i.e. exceeds 900s at current defaults). ADR-0009 records the full reasoning, including the p99-serve-time portion the config test cannot check directly.

NFR-3. **No new fd/goroutine leak class.** The park-queue ceiling (FR-5, atomic per FR-14) is a hard cap enforced the same way `Scheduler.MaxWaiters` is today (reject-fast beyond the cap) — answers ADR-0002's "fd exhaustion" objection directly. A parked request holds exactly one goroutine and one open connection, bounded by the ceiling; no unbounded growth path may exist. FR-15's every-exit-path bookkeeping is what keeps this true under all termination modes, not just the happy path.

NFR-4. **No new persistent state.** Park bookkeeping is in-memory only (channels/goroutines), matching ADR-0002's stateless-HTTP premise, which remains true for everything *except* the specific "immediate 503 during yield" behavior this feature supersedes. No SQLite table, file, or other on-disk park state is introduced — this is the `internal/job` durable-Job machinery's job (ADR-0006/0007), and it is explicitly not reused or extended here.

NFR-5. **Interactive latency is unaffected — stated as a hard constraint, not an aspiration.** The `Interactive`-class code path through `Gate` MUST acquire zero park-related locks — no shared mutex, no channel send/receive that touches park bookkeeping, nothing park-specific on the Interactive path at all. Verified two ways: (1) the existing `go test -race ./...` suite stays clean with park code present and exercised concurrently with Interactive-class traffic — a race on a park lock touched from the Interactive path would surface here; (2) a benchmark note (a recorded before/after `go test -bench` comparison on the Interactive path, not new benchmark infrastructure) documents that Interactive-path latency is unchanged within noise. Code review of lock scope is a development-time sanity check that supports this constraint; it is not itself the acceptance test.

NFR-6. **Shutdown fits the existing window.** Failing all parked requests (FR-9) must complete within the existing 10s `shutCtx` in `cmd/broker/main.go` — the feature must not require widening that timeout. The unwind is triggered earlier, by the app-lifetime `cancel()` (FR-9, FR-12); `shutCtx` is only the outer completion bound.

NFR-7. **The `Restart=always` question is a documented decision, not a silent gap.** Code truth: `deploy/broker.service` (both the worktree's checked-in copy and the live description in the ship-it brief) **already has** `Restart=always`, `RestartSec=5`, `StartLimitIntervalSec=60`, `StartLimitBurst=5`. Parking introduces a new class of long-lived blocked handler (up to `BROKER_PARK_HOLD`) that did not exist before; if a bug in the park path causes repeated crashes, systemd's `StartLimitBurst=5` within `StartLimitIntervalSec=60` could exhaust the restart budget and leave the Broker down entirely — the one outcome durability is meant to prevent. ADR-0009 MUST explicitly decide whether the existing `StartLimitBurst`/`StartLimitIntervalSec` values remain adequate for this new failure surface or need widening, and record the reasoning either way. Do not silently add `Restart=always` as if it were missing — it is not.

## Constraints

C-1. Must not delay, soften, or conditionally skip any part of the existing yield transition. `yield.Controller.applyLocked` (`serveCancel` + `doUnload`) is unchanged — parking only changes how a Batch request is *admitted* at `Gate`, never how or when yield itself begins (ADR-0003 invariant, non-negotiable per change-control rule #2). `yield.Controller` also gains no new outbound call or interface method for this feature (FR-6) — the drain supervisor only *reads* `Yielding()`, an existing method, on a poll.

C-2. New env vars (`BROKER_PARK_HOLD`, `BROKER_PARK_MAX_QUEUE`, `BROKER_PARK_DRAIN_BURST` — concrete names and defaults decided in C-8; no separate `BROKER_PARK_ENABLED`-style toggle is needed, since `BROKER_PARK_MAX_QUEUE=0` is the decided kill-switch per FR-13) are a "new config axis" per `broker-change-control` and must touch all five required places in the same change: `internal/config/config.go`, `internal/config/config_test.go`, the README `### Configuration (env)` table, `deploy/broker.service`, and the `broker-config-and-flags` skill file. (Precedent for skipping this: the Tdarr vars in `dd39d20` only touched `config.go` and are still missing from the README/deploy unit as of 2026-07-02 per the skill's own weak-points list — do not repeat that incident.)

C-3. `docs/adr/0009-*.md` is a new one-page ADR, house style (decision + rationale + rejected alternatives), written before-or-with the code (house pattern per `broker-change-control`). It must explicitly answer, point by point, ADR-0002's three original objections to a durable/held approach:
  - fd exhaustion → answered by the hard parking-queue ceiling (FR-5, NFR-3), enforced atomically (FR-14).
  - client timeouts fire anyway → answered by the hold bound staying under LightRAG's `EMBEDDING_TIMEOUT` (NFR-1, NFR-2).
  - reboot-safety → answered by no cross-restart state (NFR-4) plus fail-fast-visibly on graceful shutdown (FR-9) — and an explicit, honest statement that a *hard* crash (SIGKILL, OOM-kill, power loss) is unobservable by the Broker by definition (no process remains to emit a metric or write a status), so recovery for that case is entirely external: LightRAG's own rescan of un-embedded chunks plus its LLM-response cache (extraction is never re-billed). This limitation must be stated, not glossed over.
  It must also record the reasoning behind the decided defaults (C-8), the release-order distinction (FR-6), and the `StartLimitBurst`/`StartLimitIntervalSec` decision (NFR-7).

C-4. `docs/adr/0002-stateless-http-bounded-wait.md` is amended in place (its status line updated, per change-control non-negotiable #4 — "superseded ADRs get a status line, never deletion"), noting it is now *also* superseded for the "arrives during yield" behavior of Batch-class Synchronous requests specifically, while the rest of its stateless-HTTP model remains in force.

C-5. `CONTEXT.md` gains new vocabulary entries — at minimum **Park** (or **Parked request**) and **Drain burst** — each with an *Avoid* list, following the existing entry format exactly (see `Queue`, `Preemption` for the pattern), per change-control non-negotiable #3. "Park" must be defined distinctly from the existing "Queue" term (which already means the per-class scheduler waiter list in `internal/queue/scheduler.go`) so the two are never conflated in code comments, docs, or metric names.

C-6. `go test ./...` and `go vet ./...` must not add any new failure beyond the pre-existing, tracked `internal/admin` failure (`broker-change-control` non-negotiable #1, weak point #4 in `broker-architecture-contract`) — run before and after, diff the failures, do not drive-by-fix `admin_test.go`. `go test -race ./...` across the full repo is a separate, additionally-required clean run (AC-22).

C-7. No change to `internal/job`, `internal/schedule`, `internal/tdarr`, the `:11435` Interactive listener wiring, or Ollama itself.

C-8. **Decided names.** Requirements above state required capabilities; the concrete identifiers are decided once, here, as the single source of truth for cross-document consistency (README, `deploy/broker.service`, `internal/config/config.go`, ADR-0009, and `CONTEXT.md` must all match these exactly):
  - **Env vars**: `BROKER_PARK_HOLD` (duration, default `600s`) · `BROKER_PARK_MAX_QUEUE` (int, default `32`; `0` = kill-switch per FR-13) · `BROKER_PARK_DRAIN_BURST` (int, default `8`, requests released per 1s drain-ticker interval per FR-6/FR-7).
  - **Metrics**: `broker_parked_depth{class="batch"}` (gauge) · `broker_park_wait_seconds_sum` / `broker_park_wait_seconds_count` · `broker_requests_total{class,outcome}` with new outcome label values `expired`, `park_rejected`, `canceled`, `crash_failed`.
  - **HTTP surface**: `X-Broker-Status` header values `deferred` (existing, reused for expiry per FR-4) and `crash_failed` (new, FR-9) · `Retry-After` (existing, reused).
  - **Admin surface**: `/status` gains a live-parked-depth field per class (FR-11, AC-20).

## Out of scope

- CPU fallback routing (`bge-m3-cpu` on raw `:11434`) — explicitly not implemented. LightRAG issue #1969 (CPU-embed instability) and Preston's own prior-instability report are the reasons; the research doc's verdict already demotes this to "optional, default-off, only after a dedicated LightRAG-`embedding_func` smoke test" — not part of this feature at all.
- Any change to the Interactive lane's preempt/503 semantics.
- Any change to the durable Job path (ADR-0006/0007) or `internal/job`.
- Any change to Ollama itself.
- Any modification to `yield.Controller`, its interface, or its test fakes — release is polling-driven (FR-6), not a new callback surface.
- True cross-process-restart durability of an in-flight HTTP request — architecturally impossible (an HTTP response cannot be replayed after the caller's connection is gone, ADR-0002's own original reasoning, unchanged). The recovery contract for a hard crash is LightRAG's rescan + its LLM-response cache, documented not built (C-3).
- Deploying the Prometheus alerting rule itself (internal-infra's shared Grafana/Prometheus config, per the repo-architecture convention of shared-services-live-in-internal-infra) — this repo's deliverable stops at exposing a metric the standard `rate(...) > 0` pattern can alert on, and documenting that expression (FR-11).
- Graded/refined contention detection (`broker-graded-yield-frontier` territory) — unrelated to this feature, though FR-6's polling design is chosen partly to keep `yield.Controller` a clean seam for that future work.
- Any widening of `deploy/broker.service`'s `StartLimitBurst`/`StartLimitIntervalSec` beyond what ADR-0009 explicitly decides is needed (NFR-7) — this is a decision to make in the ADR, not a default "just widen it" change.

## Acceptance criteria

AC-1. `TestGateParksDuringYield` (new, `internal/queue`): a Batch-class request arriving while `Yielding()` is true does not return until yielding clears (within the hold bound), then is served normally — upstream is hit, `X-Broker-Status: served` trailer, `outcome=served`.

AC-2. `TestGateParkExpires` (new): a Batch-class request parked longer than `BROKER_PARK_HOLD` returns 503 with `Retry-After` and `X-Broker-Status: deferred`, never reaches upstream, and is recorded with metrics `outcome=expired` (proves the two-surface split in FR-4).

AC-3. `TestGateParkQueueCeiling` (new): with the park queue at `BROKER_PARK_MAX_QUEUE`, the next arriving Batch request during yield is rejected immediately (503, `outcome=park_rejected`) without being parked and without blocking.

AC-4. `TestGateRefusesWhenYielding` (existing, Batch class) is re-pointed per AC-21 to pin FR-13's fail-closed/never-configured behavior, and continues to pass unmodified as that pin.

AC-5. `TestGateParkDrainBurst` (new, or extended `gate_yield_test.go`): given N parked requests exceeding `BROKER_PARK_DRAIN_BURST`, when yield ends, released requests reach slot-acquire at no more than the configured burst rate, in FIFO park-entry order.

AC-6. `TestGateParkClientDisconnect` (new): cancelling a parked request's context returns promptly (no leaked goroutine, verified via `go test -race` and a goroutine-count check), with `outcome=canceled`, not counted toward `served`/`expired`/`park_rejected`.

AC-7. `TestGateParkShutdown` / `TestMainShutdownFailsParkedRequests` (new): with active parked requests, cancelling the app-lifetime context (the `cancel()` call that fires at the *start* of shutdown, strictly before `shutCtx`'s 10s window begins — FR-9) causes every parked request to begin unwinding immediately; the whole unwind resolves every parked request with 503 / `X-Broker-Status: crash_failed` / `outcome=crash_failed` well within the existing 10s `shutCtx` window, never hanging.

AC-8. `broker_parked_depth{class="batch"}` gauge reflects live park count in a scrape taken mid-park (synthetic yield), and returns to 0 after drain — verified via a metrics-handler test analogous to existing `internal/metrics/metrics_test.go` coverage.

AC-9. `broker_requests_total{class="batch",outcome=...}` and `broker_park_wait_seconds_sum`/`_count` increment correctly for each of: served-after-park, expired, park_rejected, canceled, crash_failed — one assertion per outcome.

AC-10. `go test ./...` and `go vet ./...` run before and after the change show zero new failures beyond the pre-existing `internal/admin` `Mux`-signature failure (diff the failure set explicitly in the PR/commit body, per C-6).

AC-11. `BROKER_PARK_HOLD`, `BROKER_PARK_MAX_QUEUE`, `BROKER_PARK_DRAIN_BURST` are present with the decided defaults (C-8) tested in `internal/config/config_test.go`, documented in the README `### Configuration (env)` table, set with an explanatory comment in `deploy/broker.service` (matching the `ad07905` in-unit-comment precedent), and referenced in `broker-config-and-flags`.

AC-12. `docs/adr/0009-*.md` exists, one page, states the decision, and explicitly answers all three of ADR-0002's original objections (C-3) plus names rejected alternatives (e.g. unbounded park, no ceiling, immediate full-burst replay, event-callback release instead of polling). `docs/adr/0002-stateless-http-bounded-wait.md`'s status line is amended in place (C-4), not deleted.

AC-13. `CONTEXT.md` contains new entries for **Park** (or **Parked request**) and **Drain burst**, each with an *Avoid* list, distinct from the existing **Queue** entry (C-5).

AC-14. A chaos/soak validation procedure is documented (runbook or `broker-validation-and-qa` addition): force contention (fake gaming process) mid-embed-burst against a running Broker and confirm zero LightRAG ingest failures across the yield window — the research doc's own "Next actions #3", carried into this spec as a concrete, checkable step rather than left implicit.

AC-15. ADR-0009 records an explicit decision on `StartLimitBurst`/`StartLimitIntervalSec` adequacy for the new blocked-handler failure surface (NFR-7) — either "unchanged, because <reasoning>" or a specific new value with reasoning; either is acceptable, silence is not.

AC-16. `TestGateParkGhostCleanup` (new): park requests up to and attempted past the ceiling (some rejected per FR-5), let the parked requests' hold bound expire, and assert `broker_parked_depth` returns to the true live count (zero, if none remain) and that new Batch requests can park successfully afterward — proves FR-15 (no depth drift, no ghost entries blocking future parks).

AC-17. `TestGateParkCeilingConcurrent` (new, run with `-race`): fire concurrent Batch-class arrivals during yield, more than `BROKER_PARK_MAX_QUEUE` at once; assert exactly `BROKER_PARK_MAX_QUEUE` requests are parked and the remainder are rejected with `outcome=park_rejected`, with no overshoot — proves FR-14's atomic check-then-append under real contention.

AC-18. `TestGateParkDrainPacing` (new, extends AC-5): with parked requests exceeding `BROKER_PARK_DRAIN_BURST`, assert the release rate *per drain-ticker interval* — not just the eventual total — never exceeds the configured burst, by sampling releases tick-by-tick.

AC-19. `TestGateParkFlapBounded` (new): drive `Yielding()` through many rapid true/false transitions ("flapping") while requests are parked; assert the drain supervisor's invocation count per wall-clock window stays bounded (proportional to elapsed ticks, not to the number of flaps) — proves the polling design (FR-6) does not degrade into a busy loop under adversarial flap conditions.

AC-20. The `/status` admin endpoint gains a field exposing live parked depth per class (FR-11, C-8), verified by a handler test analogous to existing `internal/admin` coverage.

AC-21. `TestGateRefusesWhenYieldingInteractive` (new): proves FR-10 for `Interactive` class specifically. The existing `TestGateRefusesWhenYielding` (`internal/queue/gate_yield_test.go`) in fact exercises `Batch` class, so it does not on its own prove Interactive is unaffected; it is re-pointed to serve instead as the fail-closed pin for FR-13 (never-configured Batch park state rejects).

AC-22. `go test -race ./...` across the full repo is clean (zero new failures beyond the pre-existing tracked `internal/admin` failure, per C-6) before merge — run and reported as a distinct, named check in the PR, not folded silently into AC-10's plain `go test ./...`.
