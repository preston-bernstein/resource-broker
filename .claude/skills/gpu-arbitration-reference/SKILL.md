---
name: gpu-arbitration-reference
description: Domain-theory reference for the ollama-resource-broker — single-GPU arbitration theory, VRAM vs CPU preemption, Ollama keep_alive/unload and NDJSON stream mechanics, utilization- vs identity-based contention detection (the V1/V2 circularity failure), priority scheduling and the batch min-run quantum, durable-queue semantics (idempotency, write-before-ack, SQLite WAL), 503/Retry-After backpressure and HTTP trailers, AMD RDNA4/gfx1201/ROCm quirks, and SigLIP dual-tower embedding pitfalls. Load when you need to understand WHY the Broker works the way it does, when a term like yield/preemption/quantum/hysteresis/trailer/WAL/dual-tower is unclear, or when reasoning about GPU sharing, Ollama unload behavior, streaming cutoffs, or Infinity embedding correctness.
---

# GPU arbitration reference (domain theory as it applies to this repo)

This is the background-knowledge pack for the ollama-resource-broker. It teaches the theory a mid-level engineer typically lacks, always tied to how it shows up in THIS codebase. It is the only skill in this library that carries general background; every other skill assumes it.

Claims verifiable in the repo cite the file. Claims the repo cannot verify are labeled **background knowledge (verify independently if load-bearing)**.

When NOT to use this skill:
- This repo's own decisions, invariants, and their rationale → `broker-architecture-contract`.
- Live misbehavior triage → `broker-debugging-playbook`.
- Historical investigations for their own sake → `broker-failure-archaeology`.
- Env var values and defaults → `broker-config-and-flags`.

## 1. Why a single consumer GPU needs an arbiter

Background knowledge (verify independently if load-bearing): CPU time is preemptible at microsecond granularity — the OS scheduler context-switches by saving registers. GPU VRAM is not. A model's weights either occupy VRAM or they do not; there is no OS-level "swap out this LLM for a frame". A resident LLM and a game compete for the *same fixed VRAM pool*, and the loser stutters or OOMs. The only "context switch" available is a full model unload followed later by a full reload — seconds of disk-to-VRAM transfer for a multi-GB model, not microseconds. That asymmetry is the entire reason this repo exists: something must decide, in advance, who holds VRAM, because the hardware cannot time-slice it.

How that manifests here:

- The Broker (see CONTEXT.md for the enforced vocabulary) serializes GPU inference through a scheduler slot (`internal/queue/scheduler.go`, `BROKER_MAX_INFLIGHT` default 1) and, on Contention (gaming/Plex), Yields: refuses new work, cancels in-flight upstream calls, and forces VRAM free.
- **Ollama residency**: Ollama keeps a model loaded in VRAM after a request finishes, governed by the request's `keep_alive` parameter (background knowledge: Ollama's default is around 5 minutes; verify against your Ollama version if load-bearing). Sending a request with `keep_alive: 0` makes Ollama unload the model immediately.
- **How the Broker actually unloads** (`internal/ollama/client.go`, verified): `Client.Unload` first calls `GET /api/ps` (`LoadedModels`) to list every model currently resident, then for each one POSTs to `/api/generate` with the body `{"model": <name>, "keep_alive": 0}` and **no prompt** (`unloadOne`). An empty generate with `keep_alive=0` is Ollama's unload idiom — there is no dedicated unload endpoint used here. It is best-effort: it attempts every model and returns only the first error. The yield controller calls this in a goroutine when yielding begins (`internal/yield/yield.go`, `doUnload`).
- **Generation is an NDJSON stream**: Ollama's `/api/generate` streams newline-delimited JSON objects; each carries a `response` fragment, and the stream terminates with an object whose `done` field is `true` (and an `eval_count` token total). `Client.Generate` scans line-by-line and returns success only when it sees `Done: true`; a stream that ends without it is treated as an error so a truncated result is never persisted as success (`internal/ollama/client.go`, the "stream ended without done" path). NDJSON = newline-delimited JSON: one complete JSON object per line.

## 2. Detection theory: utilization vs identity

Two ways an arbiter can detect that a higher-priority claimant wants the GPU:

- **Utilization-based**: sample GPU busy-percent (e.g. via `rocm-smi`) and treat high % as Contention.
- **Identity-based**: inspect *what processes exist* (process names/cmdlines) and treat known high-priority programs as Contention, regardless of measured load.

**The circularity problem** (this repo's formative failure): an arbiter that itself serves inference *raises GPU% by doing its job*. "GPU busy → yield" therefore self-triggers — the arbiter sees its own Ollama traffic as contention and starves itself. This killed the Bash V1 and V2 managers. `legacy/GO-MIGRATION-HANDOFF.md` (~line 104) records the evolution verbatim:

> V1: GPU % monitoring → Failed when Ollama uses GPU (circular logic)
> V2: GPU + hysteresis → Still couldn't distinguish Ollama from gaming
> V3: Process detection → Clean separation, Ollama can use 100% GPU
> Why it works: Detects WHAT is using resources, not THAT resources are high.

**Hysteresis** — requiring a state change to persist for some window before acting on it — damps flapping (rapid yield/serve oscillation near a threshold). It cannot fix circularity, because the false signal is not transient noise: the arbiter's own load is *sustained* for exactly as long as inference runs, so it survives any persistence window. V2 proved this empirically. Hysteresis is an anti-thrash tool, not a disambiguation tool.

**Identity-based detection is non-circular** (Ollama's process is simply not on the list) **but blind to unknown launchers**: a game started by a launcher whose process signature is not in the pattern table is invisible, and inference will happily fight it for VRAM. This repo accepts that trade: `internal/detect` ports the Bash V3 patterns verbatim ("Plex Transcoder", "SteamLaunch AppId=", lutris/heroic/wine regexes; first match wins), reads `/proc` (Linux-only), and fails open — on macOS or unreadable `/proc` it reports no Contention rather than blocking inference. Adding a launcher means adding a pattern; see `broker-architecture-contract` for the invariant and `broker-graded-yield-frontier` for the research direction that would need non-circular utilization signals.

## 3. Preemption and scheduling theory as used here

Priority tiers (CONTEXT.md, ADR-0004): gaming/Plex (absolute) > interactive > batch. The scheduler (`internal/queue/scheduler.go`) keeps two FIFO waiter queues (`iq`, `bq`) and always grants a freed slot to the interactive queue first — FIFO *within* a class, strict priority *between* classes.

**Queue-jump vs Preemption — keep these distinct:**
- *Queue-jump*: an arriving interactive request goes ahead of all waiting batch requests for the next free slot. Nothing running is disturbed. This is what the scheduler itself does (`dequeue` drains `iq` before `bq`).
- *Preemption* (CONTEXT.md term): interrupting a *running* lower-priority request. The scheduler deliberately does not do this itself — it only *signals* via the coalesced `InteractiveWaiting()` channel; the batch slot holder (the Job worker, `internal/job/worker.go`) decides whether to surrender.

**Preemption must be cancellation + resource reclaim, or it is fake.** Background knowledge: merely marking a request "preempted" while its computation continues frees nothing — the GPU is still generating tokens and the model still holds VRAM. Real preemption here is (a) context cancellation propagated into the upstream HTTP call (Go `context.Context` derived from the yield controller's serve context — cancelling it aborts the in-flight `/api/generate`, see `gate.go` and `worker.go`), plus (b) on gaming yield, the `keep_alive=0` VRAM unload from section 1. Cancellation stops the work; unload reclaims the memory. Both are required before the game actually has the GPU.

**Min-run quantum** (`BROKER_BATCH_QUANTUM`, default 10s; enforced by `worker.monitor` / `shouldPreempt`): a running batch Job is protected from *interactive* preemption for its first quantum; after that, a waiting interactive request preempts it. ADR-0004 states the three load-bearing properties:
1. **Bounds interactive added-latency to ≈ one quantum** — worst case, interactive arrives just as a batch Job starts and waits one quantum for it to become preemptible.
2. **Guarantees batch progress (anti-starvation)** — every Job gets at least a quantum of GPU before it can be bumped, so steady interactive load cannot starve batch forever.
3. **Prevents reload thrash** — without a floor, a burst of interactive requests would preempt/reload the batch model repeatedly, and (per section 1) each cycle costs seconds of model reload, wasting the GPU on churn.

Note the quantum protects batch only from *interactive*; gaming/Plex Contention preempts instantly, quantum or not (`worker.monitor`: `serveDone` cancels unconditionally). Also note ADR-0004 itself records that an earlier draft stated the rule in the opposite direction — "min-run" (protected first, preemptible after) is canonical; do not trust stale paraphrases.

**Requeue-at-front after Preemption** (work conservation + fairness): a preempted Job returns to the *front* of the Queue, not the back. Mechanism (`internal/job/sqlite.go`, `Preempt`): the Job's `position_hint` is set to `MIN(position_hint) - 1` over queued Jobs, and `ClaimNext` orders by `position_hint ASC, created_at ASC`. Why front: the Job already consumed GPU time; sending it to the back would let every later arrival leapfrog it, so under recurring preemption it could starve *and* its partial work is repeatedly re-paid (LLM generation has no checkpoint — section 4). Resume-first preserves work conservation (minimize re-done work) and arrival-order fairness.

## 4. Durable-queue theory as used here

The Job system (`internal/job/`, ADR-0006/0007, `docs/DESIGN-jobs.md`) exists because a synchronous HTTP request dies with its connection, but hour-scale batch inference on a GPU that can be yanked away any moment needs to survive disconnects, preemptions, and Broker restarts.

- **Write-before-ack**: a submitted Job is persisted to SQLite *before* the HTTP 201 Created is returned (200 on an idempotent replay — `internal/job/api.go`) (`DESIGN-jobs.md`: "Write-before-ack on submit; write-result-before-SUCCEEDED"). If the Broker crashes right after acking, the Job still exists. The mirror rule on completion — persist the result before flipping state to SUCCEEDED — means a SUCCEEDED state always has its result on disk.
- **Exactly-once-ish via idempotency keys**: network retries make "did my submit land?" unanswerable client-side, so blind resubmission would duplicate work. `POST /jobs` requires an `Idempotency-Key` header (400 without it, `internal/job/api.go`); the column is `UNIQUE` in the schema, and a retried submit with a known key returns the *existing* Job with HTTP 200 instead of creating a new one (`sqlite.go` Submit). This gives at-most-one Job per key. It is "exactly-once-ish", not exactly-once: the *execution* can still happen more than once (attempts, below) — only the *submission* is deduplicated.
- **LLM generation has no checkpoint** — background knowledge (verify independently if load-bearing): a partially-generated completion cannot be resumed mid-token-stream against a model that was unloaded; the KV cache is gone. Re-running the whole prompt is the only resume. The repo builds this in: `Client.Generate` discards the partial response on cancellation (`client.go` doc comment), and a preempted or restart-interrupted Job simply re-runs from scratch (`attempts++`, capped at `BROKER_JOB_MAX_ATTEMPTS`, default 3; restart recovery sweeps RUNNING→QUEUED@front). Transient interruptions (preemption, shutdown) requeue *without* burning an attempt — see the "prefer requeue causes over fail" classification in `worker.runJob`.
- **Retain-until-fetched retention**: results are kept until the consumer actually collects them. `GET /jobs/{id}/result` stamps `fetched_at` on first success (`sqlite.go` StampFetched); the prune sweep deletes SUCCEEDED Jobs only after `fetched_at + BROKER_JOB_FETCHED_GRACE` (default 1h), with an unconditional hard cap `BROKER_JOB_HARD_CAP` (default 168h) so never-fetched Jobs cannot accumulate forever.

**SQLite WAL in one paragraph.** Background knowledge (verify independently if load-bearing): SQLite's default rollback-journal mode blocks readers while a writer commits. WAL (write-ahead logging) mode appends changes to a separate `-wal` file instead of overwriting the database, so many readers proceed concurrently with the single writer; a `-shm` shared-memory index file coordinates them, and checkpoints fold the WAL back into the main file. That fits this workload exactly: one writer (the Job worker/API) plus concurrent readers (status polls, SSE, prune). The store opens with `_pragma=journal_mode(WAL)` and `busy_timeout(5000)` (`internal/job/sqlite.go`), and `.gitignore` excludes the `*.db-wal` / `*.db-shm` sidecars — if you see those files next to `jobs.db`, that is WAL working, not corruption.

## 5. HTTP mechanics that matter here

- **503 + Retry-After as backpressure**: HTTP 503 means "temporarily cannot serve"; the `Retry-After` header tells the client how long to wait. The Broker uses this as its *only* refusal mechanism for Synchronous requests — while Yielding, or when the wait budget (`BROKER_INTERACTIVE_WAIT`/`BROKER_BATCH_WAIT`) is exceeded, `deferRequest` in `internal/queue/gate.go` sets `Retry-After` (the wait budget, min 1s) plus `X-Broker-Status: deferred` and returns 503. The contract pushes queueing onto consumers deliberately: the Broker stays stateless for synchronous traffic (ADR-0002), and consumers that need durability use Jobs instead.
- **Streaming responses and HTTP trailers**: a chunked (streamed) HTTP response sends its headers *before* the body. Background knowledge: HTTP trailers are headers delivered *after* the body of a chunked response — the only standard place to put information that is unknowable at header time. A mid-stream Preemption is exactly that: when the response started, the outcome was unknown, so the header can only be optimistic. Hence a header/trailer pair, both named `X-Broker-Status`: header says `served` (optimistic), trailer carries the authoritative final outcome (`served` or `preempted`).
- **Go's `http.TrailerPrefix` mechanism**: Go's `net/http` promotes any response header set with the magic `Trailer:` key prefix into a real HTTP trailer, even when written after the handler streamed the body. `gate.go` (verified):

  ```go
  w.Header().Set("X-Broker-Status", "served")
  w.Header().Set(http.TrailerPrefix+"X-Broker-Status", "served")
  next.ServeHTTP(w, r.WithContext(ctx))
  ...
  w.Header().Set(http.TrailerPrefix+"X-Broker-Status", outcome) // "served" or "preempted"
  ```

  The in-code comment also covers the non-streamed case: a non-streamed request that is preempted fails *before any body* and surfaces as a 503, so its header is never wrong and no trailer is needed.
- **Why no in-band error marker**: preemption cancels the upstream context, which can cut the NDJSON stream *mid-line*. Injecting a JSON "preempted" object into a half-written line would corrupt the stream for any NDJSON parser. `docs/DESIGN.md` (verified): "On preemption the stream is cut with no in-band marker; a consumer detects it via the `X-Broker-Status: preempted` trailer, the `preempted` metric, or the absence of Ollama's terminal `{"done":true}`. (An injected in-band marker was rejected — cancelling mid-line would corrupt the NDJSON.)" Consumer rule of thumb: no `{"done":true}` line ⇒ the response is incomplete, whatever else you received.

## 6. Hardware/platform quirks (date-stamped, as of 2026-07-02)

- **AMD RX 9070 XT = RDNA4 / gfx1201** (Navi 48), released January 2025 — a *consumer* (RDNA) architecture, distinct from AMD's datacenter CDNA line (MI200/MI300). Source: `legacy/GPU-TROUBLESHOOTING.md` timeline.
- **Support-lag pattern**: brand-new GPU architectures trail in the ML stack. Ollama 0.13.5 could not see this card at all — logs showed `entering low vram mode, "total vram"="0 B"` — because it did not recognize gfx1201. The fix was upgrading Ollama to ≥ 0.16.3, which added RDNA4 support (`legacy/GPU-TROUBLESHOOTING.md`). Expect the same lag for any future-architecture upgrade: check the inference stack's supported-gfx list *before* blaming drivers.
- **`HSA_OVERRIDE_GFX_VERSION` exists but is not a cure-all**: it makes ROCm report a different gfx version (e.g. pretend gfx1201 is gfx1100/RDNA3). It was attempted pre-0.16.3 and failed — the troubleshooting doc's lesson: it can paper over *minor* version gaps but not major architecture changes (RDNA3→RDNA4). Do not reach for it before checking real support.
- **Infinity's prebuilt ROCm image targets MI200/MI300 (CDNA) only** — it does not support RDNA4, so the SigLIP embedding server runs on **CPU** here (ADR-0008, verified). That single fact shapes the Embed lane's whole design: CPU work must not consume the GPU scheduler slot, hence its own `queue.Scheduler` with a shared yield Controller.
- **Ground-truth GPU tools**: `rocm-smi` (utilization/VRAM/temps; `watch -n 1 rocm-smi` for live view) and `rocminfo | grep "Name.*gfx"` (which gfx architecture ROCm actually sees). When Ollama and the OS disagree about the GPU, these two are the arbiters — both were decisive in the 2026-02-21 incident.

## 7. SigLIP / Infinity essentials

Background knowledge (verify independently if load-bearing): SigLIP, like CLIP, is a **dual-tower** embedding model — a separate image tower and text tower trained so that an image and its matching caption land near each other in one shared vector space. The towers take different input types: the image tower eats pixels, the text tower eats tokens.

**The failure mode behind ADR-0008** (verified in the ADR): Infinity's unified OpenAI-style `POST /embeddings` endpoint tokenizes a base64 `data:` URI **as text**. Feed it images that way and every image comes back as a near-identical *text-tower* vector — the tower is embedding the string "data:image/jpeg;base64,/9j/4AA…", whose token prefix is nearly constant across images. The vectors are well-formed, the HTTP calls succeed, and similarity search silently returns garbage. This trap was actually hit here. Image embedding must target Infinity's `POST /embeddings_image`; the Broker's Embed lane rewrites the path (`internal/proxy`, `NewEmbed`) so Consumers keep the plain OpenAI `/embeddings` contract and can never fall into it.

**`--served-model-name` pinning** (ADR-0008): Infinity runs with `--served-model-name siglip-so400m-patch14-384` so the model id a Consumer sends — and stamps into its database next to every stored vector — is the frozen identifier, independent of the underlying HuggingFace path or serving stack. Embedding corpora are only comparable within one frozen model; pinning the served name is how "same model" stays provable after infrastructure changes.

## 8. Glossary handoff

`CONTEXT.md` at the repo root is the **enforced** vocabulary — each term has an *Avoid* list (say Broker not gateway, Yield not pause, Preemption not kill, Job not task, Consumer not client, …). Read it first; this section defines only terms CONTEXT.md lacks:

| Term | Definition |
|---|---|
| VRAM | GPU-attached memory holding model weights, KV cache, and game assets; a fixed pool with no OS-level preemption or swapping (section 1) |
| NDJSON | Newline-delimited JSON: one complete JSON object per line; Ollama's streaming wire format (sections 1, 5) |
| Trailer | HTTP header delivered after a chunked response body; the only standard carrier for an outcome unknown at header time (section 5) |
| WAL | SQLite write-ahead logging journal mode: concurrent readers + one writer, with `-wal`/`-shm` sidecar files (section 4) |
| Quantum | The batch min-run window (`BROKER_BATCH_QUANTUM`): time a running batch Job is protected from interactive Preemption (section 3) |
| Hysteresis | Requiring a detected state change to persist before acting on it; damps flapping, cannot fix detection circularity (section 2) |
| Dual-tower | Embedding architecture with separate image and text encoders sharing one vector space (SigLIP/CLIP) (section 7) |
| RDNA / CDNA | AMD's consumer (RDNA — this desktop's RX 9070 XT) vs datacenter (CDNA — MI200/MI300) GPU architecture lines; ML-stack support differs between them (section 6) |

## Provenance and maintenance

Repo-anchored claims and their one-line re-verification (run from the repo root, `/Users/prestonbernstein/dev/ollama-resource-broker`):

- Unload = list `/api/ps` then per-model `/api/generate` with `keep_alive: 0`: `grep -n "keep_alive" internal/ollama/client.go`
- `{"done":true}` terminal line handling: `grep -n "done" internal/ollama/client.go`
- V1/V2 circularity quote: `sed -n '104,112p' legacy/GO-MIGRATION-HANDOFF.md`
- Detection patterns and fail-open: `grep -rn "Plex Transcoder\|SteamLaunch" internal/detect/`
- Quantum rule + three properties (and the reversed-draft warning): `grep -n "quantum" docs/adr/0004-gpu-scheduling-policy.md internal/job/worker.go`
- Requeue-at-front mechanism: `grep -n "position_hint" internal/job/sqlite.go`
- Idempotency-Key required + UNIQUE: `grep -rn "Idempotency" internal/job/api.go internal/job/sqlite.go`
- Write-before-ack / retain-until-fetched: `grep -n "Write-before-ack\|retain-until-fetched" docs/DESIGN-jobs.md`
- WAL pragma + sidecars: `grep -n "journal_mode" internal/job/sqlite.go && grep -n "db-wal" .gitignore`
- TrailerPrefix usage and 503/Retry-After: `grep -n "TrailerPrefix\|Retry-After" internal/queue/gate.go`
- No in-band marker rationale: `grep -n "in-band" docs/DESIGN.md`
- RDNA4/0.16.3 incident and HSA_OVERRIDE failure: `grep -n "0.16.3\|HSA_OVERRIDE" legacy/GPU-TROUBLESHOOTING.md`
- Infinity CPU-only on RDNA4, path rewrite, `--served-model-name`: `grep -n "MI200\|embeddings_image\|served-model-name" docs/adr/0008-image-embedding-lane.md`
- Enforced vocabulary: `cat CONTEXT.md`

Volatile facts date-stamped 2026-07-02: Ollama/Infinity version-support statements and anything marked "background knowledge" may drift with upstream releases — re-verify against current upstream docs before relying on them in a change.
