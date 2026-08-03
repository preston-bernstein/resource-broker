---
name: broker-failure-archaeology
description: Chronicle of every major investigation, dead end, rejected fix, and revert in the ollama-resource-broker project. Load when you need the HISTORY behind a behavior — why detection is process-name based and not GPU-%, why the X-Broker-Status trailer exists in its current form, why batch wait is 300s live but 5s in config, why ADR-0002 says "superseded", what broke the test suite at HEAD, why Ollama once reported "total vram=0 B" on the RX 9070 XT, why image embeddings once came back near-identical. Also load before repeating an experiment ("has this been tried?"), before writing an ADR that touches yield/detection/Jobs, or when a symptom smells like a known past incident. NOT for live triage — use broker-debugging-playbook for that.
---

# Broker failure archaeology

The permanent record of what went wrong in this project, what was tried, what was rejected, and where each thread stands. Every entry has four fields:

- **SYMPTOM** — what was observed.
- **ROOT CAUSE** — the mechanism, once found.
- **EVIDENCE** — file, commit, or doc you can open right now to verify.
- **STATUS** — `fixed` / `open` / `superseded` (a fix that was itself replaced).

Vocabulary follows `CONTEXT.md`: Broker (not gateway), Yield (not pause), Preemption (not kill), Job (not task), Consumer (not client), Contention.

## When NOT to use this skill

- Something is misbehaving **right now** → `broker-debugging-playbook` (symptom→triage).
- You want the current architecture and its invariants → `broker-architecture-contract`.
- You want to *extend* graded yield research → `broker-graded-yield-frontier` (it builds on entries A1 and A2 below).
- You want to *execute* the fixes for the open entries → `broker-cutover-hardening-campaign`.

---

## Part 1 — Open at HEAD (verified 2026-07-02)

### O1. Tdarr commit broke `go test ./...` at HEAD

- **SYMPTOM:** `go test ./...` and `go vet ./...` fail on branch `v2-go` HEAD: `internal/admin/admin_test.go:31` — `not enough arguments in call to Mux; have (Controller, fakeStats, http.HandlerFunc, nil, nil); want (Controller, StatsProvider, http.Handler, http.Handler, func() any, TdarrStatusFn)`. All other packages pass.
- **ROOT CAUSE:** Commit `dd39d20` (2026-06-29, "Add Tdarr cooperative GPU management + schedule awareness") added a sixth `TdarrStatusFn` parameter to `admin.Mux` (`internal/admin/admin.go:39`) but its diff never touched `internal/admin/admin_test.go`. The full suite was evidently not run before commit. The same commit created `internal/tdarr/` and `internal/schedule/` with **zero tests** — still true at HEAD.
- **EVIDENCE:** `git show --stat dd39d20` (no `admin_test.go` in the stat); `go test ./...` output; `/Users/prestonbernstein/dev/ollama-resource-broker/internal/admin/admin_test.go` line 31 (`newMux` passes 5 args).
- **STATUS:** **open** (re-verified 2026-07-02). Lesson on record: a cross-package signature change must run `go test ./...` before commit. Do not "fix" this casually — it is a tracked item in `broker-cutover-hardening-campaign`.

### O2. ADR-0005 control-plane auth accepted but never implemented

- **SYMPTOM:** `POST /control` on the control plane (`:11437`, bound to all interfaces on the live desktop) accepts unauthenticated mode changes. Any LAN device can `POST /control {"mode":"serve"}` (defeats yield — games stutter) or `{"mode":"yield"}` (trivial inference denial of service).
- **ROOT CAUSE:** The decision was made and documented but the code change never landed. `docs/adr/0005-control-plane-auth.md` has read "**Status: accepted (audit 2026-06-16). Code change pending.**" since commit `d2b831d` (2026-06-16). The designed mechanism — bearer token `BROKER_CONTROL_TOKEN`, loopback-only mutations when unset — appears nowhere in the source: `grep -rn "BROKER_CONTROL_TOKEN" --include='*.go' .` returns nothing (verified 2026-07-02).
- **EVIDENCE:** `docs/adr/0005-control-plane-auth.md`; the grep above; `git show d2b831d`.
- **STATUS:** **open** for ~16 days as of 2026-07-02. Implementation belongs to `broker-cutover-hardening-campaign`.

### O3. Cutover never finished: legacy V3 daemon still running beside the Broker

- **SYMPTOM:** As of the 2026-07-02 read-only live check (do not re-probe; take as dated fact), `resource-manager.service` (`/usr/local/bin/resource-manager.sh`, the Bash V3 daemon) is **still active** on the desktop alongside `ollama-broker.service`.
- **ROOT CAUSE:** The migration plan was explicitly "run alongside → cut consumers over one at a time → retire V3 only after a soak" (`README.md` ~line 124). The retire step was never executed. This directly violates the design: `docs/DESIGN.md` line 47 — the Bash V3 "must **not** be run alongside the Broker (two uncoordinated GPU arbiters)"; the "Severe audit" commit `d2b831d` demoted `legacy/` to reference-only for the same reason.
- **EVIDENCE:** `docs/DESIGN.md:47`; `README.md` Deploy section; live-deployment findings dated 2026-07-02 (shared authoring brief). Related drift found in the same check: live unit is `ollama-broker.service` while README says install as `broker.service`; repo `deploy/broker.service` lacks the Tdarr env vars the live unit sets.
- **STATUS:** **open**. This is the headline item of `broker-cutover-hardening-campaign`.

### O4. Config-vs-deploy drift class: batch wait budget fixed in the unit, not the default

- **SYMPTOM:** Bulk LightRAG ingestion (entity extraction + embeddings via the batch port `:11436`) fast-failed with `GPU busy: wait budget exceeded` whenever an interactive generation was mid-flight. Bulk backfills were effectively impossible during any interactive use.
- **ROOT CAUSE:** `BROKER_BATCH_WAIT` defaulted to 5s — tuned for "fail fast, Consumer retries" — but batch is non-real-time and should queue patiently behind interactive. A single interactive generation routinely outlasts 5s, so every concurrent batch waiter burned its budget and got 503.
- **FIX:** Commit `ad07905` (2026-07-01) raised the wait to 300s — **but only in `deploy/broker.service`** (the commit touches exactly that one file). The code default in `internal/config/config.go` is **still 5s**. Anyone running the binary without the unit file, or writing tests against defaults, gets the old failure mode back.
- **EVIDENCE:** `git show ad07905` (message + diff: `-Environment=BROKER_BATCH_WAIT=5s` / `+Environment=BROKER_BATCH_WAIT=300s` with rationale comment); `internal/config/config.go` default.
- **STATUS:** behavior **fixed in the live unit**; the drift (default ≠ deployed value) is **open** as a defect class. See `broker-config-and-flags` for the full default-vs-live table.

---

## Part 2 — V2 Go Broker era (2026-06-15 → 2026-07-01), resolved

### G1. Infinity SigLIP text-tower trap: every image embedded near-identically

- **SYMPTOM:** Image embeddings requested through Infinity's unified OpenAI `POST /embeddings` endpoint returned **near-identical vectors for every image** — similarity search useless.
- **ROOT CAUSE:** Infinity's unified `/embeddings` endpoint tokenizes a base64 `data:` URI as **text** and runs it through SigLIP's *text tower*. Every long base64 string looks alike as text, so every "embedding" was effectively the same text vector. Image embedding must target Infinity's dedicated `POST /embeddings_image`.
- **FIX:** Rather than teach every Consumer an Infinity-specific path, the Broker's embed lane (`internal/proxy.NewEmbed`, port `:11438`) presents the standard OpenAI `/embeddings` (and `/v1/embeddings`) face and **rewrites the path** to `/embeddings_image` on the way upstream; bodies untouched. Consumers keep the model-agnostic OpenAI wire contract.
- **Bonus finding from the same work:** Infinity's prebuilt ROCm image supports MI200/MI300 only — NOT RDNA4 (gfx1201) — so Infinity runs on **CPU** on this box. That is why the embed lane has its OWN `queue.Scheduler` (CPU work must not consume the GPU slot) while sharing the yield Controller.
- **EVIDENCE:** `docs/adr/0008-image-embedding-lane.md` ("tokenizes a base64 `data:` URI as **text** (every image returns a near-identical text-tower vector)"); commit `e1304ec` (2026-06-30).
- **STATUS:** **fixed** by design (path rewrite is load-bearing; removing it silently reintroduces the trap with no error).

### G2. Stateless-only design proved too weak → same-day pivot to durable Jobs

- **SYMPTOM:** The v2 stateless model (ADR-0002: bounded wait → 503 + `Retry-After`, Consumer owns retry) could not serve long, resilience-critical batch work (multi-item scoring runs, vision): no queue Position, no live status, no restart survival — a multi-hour gaming session simply outlasts any HTTP connection.
- **ROOT CAUSE:** Design assumption failure, not a code bug: "every consumer tolerates a deferred LLM" held for short work but long batch needs durable queueing and observability the stateless path cannot provide by construction.
- **FIX:** Same-day design pivot on 2026-06-16: `bdf74b2` (decisions only — "NOT implemented. Code still runs the v2 stateless model") wrote ADR-0006/0007 + `docs/DESIGN-jobs.md`; `8f2a981` implemented the full durable Job system (`internal/job/`: SQLite WAL store, `QUEUED→RUNNING→SUCCEEDED|FAILED|CANCELED`, mandatory `Idempotency-Key`, preempted Jobs requeue at the FRONT, restart re-runs `RUNNING` Jobs with `attempts++` capped at 3) plus the ADR-0004 scheduler upgrade — ~2,600 lines in one commit.
- **House style note:** ADR-0002 was **not deleted**; its Status line was updated to "still in force for Synchronous requests… superseded for long batch by ADR-0006/0007." Superseded decisions stay on record with a pointer.
- **EVIDENCE:** `docs/adr/0002-stateless-http-bounded-wait.md` (status line); `docs/adr/0006-durable-job-system.md`, `docs/adr/0007-job-durability-and-restart.md`; `git show --stat bdf74b2` and `git show --stat 8f2a981`.
- **STATUS:** **superseded-in-part by design** (ADR-0002 remains correct for interactive + short batch; Job path owns long batch). Working as designed since 2026-06-16.

### G3. ADR-0004 quantum rule: an earlier draft stated it backwards

- **SYMPTOM:** An earlier draft of ADR-0004 stated the batch-quantum preemption-protection rule in the **opposite direction** (i.e., inverted who is protected when).
- **ROOT CAUSE:** Writing the rule before pinning down what it must achieve. The rule only disambiguates itself once its three load-bearing properties are explicit: (1) bound interactive added-latency to ≈ one quantum, (2) guarantee batch *progress* (every Job gets at least a quantum of GPU before it can be bumped — steady interactive load cannot starve batch), (3) prevent model-reload thrash from interactive bursts.
- **FIX:** The canonical framing is **min-run**: a running batch Job is *protected from interactive Preemption for its first quantum* (`BROKER_BATCH_QUANTUM`, default 10s); after that, a waiting interactive request preempts it and the Job requeues at the front. The ADR itself records the flip: "(An earlier draft of this ADR stated the rule in the opposite direction; the three properties above are the load-bearing intent and the 'min-run' framing is canonical.)"
- **LESSON (reusable):** for any scheduling/priority rule, write the load-bearing properties first, then derive the rule — a rule stated without its properties can be inverted and still *sound* plausible.
- **EVIDENCE:** `docs/adr/0004-gpu-scheduling-policy.md`, point 3 (the parenthetical is the self-recorded correction); enforced in `internal/job/worker.go`.
- **STATUS:** **fixed**; the correction is permanently documented inside the ADR.

### G4. The X-Broker-Status trailer saga: shipped → reverted → reinstated correctly

The one genuine ship-revert-reship chain in the repo. Three commits, all 2026-06-16:

- **Round 1 — `2a07b70`:** `X-Broker-Status` sent as an HTTP trailer with the definitive outcome (`served`/`preempted`), because mid-stream Preemption cannot be known at header-write time.
- **Round 2 — `529d075` (independent review, item H2): REVERTED.** The predeclared trailer "was never delivered (Content-Length responses can't send trailers; chunked also dropped it)." Fell back to a plain optimistic header; preemption visible only via truncated body + metric. Round 1 had shipped a mechanism that silently did nothing.
- **Round 3 — `3e35ae5`:** reinstated correctly using Go's `http.TrailerPrefix` mechanism — keep the optimistic `served` header AND emit the true final outcome as a trailer, "verified delivered on chunked streamed responses (header AND trailer both arrive)." Non-streamed preemption still surfaces as a 503.
- **ROOT CAUSE of the round-1 failure:** wrong Go trailer API for the response shape; and the defect was invisible without checking the wire (the code compiled, tests passed, the trailer just never arrived).
- **EVIDENCE:** `git log -1 2a07b70`, `git log -1 529d075` (H2 text), `git log -1 3e35ae5`; current behavior documented in `README.md`/`docs/DESIGN.md`.
- **STATUS:** **fixed** (round 3 is current). Consumer-facing consequence still true today: a preempted stream is cut with NO in-band marker — detect via the trailer or the absence of Ollama's terminal `{"done":true}` line.

### G5. What independent review caught (the H/M/L commit chain)

Not one incident but a recorded pattern: this repo runs "grill" reviews whose findings are graded High/Medium/Low and fixed in dedicated commits. The classes of defect that review — not testing, not operation — caught on 2026-06-16:

| Commit | Defect classes caught |
|---|---|
| `2a07b70` | Proxy `ErrorHandler` missing → non-JSON `http: proxy error` noise on stderr at every game start (context.Canceled is *normal* on Yield); raced status flag → read outcome deterministically from serve context; scheduler retained backing-array references for dequeued waiters (memory growth under sustained queuing) |
| `529d075` | H1: Ollama client didn't drain response bodies before close → CLOSE_WAIT/TCP-reuse leak across unload cycles; H2: trailer never delivered (see G4); H3: `os.Exit()` in a goroutine skipped graceful shutdown; M1: unbounded waiter queue → per-class cap (256) → fast `ErrQueueFull` 503; M3: pre-header cancellation wrote no status; M4: logging under the controller lock; L3: base-URL path prefix dropped (sub-path proxies broke VRAM unload) |
| `3e35ae5` | Hardcoded waiter cap → `BROKER_MAX_WAITERS`; trailer reinstated via `http.TrailerPrefix` |
| `d2b831d` | Doc/decision drift: DESIGN.md claimed a "truncation marker" that never existed (removed); tiered Preemption and control-plane auth captured as ADR-0004/0005 (the latter still unimplemented — see O2) |

- **EVIDENCE:** `git log -1 --format=%B` on each SHA; graded-item lists are in the commit messages verbatim.
- **STATUS:** all listed code items **fixed** same day except ADR-0005 implementation (**open**, O2). Meta-lesson: the leak/noise/raced-flag class was invisible to unit tests and found only by adversarial reading — the review culture is load-bearing (see `broker-change-control`).

---

## Part 3 — Legacy Bash era (pre-2026-06-15) — the origin stories

Reference material lives in `legacy/` (imported scrubbed as commit `bbcdb52`). Historiography warning: legacy docs record beliefs *of their era* — `legacy/BLOG-POST-FINAL.md` calls GPU-% + hysteresis the "Final Form" (V2) and its "Lessons Learned" even recommends measuring GPU % over process names; V3 later reversed exactly that. When legacy docs conflict, `legacy/GO-MIGRATION-HANDOFF.md` is the latest and canonical.

### L1. The central failure: GPU-% detection is circular (V1 → V2 → V3)

- **SYMPTOM:** The resource manager killed Ollama's own batch jobs. An Ollama job pushing the GPU to 60% read as "Gaming detected!" — the manager preempted the very workload it existed to schedule.
- **ROOT CAUSE:** Circular logic. Detection asked *"is the GPU busy?"* — but Ollama is itself the biggest GPU consumer, so its own load was indistinguishable from Contention. V2 added hysteresis (30s sustained to enter gaming state, 5min to exit — genuinely useful against loading-screen/menu flapping) but hysteresis only filters *noise in the signal*; it cannot fix a signal that is *measuring the wrong thing*. V2 still could not tell Ollama from gaming.
- **FIX (V3, carried verbatim into the Go Broker):** switch the question from *THAT* resources are high to **WHAT is using them** — process-name detection (`pgrep -f "SteamLaunch AppId="`, `"Plex Transcoder"`, Lutris/Heroic/Wine patterns). With identity known, Ollama may legitimately use 100% GPU. Ported to `internal/detect` with first-match-wins ordering.
- **EVIDENCE:** `legacy/GO-MIGRATION-HANDOFF.md` §"Process-Based Detection (Not GPU %)" (~line 105): "V1: GPU % monitoring → Failed when Ollama uses GPU (circular logic); V2: GPU + hysteresis → Still couldn't distinguish Ollama from gaming; V3: Process detection → … Detects WHAT is using resources, not THAT resources are high." The failure transcript is in `legacy/BLOG-POST-FINAL.md` ~lines 360–375; the V2 artifacts are `legacy/archive/resource-manager-v2.sh` and `legacy/archive/RESOURCE-MANAGER-UPGRADE-V2.md`.
- **STATUS:** **fixed by design** — and this is the single most load-bearing decision in the codebase. **BUT the problem returns:** the roadmap item "hybrid graded yield" (inference concurrent with light games, full Yield for heavy games — `docs/DESIGN.md:45`) explicitly requires "hysteresis + non-circular GPU-% detection (the V1/V2 failure mode)". Any graded-yield work must first solve what V1/V2 could not. See `broker-graded-yield-frontier`.

### L1a. Corollary incident: `OLLAMA_KEEP_ALIVE=-1` made an *idle* model read as gaming

- **SYMPTOM:** With a model kept loaded permanently, `/sys/class/drm/card1/device/gpu_busy_percent` read **100% while Ollama did nothing** (rocm-smi: 92W vs 44W idle). GPU-% gaming detection was completely broken and ~52W wasted continuously.
- **ROOT CAUSE:** Premature optimization ("keep model loaded = instant responses") interacting with the circular signal of L1: a resident model on this AMD stack reports full busy. Fix was on-demand loading (default 5-minute keep-alive).
- **EVIDENCE:** `legacy/BLOG-POST-FINAL.md` ~lines 290–320 (transcript: `ollama ps` shows model loaded "Forever", `gpu_busy_percent` = 100).
- **STATUS:** **fixed** in its era; **still relevant** — any future GPU-% signal must account for resident-model busy readings, and the Go Broker's hard Yield deliberately forces VRAM unload (`keep_alive=0`, ADR-0003) partly for this family of reasons.

### L2. 2026-02-21: Ollama 0.13.5 cannot see the RX 9070 XT ("total vram=0 B")

- **SYMPTOM:** After enabling GPU support, Ollama logged `msg="entering low vram mode" "total vram"="0 B"` and ran CPU-only, despite the GPU working fine for everything else.
- **DEAD ENDS (both documented as failed):**
  1. Adding the `ollama` user to `render`/`video` groups (`sudo usermod -aG render ollama` etc.) — permissions were never the problem.
  2. `HSA_OVERRIDE_GFX_VERSION=11.0.0` — masquerade gfx1201 (RDNA4) as gfx1100 (RDNA3). Useless because the runtime itself predated the architecture.
- **ROOT CAUSE:** Version gap, not configuration. The RX 9070 XT is RDNA4 / gfx1201 / Navi 48 — an architecture newer than Ollama 0.13.5's ROCm support. No environment variable can teach an old binary a new GPU.
- **FIX:** Upgrade Ollama to **>= 0.16.3** (RDNA4 support). Verified in the doc's aftermath section: full VRAM detected, gfx1201 supported.
- **LESSON (from the doc itself):** check the software-release vs hardware-release timeline *first*; don't assume `HSA_OVERRIDE_GFX_VERSION` works — it helps only when the runtime already supports a *similar* architecture.
- **EVIDENCE:** `legacy/GPU-TROUBLESHOOTING.md` (dated 2026-02-21; Attempts 1–2 marked failed; fix and verification at the end).
- **STATUS:** **fixed** (Ollama upgrade). Keep for the diagnostic pattern — "new AMD GPU + `total vram=0 B`" recurs with every new RDNA generation.

### L3. Small but permanent lessons from the Bash era

- **Runtime memory ≠ file size:** `MemoryMax=2G` cgroup throttle OOM-killed `llama3.2:3b` — 2.0GB on disk but 2.5GB during inference. Fix: 3G. Lesson: size limits from *measured runtime* peaks, never file size. (`legacy/BLOG-POST-FINAL.md` ~lines 155–170.)
- **Shebang at byte 0:** systemd exit `203/EXEC` because a nano paste indented the script — shebang had leading whitespace. Fixed with `sed -i 's/^  //'`. (`legacy/WORKING-GUIDELINES.md` ~line 452.)
- **Hysteresis caveat:** hysteresis was validated for loading screens (GPU dips to 0%), but many modern games keep GPU high even in menus — so exit-hysteresis rarely triggers there. Relevant again for graded yield. (`legacy/BLOG-POST-FINAL.md` ~line 684.)

---

## How to add an entry

This file is **append-only history**. Never rewrite or delete an existing entry; if its status changes, edit only its STATUS line (e.g., `open` → `fixed by <sha>, YYYY-MM-DD`) and add a dated note. New entries require:

1. All four fields (SYMPTOM / ROOT CAUSE / EVIDENCE / STATUS). No entry without a citation someone can open — a commit SHA, a repo file path with approximate line, or a doc of record. "I remember" is not evidence.
2. Date-stamp anything volatile ("as of YYYY-MM-DD").
3. If the entry records a *rejected* approach, say explicitly why it failed — rejected fixes are the most valuable part of this file.
4. Place it: open issues in Part 1, resolved Go-era in Part 2, legacy in Part 3.
5. If it changes what siblings should say (e.g., a new trap for `broker-debugging-playbook`), cross-reference by skill name; don't duplicate the story there.

## Provenance and maintenance

All claims verified 2026-07-02 on branch `v2-go` at `ad07905`. Re-verify with:

- Test breakage still open? `cd /Users/prestonbernstein/dev/ollama-resource-broker && go test ./... 2>&1 | grep -E 'FAIL|no test files'` (expect `internal/admin` FAIL, `internal/tdarr`/`internal/schedule` no test files — until fixed).
- ADR-0005 still unimplemented? `grep -rn "BROKER_CONTROL_TOKEN" --include='*.go' /Users/prestonbernstein/dev/ollama-resource-broker` (empty = still open) and check the Status line of `docs/adr/0005-control-plane-auth.md`.
- Batch-wait drift still present? `grep -n 'BROKER_BATCH_WAIT' /Users/prestonbernstein/dev/ollama-resource-broker/internal/config/config.go /Users/prestonbernstein/dev/ollama-resource-broker/deploy/broker.service` (drift = default 5s vs unit 300s).
- Legacy daemon still running live? Requires an authorized check of `resource-manager.service` on the desktop — see `broker-cutover-hardening-campaign`; do not assume this file's 2026-07-02 snapshot holds.
- Commit citations: `git -C /Users/prestonbernstein/dev/ollama-resource-broker show --stat <sha>` for any SHA above (`dd39d20`, `ad07905`, `e1304ec`, `bdf74b2`, `8f2a981`, `2a07b70`, `529d075`, `3e35ae5`, `d2b831d`).
- New history since this file was written? `git -C /Users/prestonbernstein/dev/ollama-resource-broker log --oneline ad07905..v2-go` — anything listed is not yet chronicled here.
