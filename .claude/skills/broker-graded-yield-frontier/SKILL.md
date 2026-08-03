---
name: broker-graded-yield-frontier
description: The research frontier for the ollama-resource-broker — hybrid graded yield (run inference concurrently with light games, full-yield heavy games) plus the project's external positioning (what is novel vs known, what must be proven before any public claim). Load when asked "can inference run while gaming", "graded/partial/hybrid yield", "GPU utilization detection", "per-process GPU attribution", "what should this project research next", "is this novel / can we blog about it", or before designing any experiment that touches yield behavior. Everything here is OPEN/CANDIDATE — adoption requires an ADR through broker-change-control. NOT for how yield works today (broker-architecture-contract) or the history of why GPU-% detection failed (broker-failure-archaeology).
---

# Graded yield: the research frontier

**ASSUMPTION (A2, unconfirmed):** graded yield is framed as this project's "beyond state of the art" target. It is the only research-class item on the project's own roadmap. Confirm with Preston before investing serious effort.

The roadmap already names it (docs/DESIGN.md, "Roadmap (deferred)"): "**Hybrid graded yield** — run inference concurrently with light games (RimWorld), full-yield heavy games (Cyberpunk); needs hysteresis + non-circular GPU-% detection (the V1/V2 failure mode)." Nothing in this file is decided. Any behavior change lands as an ADR + experiment flag through `broker-change-control`.

## 1. Problem statement

Today's yield is binary: any detected game/transcode → ALL inference refused, VRAM force-unloaded. A light game (RimWorld-class) leaves most of an RX 9070 XT idle while every inference request 503s for the whole session. The wasted capacity is real; so is the danger:

- **Circularity (the V1/V2 corpse):** utilization-based detection self-triggers — the broker's own inference raises GPU%, which reads as contention, which stops inference, which drops GPU%, which resumes it. V2 added hysteresis and still failed, because hysteresis smooths oscillation but cannot tell WHO is using the GPU. Full history: `broker-failure-archaeology` (legacy/GO-MIGRATION-HANDOFF.md: "Detects WHAT is using resources, not THAT resources are high"). A corollary from the legacy blog record: `OLLAMA_KEEP_ALIVE=-1` once made an idle-but-resident model read as busy.
- **Flapping:** any threshold scheme oscillates near the threshold; graded yield needs hysteresis (asymmetric enter/exit thresholds + minimum dwell times) ON TOP of a non-circular signal, not instead of one.
- **Harm asymmetry:** a game stutter violates the project's prime invariant ("gaming absolute" — ADR-0003). A missed inference opportunity costs seconds of batch throughput. Therefore every classifier error must fail toward FULL yield, and the heavy-game path must stay exactly as fast as today (yield within ~2× `BROKER_DETECT_INTERVAL`).
- **VRAM is the sharper constraint than compute:** an 8B model resident under RimWorld may be harmless — until the game's own VRAM demand spikes. Reloading a model mid-session is exactly the stutter we must never cause. Residency policy (which models may stay loaded, when to preemptively unload) matters as much as admission policy.

## 2. Why this repo specifically could get a result (the assets)

1. **The broker owns ALL inference admission.** Unlike a generic monitor, it KNOWS when its own inflight > 0 (`/status queue.inflight`). It can sample GPU utilization only when it has zero in-flight work, obtaining an uncontaminated reading of everyone-else's load — a structural fix for circularity that V1/V2 could not have.
2. **Identity-based attribution is available on Linux/amdgpu.** Per-process GPU engine usage is exposed via `/proc/<pid>/fdinfo` (drm engine fields) and rocm-smi's per-process view. Attributing GPU time to the GAME's pid vs ollama's pid breaks circularity by identity rather than level. *Background knowledge — the exact field names/format on this kernel/driver are UNVERIFIED; verifying them on the desktop is literally Step 1 below.*
3. **The detector already yields identity.** `internal/detect` reports WHICH launcher matched (`gaming-steam`, `gaming-lutris`, ...) and the full cmdline is available — a per-game classification (light/heavy list) hangs naturally off the existing rules.
4. **One clean seam.** All yield state flows through `internal/yield.Controller` (Mode + effective bool + serve-context). A graded state extends one state machine, not five call sites. The embed lane already demonstrates "shares yield, different resource" — precedent for policies differentiated by resource.
5. **A prior-load calendar.** `internal/schedule` knows the weekly windows; graded admission can be schedule-aware (e.g. never graded during the Friday scraper window when batch pressure is highest).

## 3. Why the naive approaches fail (fence the known dead ends)

| Approach | Why it fails here |
|---|---|
| Raw GPU-% threshold to yield | Circular — proven fatal twice (V1, V2). Never re-add a utilization trigger that cannot exclude the broker's own load. |
| Hysteresis alone | Smooths flapping; cannot distinguish Ollama from a game. V2's exact mistake. |
| NVIDIA-style sharing (MPS/MIG/timeslicing) | Does not exist for consumer RDNA4/ROCm. Not portable here. *Background knowledge.* |
| Static per-game allowlist ONLY | Viable as a v1 (and recommended as the first graded mode) but decays — every new game needs manual classification; must be paired with a conservative default (unknown game = heavy). |
| "Just cap Ollama's VRAM" | Ollama offers keep_alive/num_gpu knobs, not a hard runtime VRAM ceiling per tenant; and compute contention remains. *Background knowledge — recheck against current Ollama before dismissing.* |

## 4. First three concrete steps (in this repo)

Each step is gated: hypothesis with predicted numbers WRITTEN BEFORE running (`broker-research-methodology`), log-only before behavior-changing, ADR before any default flips.

**Step 1 — Per-pid GPU attribution sampler (log-only, zero behavior change).**
New package (e.g. `internal/gpustat`) behind `BROKER_GPU_SAMPLE_INTERVAL` (unset = off, matching the INFINITY_URL optionality pattern). Every interval: read per-pid GPU busy (fdinfo or `rocm-smi --showpidgpus`-equivalent), tag each pid via the existing detect rules + "ollama" + "other", and emit one JSON log line: `{game_pct, ollama_pct, other_pct, broker_inflight, yielding}`. Validation: run on the desktop through normal use (a gaming evening, a scraper Friday). *You have a result when* a week of logs shows game-pid attribution that (a) is nonzero during known gaming sessions, (b) is zero for ollama-only load, and (c) `broker_inflight` correlates with ollama-pid GPU% — i.e. the circularity is measurably broken. If fdinfo attribution proves unreadable/unstable on this kernel → fall back to designing around "sample only when inflight==0" (asset 1) and record the dead end in `broker-failure-archaeology`.

**Step 2 — Offline light/heavy classification from real logs.**
Analyze Step 1 logs: for each game actually played, distribution of game-pid GPU% and VRAM headroom. Hypothesis to state up front, per game: "RimWorld-class sessions leave ≥ X% compute and ≥ Y GiB VRAM headroom sustained." Produces: a candidate light-game list + the hysteresis constants (enter-graded / exit-to-full thresholds + dwell) derived from observed variance, not guesses. Pure analysis — no repo behavior change; the analysis script and findings land in the repo (or an ADR appendix) for reproducibility.

**Step 3 — `ModeGraded` behind an experiment flag (default off).**
Extend `yield.Controller` with a graded state reachable ONLY when: flag on AND detected game is on the light list AND Step-1 sampler is running (the mode is signal-dependent by construction). Graded admission, most conservative first: batch-class embed lane only (CPU — zero GPU risk) → then small-model interactive with a VRAM budget. ANY heavy signal (heavy game detected, headroom below threshold, sampler stale) → full yield within one detect interval. Tests: unit-test the state machine with fake detector/sampler (no live experimentation — `broker-validation-and-qa`); live trial only as a scheduled, Preston-attended session.

**Falsifiable milestone (the "you have a result when"):** a ≥30-minute light-game session with graded yield ON shows (a) game frame-time p95 within N% of the same game with binary yield (frame data via mangohud or the game's own tooling — operator-assisted measurement), (b) ≥ M inference requests actually served during the session, and (c) a heavy-game launch mid-session triggers full yield within 2× `BROKER_DETECT_INTERVAL`. N and M are set in the Step-3 experiment design BEFORE the trial — the discipline is that they exist, not their particular values. Failing (a) at any N the humans can feel = the experiment failed; record it and stop.

## 5. External positioning (what may be claimed, and when)

Honest ledger as of 2026-07-02:

- **Engineering, not research (fine to describe, not to oversell):** an HTTP-fronting single-GPU arbiter with identity-based detection, tiered preemption + min-run quantum, durable restart-safe Jobs, and a yield-shared CPU embed lane is solid systems engineering on a home lab. Datacenter GPU sharing (MPS, MIG, timeslicing, Slurm/K8s schedulers) is well-trodden; the home/consumer-AMD/process-priority niche is under-documented rather than unexplored.
- **Candidate novelty (claimable only after the milestone):** graded GPU sharing between a live game and LLM inference on consumer AMD hardware, with non-circular attribution and a measured no-stutter bound. If the Step 1–3 chain lands with published logs, that is a strong blog-grade result (the repo already has one legacy blog draft: `legacy/BLOG-POST-FINAL.md` — any new post supersedes it and must meet this bar).
- **Reproducibility standard before ANY public claim:** scripted experiment, config snapshot (unit env + flag values), raw logs published alongside conclusions, and the failure archaeology of what did not work included. One mechanism must explain all observations, including the negatives (`broker-research-methodology`).
- Nothing enters README or a post while the graded work is at candidate status. Unproven = labeled open. This file is the only place the ambition is written down, deliberately.

## When NOT to use this skill

- How yield works TODAY → `broker-architecture-contract`.
- Why GPU-% failed historically → `broker-failure-archaeology`.
- Designing the experiment's evidence protocol → `broker-research-methodology`.
- The cutover/hardening backlog → `broker-cutover-hardening-campaign` (finish P1–P4 before starting Step 3 here; do not mix research and hardening in one change).

## Provenance and maintenance

Written 2026-07-02 against branch `v2-go`. Repo-anchored claims re-verify with:

```sh
grep -n "Hybrid graded yield" /Users/prestonbernstein/dev/ollama-resource-broker/docs/DESIGN.md
grep -n "V1\|V2\|circular" /Users/prestonbernstein/dev/ollama-resource-broker/legacy/GO-MIGRATION-HANDOFF.md | head -5
grep -rn "BROKER_GPU_SAMPLE_INTERVAL\|ModeGraded" /Users/prestonbernstein/dev/ollama-resource-broker/internal  # empty until Step 1/3 land
```

Hardware/driver claims (fdinfo fields, rocm-smi per-process support, Ollama VRAM knobs) are background knowledge, date-stamped 2026-07-02, and MUST be re-verified on the desktop before Step 1 is designed.
