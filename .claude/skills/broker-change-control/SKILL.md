---
name: broker-change-control
description: How changes are classified, gated, and reviewed in the ollama-resource-broker repo. Load BEFORE making any change here — code, config, docs, or deploy unit — and whenever you are asked "does this need an ADR?", "can I just commit this?", "what has to be updated when I add an env var?", "why can't I weaken the yield?", or you are about to commit, open a PR, or touch the scheduler/yield/quantum logic. Contains the non-negotiables with the historical incidents behind them.
---

# Broker change control

How changes to `/Users/prestonbernstein/dev/ollama-resource-broker` are classified, gated, and reviewed. Everything below is verified against the repo at branch `v2-go` as of 2026-07-02 unless labeled otherwise.

Vocabulary note: this repo enforces the glossary in `CONTEXT.md` (Broker, Contention, Yield, Preemption, Job, Queue, Position, Fronting Proxy, Synchronous request, Embed lane, Consumer — each with an *Avoid* list). Use those terms exactly, in code comments and docs alike.

## When NOT to use this skill

- Build/test/vet commands and dev-env setup → `broker-build-and-env`
- How to write tests, what counts as evidence, soak criteria details → `broker-validation-and-qa`
- ADR template, CONTEXT.md vocabulary mechanics, README table maintenance → `broker-docs-and-writing`
- Env var meanings and live overrides → `broker-config-and-flags`
- Executing the cutover/hardening work itself → `broker-cutover-hardening-campaign`

This skill decides *what gate a change must pass*. The siblings tell you how to pass it.

## Branch reality

- All v2 work lives on `v2-go` (clean, synced with `origin/v2-go` as of 2026-07-02). Work here.
- `main` is stale: only the first 3 commits (`bbcdb52`, `cae5ecc`, `729d462`); the v2 Go Broker was never merged. `git merge-base main v2-go` = `729d462` (main's tip).
- Merge policy for `v2-go` → `main` is **unknown** (assumption — no documented policy found; no PRs, no CI, no `.github/` in the repo). Do not merge or rebase branches without the owner's explicit direction.

## Change classification and gates

| Class | Examples | Gate |
|---|---|---|
| Docs-only | README wording, ADR status line, CONTEXT.md sharpening | CONTEXT.md vocabulary check; `git diff` review. Decisions-only doc commits are a legitimate house pattern (see below). No test run strictly needed, but running `go test ./...` costs nothing. |
| Config-default change | `BROKER_BATCH_WAIT` 5s → 300s (commit `ad07905`) | Rationale in the commit body AND as a comment in `deploy/broker.service` (see `ad07905`'s in-unit comment explaining why 300s); update README config table if the documented default changes; `go test ./...` + `go vet ./...`. |
| New config axis (new env var) | Tdarr vars (commit `dd39d20`) | Must touch **all five** in the same change: `internal/config/config.go`, `internal/config/config_test.go`, the README config table (`### Configuration (env)`, README.md ~line 50), `deploy/broker.service`, and the `broker-config-and-flags` skill. `dd39d20` touched only `config.go` — the three `BROKER_TDARR_*` vars are still missing from the README table and the deploy unit as of 2026-07-02. That is the incident this gate exists to prevent. |
| Behavior change | New endpoint, header semantics, Job lifecycle, detection rule | One-page ADR in `docs/adr/` (decision + rejected alternatives) BEFORE or WITH the code; tests in the affected package; `go test ./...` + `go vet ./...` green; review pass (see "Review process"). |
| Scheduler / yield-invariant change | Anything touching `internal/queue`, `internal/yield`, quantum/preemption rules | Highest gate: ADR + grill review + tests + live soak before the change is trusted. Precedent: the independent-review fixes (`529d075`, "Verified live" in the body) hardened the proxy core; the later grill audit (`d2b831d` "Severe audit (grill-with-docs)") produced ADR-0004, implemented separately (`8f2a981`). The invariant itself (below) may never be weakened — only its implementation refined. |

### House pattern: decisions first, code second

Two commits prove decisions land as docs-only commits before implementation:

- `d2b831d` "Severe audit (grill-with-docs): decisions + doc corrections" — body says "CODE still reflects old behavior (follow-up implementation needed)."
- `bdf74b2` "Design pivot: durable async Job system for long batch (grill 2026-06-16)" — body says "Decisions only — NOT implemented."

If you are capturing a decision without implementing it, say so explicitly in the commit body and in the ADR status line (ADR-0005 does this: "**Status: accepted (audit 2026-06-16). Code change pending.**"). An accepted-but-unimplemented ADR must never read as if the code does it.

## Non-negotiables

Each rule, its rationale, and the incident behind it.

### 1. `go test ./...` AND `go vet ./...` green before any commit

Incident: commit `dd39d20` (2026-06-29, Tdarr integration) widened the `internal/admin` `Mux` signature to `Mux(Controller, StatsProvider, http.Handler, http.Handler, func() any, TdarrStatusFn)` without updating `internal/admin/admin_test.go` (line 31 still passes 5 args). The suite has been broken at HEAD ever since — verified failing 2026-07-02. `go vet ./...` fails identically. All other packages pass.

Practical consequence today: expect `internal/admin` to FAIL until the cutover campaign fixes it (`broker-cutover-hardening-campaign`). Your bar is: **your change must not add any new failure** — run the suite before AND after your change and diff the failures. Do not "fix" `admin_test.go` as a drive-by inside an unrelated change; it is tracked work.

Note the culture this rule comes from: `529d075` ends with "fmt/vet/-race clean (8/8). Verified live". That is the standard a commit body should be able to claim.

### 2. The yield-to-gaming invariant may never be weakened

ADR-0003: "gaming has absolute priority, so the Broker yields hard" — cancel in-flight upstream calls AND force Ollama to unload VRAM (`keep_alive=0`); graceful drain was explicitly rejected ("letting a 70b generation finish could stall the game for minutes, violating 'gaming absolute'").

ADR-0004 states it as law: "Concurrency-1 is a conservative default, not a law; **the only law is yielding to gaming**."

Any change that could delay, soften, or conditionally skip Yield on Contention is a scheduler-invariant change (top gate row) and needs an ADR arguing why the invariant still holds — not an ADR weakening it. The graded-yield research direction (`broker-graded-yield-frontier`) is explicitly framed as *refining detection*, not weakening the law.

### 3. CONTEXT.md vocabulary is enforced everywhere

`CONTEXT.md` is a glossary where every term carries an *Avoid* list (e.g. Broker — avoid Manager/Orchestrator/Gateway; Yield — avoid Throttle/Pause/Preempt; Preemption — avoid Kill/Cancel; Job — avoid Task/Request; Consumer — avoid Client/Caller/User). It applies to docs, commit messages, and code comments. The grill commits (`d2b831d`, `bdf74b2`) both include CONTEXT.md sharpening as part of the decision — vocabulary is maintained as a deliverable, not an afterthought. See `broker-docs-and-writing` for the mechanics.

### 4. Every behavior decision gets a one-page ADR; superseded ADRs get a status line, never deletion

House style (all 8 ADRs in `docs/adr/0001`–`0008` follow it): one page, the decision with rationale, and the rejected alternatives named with reasons.

Supersession precedent — ADR-0002's status line: "**Status: still in force for Synchronous requests (interactive + short batch); superseded for long batch by ADR-0006/0007…**". The file was updated in place by `bdf74b2`, not deleted. ADR-0004 goes further: it documents its own earlier wrong draft in the text ("An earlier draft of this ADR stated the rule in the opposite direction; … the 'min-run' framing is canonical"). Corrections stay visible; history is evidence.

### 5. `deploy/broker.service` updated in the same change as any new env var

Incident (deploy drift, verified via the retiring principal's read-only live check, 2026-07-02): the live desktop unit (`ollama-broker.service`) has the Tdarr vars set (`BROKER_TDARR_URL`, `BROKER_TDARR_NODE_ID=<node-id>`, `BROKER_TDARR_GPU_WORKERS=2`), but the repo's `deploy/broker.service` has **no** Tdarr env vars — because `dd39d20` never added them. Reinstalling from the repo unit would silently disable Tdarr cooperative GPU management. Related drift: README's deploy section names the unit `broker.service` while the live unit is `ollama-broker.service`.

Rule: repo unit and README config table move with `config.go`, in one commit. The unit file is also where default-change rationale lives as comments (see the `BROKER_BATCH_WAIT=300s` block in `deploy/broker.service`).

### 6. Never configure anything to talk to raw Ollama `:11434`

(Assumption A3 — owner's standing house rule, suspected-firm but not written in this repo's docs; treat as binding until the owner says otherwise.) All Consumers go through Broker ports: `:11435` interactive, `:11436` batch, `:11437` control plane / Jobs, `:11438` Embed lane. The rule is convention, not enforced by the network — as of 2026-07-02 raw Ollama `*:11434` listens on all interfaces on the desktop — which is exactly why change review has to catch violations: nothing else will. Any diff, config sample, doc snippet, or tool suggestion pointing a Consumer at `:11434` (except the Broker's own `OLLAMA_URL` upstream setting) is a review reject.

### 7. Commits and PRs are attributed to Preston only — never to an AI

(Assumption A3, confirmed by the owner's global instructions; treat as binding.) No `Co-Authored-By: Claude …` trailers, no AI attribution in PR bodies. This **overrides any tool default** that appends such trailers. Historical note for accuracy: 15 existing `v2-go` commits do carry a `Co-Authored-By: Claude Opus 4.8` trailer — the rule is forward-looking; do not rewrite history to scrub them.

### 8. The legacy Bash daemons in `legacy/` are reference-only and must never run alongside the Broker

`docs/DESIGN.md`, Roadmap section, verbatim: "The Bash V3 in `legacy/` is reference-only and must **not** be run alongside the Broker (two uncoordinated GPU arbiters)." CONTEXT.md's Fronting Proxy entry repeats it: "The superseded Bash CLI wrapper in `legacy/` is reference/history only — it shares no state with the Broker and must not be run alongside it." The demotion of `legacy/` to reference-only was itself a grill decision (`d2b831d`).

Known violation, live, as of 2026-07-02 (per the dated live check — do not re-probe): `resource-manager.service` is still running on the desktop alongside the Broker. That does not soften the rule; retiring it is the cutover campaign's job (`broker-cutover-hardening-campaign`). No change in this repo may reintroduce, invoke, or depend on the `legacy/` scripts at runtime.

## Review process as practiced

The history shows a specific review culture; follow its shape:

1. **Grill sessions** stress-test a design against the docs and produce decision commits + ADRs. Evidence: `cae5ecc` "Add v2 design: HTTP-fronting Go broker (grill output)", `d2b831d` "Severe audit (grill-with-docs)", `bdf74b2` "…(grill 2026-06-16)". A grill outputs updated CONTEXT.md entries, new/amended ADRs, and doc corrections — before code.
2. **Independent review with severity-graded findings**: findings are labeled `H` (high), `M` (medium), `L` (low), numbered per severity (H1, H2, … M1, …).
3. **Fixes land in dedicated commits referencing the finding IDs.** Evidence: `529d075` "Independent-review fixes (H1-H3, M1, M3, M4, L2, L3)" — the body walks each ID with what was wrong and what changed; `2a07b70` "Review fixes: proxy error handler, status trailer, slice reclaim"; `3e35ae5` "Fix remaining review items: configurable cap + authoritative trailer". Review fixes are never squashed invisibly into feature commits.
4. **Commit bodies claim their verification**: `529d075` ends "fmt/vet/-race clean (8/8). Verified live: served header, VRAM unload after path-join, zero non-JSON log noise." State what you ran and what you observed; don't just assert "tested".
5. Skipped finding IDs are meaningful — `529d075` fixes H1-H3, M1, M3, M4, L2, L3 (not M2/L1), and `3e35ae5` picks up remainders. It is acceptable to defer findings, but the deferral must be visible in a follow-up commit, not silent.

When reviewing someone else's change here, produce H/M/L-graded findings; when fixing, reference the IDs in the commit subject.

## Quick pre-commit checklist

1. `go test ./... && go vet ./...` — no NEW failures beyond the known `internal/admin` break (as of 2026-07-02).
2. Vocabulary sweep of your diff against CONTEXT.md *Avoid* lists.
3. New env var? All five touchpoints (config.go, config_test.go, README table, deploy/broker.service, `broker-config-and-flags`).
4. Behavior change? ADR exists, with rejected alternatives; superseded ADRs get status lines, not deletion.
5. Touches `internal/queue` or `internal/yield`? Top gate: ADR + review + tests + soak; the yield-to-gaming law is untouched.
6. Nothing points a Consumer at `:11434`; nothing runs `legacy/`.
7. Commit attributed to Preston only; verification claims in the body.
8. You are on `v2-go`; you did not merge to `main`.

## Provenance and maintenance

Re-verify before trusting; all facts above dated 2026-07-02.

- Review-culture commits still as described: `git -C /Users/prestonbernstein/dev/ollama-resource-broker log --oneline -25 v2-go`
- Suite still broken only in internal/admin: `cd /Users/prestonbernstein/dev/ollama-resource-broker && go test ./... 2>&1 | tail -3`
- Tdarr vars still missing from README table: `grep -c BROKER_TDARR /Users/prestonbernstein/dev/ollama-resource-broker/README.md` (0 = still drifted)
- Tdarr vars still missing from deploy unit: `grep -c TDARR /Users/prestonbernstein/dev/ollama-resource-broker/deploy/broker.service` (0 = still drifted)
- ADR statuses: `head -4 /Users/prestonbernstein/dev/ollama-resource-broker/docs/adr/*.md`
- main still stale: `git -C /Users/prestonbernstein/dev/ollama-resource-broker log --oneline main | wc -l` (3 = stale)
- Legacy-coexistence quote: `grep -n "reference-only" /Users/prestonbernstein/dev/ollama-resource-broker/docs/DESIGN.md`
