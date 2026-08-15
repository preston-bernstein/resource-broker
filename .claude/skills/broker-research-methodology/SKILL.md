---
name: broker-research-methodology
description: How the resource-broker project investigates and proves things — the evidence bar (one mechanism must explain ALL observations including negatives; hypotheses predict numbers before running; assigned adversarial refutation), five reusable analysis recipes each with a worked example from this repo's real history (discriminating experiments, invariant-first reasoning, drift audits, grill-style failure-mode review, measurement-before-mechanism), and the idea lifecycle from experiment flag to adopted default or documented retirement. Load when designing an experiment, evaluating a hypothesis about broker behavior, reviewing a proposed mechanism/root-cause claim, asked "how do we know this is true", "design an experiment for X", "is this evidence good enough", or before writing an ADR that makes an empirical claim. NOT for domain theory (gpu-arbitration-reference) or what to measure with (broker-diagnostics-and-tooling).
---

# Broker research methodology

This project's history shows a consistent epistemic style: grills before designs, severity-graded independent review, ADRs that record rejected alternatives, and superseded decisions kept on the books. This skill codifies that style so cheaper sessions apply it deliberately. Where a narrative below is reconstructed from documents rather than a recorded play-by-play, it is labeled *(reconstruction)*.

## 1. The evidence bar

Three tests a mechanism/root-cause claim must pass before it is believed:

**(a) One mechanism explains ALL observations — including the negatives.**
Worked example *(reconstruction from legacy/GO-MIGRATION-HANDOFF.md and legacy/BLOG-POST-FINAL.md)*: V2's hypothesis was "false yields come from flapping; hysteresis will fix it." Hysteresis reduced oscillation and the false yields continued — the hypothesis never explained the core negative observation that yield triggered during steady inference with no game running. Only the circularity mechanism (the arbiter's own inference load reads as contention) explained every observation: the false triggers, why hysteresis didn't help, and the `OLLAMA_KEEP_ALIVE=-1` corollary (an idle resident model reading as busy). The rule: if your mechanism needs a second mechanism for the awkward observations, you don't have a mechanism yet.

**(b) The hypothesis predicts numbers/observables BEFORE the run.**
Write the prediction down first ("if the quantum is honored, an interactive request arriving mid-Job waits at most ≈ BROKER_BATCH_QUANTUM"). If the result needs post-hoc reinterpretation to fit, the hypothesis failed — record that, don't reinterpret. This is enforced structurally in `broker-graded-yield-frontier` Step 2–3 (N/M thresholds set at design time) and in `broker-validation-and-qa` ("a soak is finished when its exit criteria were written before it started").

**(c) Assigned adversarial refutation.**
Before adopting, someone is explicitly TASKED with breaking the claim — not invited, tasked. This is the project's real practice: design grills (`docs/DESIGN.md` is "output of a design grill"; the Job system came from "grill 2026-06-16"), a "Severe audit (grill-with-docs)" commit (d2b831d), and independent review producing severity-graded findings fixed in dedicated commits (529d075 "Independent-review fixes (H1-H3, M1, M3, M4, L2, L3)", 2a07b70). For an AI-session workflow: spawn or role-play a dedicated reviewer whose only goal is refutation, and let findings carry severity grades (H/M/L) per house style.

## 2. Analysis recipes

### 2.1 Discriminating experiment
*Use when two mechanisms explain the same symptom.* Find the observation only ONE candidate can produce; run the cheapest version of it.
Worked example *(reconstruction from ADR-0008)*: every image embedding came back near-identical. Candidates: (broken model/server) vs (images entering the TEXT tower — Infinity's unified `/embeddings` tokenizing the base64 data-URI as text). Discriminator: text-tower tokenization predicts similarity tracks the TEXT of the URI (two different images share the `data:image/png;base64,` prefix and base64 alphabet → near-identical), while a broken model predicts equally-degenerate output for text inputs too. Resolution: `/embeddings_image` returned distinct vectors → mechanism confirmed → broker-side path rewrite so no consumer can re-hit the trap. The recipe survives as a permanent instrument: `broker-diagnostics-and-tooling/scripts/embed-sanity.sh` (red vs blue PNG cosine check).

### 2.2 Invariant-first reasoning
*Use when choosing or reviewing a RULE (scheduling, retention, auth).* State the load-bearing PROPERTIES first; derive the rule; check the rule against each property. Worked example (documented in ADR-0004 itself): an earlier draft stated the batch-quantum rule in the OPPOSITE direction. Writing the three properties — bounded interactive added-latency ≈ quantum; guaranteed batch progress (anti-starvation); no reload thrash — exposed the inversion, and the ADR now records "the three properties above are the load-bearing intent and the 'min-run' framing is canonical." If you can't state what a rule must guarantee, you can't tell when it's backwards.

### 2.3 Drift audit
*Use when a fact lives in more than one place.* Enumerate every home of the fact (code default, README, repo unit file, LIVE unit, test) and diff them mechanically. Worked example (verified 2026-07-02): `BROKER_BATCH_WAIT` = 5s in `config.go`, 300s in `deploy/broker.service` and live; three Tdarr env vars live but absent from the repo unit AND the README table; unit name `broker.service` (README) vs `resource-broker.service` (live). Each divergence is either an undocumented decision (record it) or a bug (fix it) — never leave it unclassified. The scripted check lives in `broker-cutover-hardening-campaign` P2.5.

### 2.4 Failure-mode-first design review (the grill)
*Use BEFORE building.* Attack the design with "what breaks this" until the failure modes are enumerated; every survivor becomes an ADR with its rejected alternatives. Worked examples (documented): the 2026-06-16 grill killed the stateless-only model by asking what long batch work needs under restart/preemption — answer: position, status, durability — producing ADR-0006/0007 the same day (ADR-0002 superseded-not-deleted); ADR-0003 rejected graceful drain by asking "what does a 70b generation do to the game while draining?" (minutes of stall → violates gaming-absolute). A grill output is decisions + rejections, in writing — see `broker-docs-and-writing` for the format.

### 2.5 Measurement before mechanism
*Use always.* Never explain what you haven't measured; never let one measurement anchor you. The instruments and their interpretation guides are `broker-diagnostics-and-tooling`. Worked example (live, 2026-07-02): lane ports :11435/:11436 answered HTTP 000 while :11437/:11438 responded — outage-shaped. `/status` measurement: `busy:true, inflight:1`, yield off — mechanism: a long in-flight batch generation holds the single slot and even cheap `GET /api/tags` queues behind it (the Gate wraps every lane path). One GET distinguished "working as designed under load" from "outage"; no restart needed — and note the asymmetric cost had the eyeball diagnosis won: a restart would have killed the in-flight work to fix a non-problem.

## 3. Idea lifecycle

*(Codified from observed practice; the lifecycle as a named process is this skill's addition.)*

```
idea → grill (failure-mode review; decisions recorded)
     → ADR (decision + rejected alternatives; status line)
     → experiment flag (env var, default OFF; log-only where possible — the INFINITY_URL
       optionality pattern: unset changes nothing)
     → discriminating validation (predictions pre-registered; soak criteria written first)
     → ADOPT: flip default + update README/deploy/config docs (broker-config-and-flags checklist)
       via broker-change-control
     → or RETIRE: record in broker-failure-archaeology as symptom → root cause → evidence → status.
       Retirement with evidence is a RESULT, not a failure — V1 and V2 retirements bought the
       insight (identity over utilization) the whole design now rests on.
```

Where good ideas historically came from here: incident postmortems (batch-wait 300s came from a real ingestion failure), consumer pressure (the Job system came from internal-monitor-app's resilience needs), and grills (the entire v2 architecture). Speculative refactors with no forcing observation have no track record in this repo — treat them with suspicion.

## 4. Binding

Any empirical claim entering an ADR, the README, or an external post must have passed §1. The gates that enforce this are `broker-change-control` (process) and `broker-graded-yield-frontier` §5 (external claims). If a claim can't cite its discriminating observation, it ships labeled "candidate" or it doesn't ship.

## When NOT to use this skill

- You need domain facts, not method → `gpu-arbitration-reference`.
- You need an instrument → `broker-diagnostics-and-tooling`.
- You're triaging a live symptom under time pressure → `broker-debugging-playbook` first; methodology after the bleeding stops.

## Provenance and maintenance

Written 2026-07-02 against branch `v2-go`. Worked-example anchors re-verify with:

```sh
git -C /Users/prestonbernstein/dev/resource-broker log --oneline | grep -iE 'grill|review|audit'
grep -n "min-run" /Users/prestonbernstein/dev/resource-broker/docs/adr/0004-gpu-scheduling-policy.md
grep -n "embeddings_image" /Users/prestonbernstein/dev/resource-broker/docs/adr/0008-image-embedding-lane.md
grep -n "circular" /Users/prestonbernstein/dev/resource-broker/legacy/GO-MIGRATION-HANDOFF.md
```
