---
name: broker-docs-and-writing
description: The ollama-resource-broker repo's docs of record, enforced vocabulary, and writing templates. Load BEFORE writing or editing any prose in this repo — an ADR, CONTEXT.md entry, DESIGN doc, README section, commit message, or a new .claude/skills/ file — and whenever you are asked "where does this get documented?", "what's the ADR format here?", "how do I mark an ADR superseded?", "what do I call this thing?" (naming a component/state/request), "update the README config table", or you catch yourself typing gateway, task, kill, pause, client, or passthrough. Contains the full CONTEXT.md Avoid-list vocabulary, the ADR template derived from the 8 real ADRs, and the commit-message house style.
---

# Broker docs and writing

How prose is written in `/Users/prestonbernstein/dev/ollama-resource-broker` (branch `v2-go`): which document owns which kind of fact, the enforced vocabulary, and fill-in templates for ADRs, design docs, README maintenance, commit messages, and skill files. Everything below is verified against the repo as of 2026-07-02 unless labeled otherwise.

## When NOT to use this skill

- Deciding whether a change **needs** an ADR, what gate it must pass, review process → `broker-change-control`. This skill tells you how to *write* the ADR once that decision is made.
- Env var meanings, defaults, live overrides → `broker-config-and-flags`
- Why the architecture is shaped the way it is → `broker-architecture-contract`
- Build/test commands → `broker-build-and-env`; deploy/operations → `broker-run-and-operate`

## Docs-of-record hierarchy

Four documents of record, in precedence order. On any conflict between them, the higher one wins and the lower one gets fixed.

| Rank | Document | It is FOR | Must NEVER contain |
|---|---|---|---|
| 1 | `CONTEXT.md` | The glossary — **enforced vocabulary**. One entry per recurring noun: definition + *Avoid* list. | Design narratives, decisions, config values, procedures. It defines words, nothing else. |
| 2 | `docs/adr/NNNN-*.md` | Decisions. One decision per file: what was decided, why, what was rejected and why, current status. | Tutorials, config reference, vocabulary forks. An ADR uses CONTEXT.md terms; it never redefines them. |
| 3 | `docs/DESIGN.md`, `docs/DESIGN-jobs.md` | Design narratives with status lines: the architecture story, a decision table pointing at ADRs, and a roadmap of deferred items. | Authoritative decision text. A design doc *summarizes* a decision and cites its ADR; the ADR is the record. |
| 4 | `README.md` | Operator-facing summary: what it does, build/run, config table, control-plane and Job API examples, consumer port map, deploy steps. | Decisions or rationale beyond a one-line pointer ("ADR-0004"), vocabulary definitions (it links CONTEXT.md as "the glossary"). |

Two non-record files to know about:

- `docs/BUILD-PLAN.md` — **gitignored, local-only** (its own header says so; mirrored to the owner's vault). Milestone plan. Never cite it as a doc of record; never assume a fresh clone has it.
- `legacy/` docs — reference/history only (migration handoff, GPU troubleshooting). Quote them as evidence of past incidents; never as current behavior.

Routing rule: a **word** goes in CONTEXT.md, a **decision** goes in an ADR, a **story** goes in a DESIGN doc, an **operator fact** goes in README. Decisions do not live in README; vocabulary does not fork into other files.

## Vocabulary discipline

`CONTEXT.md` is enforced everywhere — docs, code comments, commit messages, log/metric naming discussions, and these skill files. Full term list as of 2026-07-02 (definitions abridged; the file is authoritative):

| Term | Means | *Avoid* |
|---|---|---|
| **Broker** | The arbiter between inference callers and Ollama; single source of truth for GPU access | Manager, Orchestrator, Gateway |
| **Contention** | A high-priority claim on the GPU (gaming or Plex transcode), detected by process name | Load, Pressure, Busy |
| **Yield** | The whole-Broker state of admitting no inference so gaming/Plex gets 100% of the GPU | Throttle, Pause, Preempt (preempt is the act on one job; yield is the whole-Broker state) |
| **Preemption** | Interrupting a running lower-priority request for a higher-priority claimant; priority gaming/Plex > interactive > batch | Kill, Cancel |
| **Job** | A durable unit of long batch inference: id, lifecycle, survives restart; distinct from a Synchronous request | Task, Request |
| **Queue** | The durable, ordered line of Jobs awaiting the GPU; a preempted Job returns to the front | Backlog, Buffer |
| **Position** | A Job's 1-based place among batch Jobs ahead of it; pairs with a clearly-soft ETA, never a hard wait guarantee | Rank, Slot, Place |
| **Fronting Proxy** | The Broker's synchronous HTTP entry point: speaks Ollama's own API, applies yield/priority, streams live | Gateway, Reverse Proxy (generic), Shim |
| **Synchronous request** | Inference handled live through the Fronting Proxy — admitted or 503'd, streamed, never persisted | Sync call, Passthrough |
| **Embed lane** | The optional second upstream (`:11438` fronting Infinity SigLIP, ADR-0008): own scheduler, shared Yield controller | Embedding proxy, CPU broker |
| **Consumer** | Any service sending inference through the Broker (internal-monitor-app, LightRAG, internal-scraper-service, ad-hoc CLI) | Client, Caller, User |

Rules:

1. **Every new noun that recurs gets a CONTEXT.md entry, WITH an *Avoid* list.** The Avoid list is not optional — it is what makes the vocabulary enforceable in review. Format matches the existing entries exactly: `**Term**:` bold, definition paragraph, `_Avoid_: A, B, C`.
2. Vocabulary is maintained as a **deliverable of design work**, not an afterthought: the grill commits `d2b831d` and `bdf74b2` both ship CONTEXT.md sharpening alongside the decision (e.g. `bdf74b2` added Job, Queue, Position, Synchronous request and *reframed* Fronting Proxy when the Job system changed what the proxy meant).
3. When a decision changes what a term means, the CONTEXT.md entry is edited in the same commit as the ADR — the glossary never lags the decisions.
4. A vocabulary sweep of your diff against the *Avoid* lists is a pre-commit checklist item (see `broker-change-control`).
5. Some Avoid words are unavoidable in narrow technical senses — `context.Canceled`, HTTP "request", Go type names. The rule governs *your prose naming the domain concept*, not upstream API identifiers. When in doubt, use the CONTEXT.md term.

## ADR house style

Derived from the 8 real ADRs in `docs/adr/` (0001–0008). Study `0004-gpu-scheduling-policy.md` and `0006-durable-job-system.md` as the fullest exemplars.

**Filename**: `NNNN-kebab-title.md`, zero-padded four digits, next free number.

**Title is the decision**, stated as an imperative or declarative sentence — not a topic label:

- `# Arbitrate inference via an HTTP-fronting proxy, written in Go` (0001)
- `# HTTP path is stateless: bounded wait then 503, consumers own retry` (0002)
- `# Yield is hard: cancel in-flight and force Ollama to unload VRAM` (0003)

Someone reading only `ls docs/adr/` plus the titles should know every decision in force.

**Status line**: first body line, bold, with dates, provenance (which audit/grill), and implementation pointers into the code:

- `**Status: accepted (audit 2026-06-16); implemented 2026-06-16 — supersedes the implicit "concurrency-1, gaming-only preemption" model.**` (0004)
- `**Status: accepted (design grill 2026-06-16); implemented 2026-06-16 in `internal/job/`. Supersedes ADR-0002 for long batch.**` (0006)
- `**Status: accepted (audit 2026-06-16). Code change pending.**` (0005 — accepted-but-unimplemented is stated plainly, never left to read as if the code does it. Note: 0005 is *still* pending as of 2026-07-02.)

(Historical wrinkle: ADRs 0001 and 0003 predate the 2026-06-16 audit and carry no Status line; 0002 gained one retroactively via its supersession amendment. Every ADR since carries one. New ADRs must.)

**Body**: dense prose paragraphs — no Context/Decision/Consequences bullet ceremony. State the forcing problem, the decision, the mechanism, the accepted costs. Bulleted sub-points appear only when the decision genuinely has enumerable parts (0004's three policy rules; 0007's four durability guarantees).

**Rejected alternatives are INLINE, each with its reason** — never a bare list:

> We rejected holding connections until served (file-descriptor leaks, client timeouts fire anyway) and a durable SQLite request queue (an HTTP request/response cannot be replayed after the caller's connection is gone). (0002)

0004 uses the variant `Alternatives rejected: X (reason); Y (reason); Z (reason).` Either form is fine; the reason in parentheses is not.

**Supersession: status amendment, NEVER deletion.** The exemplar is ADR-0002's amended status line, quoted verbatim:

> **Status: still in force for Synchronous requests (interactive + short batch); superseded for long batch by ADR-0006/0007, which add a durable async Job path.** The stateless model proved too weak once many services and long, resilience-critical workloads (multi-item scoring, vision) needed position/status feedback and restart survival — see ADR-0006.

That amendment landed in commit `bdf74b2` by editing the file in place. Note it is *scoped* — the ADR says exactly which part still holds. ADR-0004 goes further and keeps its own earlier mistake visible in the body: "(An earlier draft of this ADR stated the rule in the opposite direction; the three properties above are the load-bearing intent and the 'min-run' framing is canonical.)" Corrections stay visible; history is evidence.

**Length**: one page maximum. If it doesn't fit, it is more than one decision — split it (0006/0007 are a decision and its durability-semantics companion, cross-referenced as "Companion to ADR-0006").

### ADR template

```markdown
# <The decision, as an imperative/declarative sentence>

**Status: accepted (<grill|audit|review> YYYY-MM-DD)<; implemented YYYY-MM-DD in `internal/<pkg>/` | . Code change pending>.<
Supersedes/amends note if any.>**

<Forcing problem: what broke or what need appeared, named concretely — which
Consumer, which workload, which incident. Then the decision and its mechanism,
in prose, using CONTEXT.md terms. State accepted costs explicitly ("The cost
is X — an acceptable trade for Y").>

<Optional: numbered/bulleted rules ONLY if the decision has enumerable parts.>

We rejected <alternative A> (<reason>), <alternative B> (<reason>), and
<alternative C> (<reason>).
```

When your ADR supersedes an earlier one: edit the old ADR's Status line in the same commit, scoped like ADR-0002's ("still in force for X; superseded for Y by ADR-NNNN"). Do not delete or rewrite its body.

## Design doc style

Two exemplars: `docs/DESIGN.md` (v2 design) and `docs/DESIGN-jobs.md` (durable Job system).

**Status header** — first body line: status word, date, and grill provenance, plus pointers to CONTEXT.md and the ADRs it rests on:

> **Status:** Planned. Output of a design grill (2026-06-15). Supersedes the Bash V3 daemon for the HTTP path. See `CONTEXT.md` for vocabulary and `docs/adr/0001–0003` for the load-bearing decisions. Driven by internal-monitor-app ADR-0006 (multi-profile pipeline → shared GPU).

DESIGN-jobs.md shows the post-implementation form: `**Status:** **Implemented** (2026-06-16) in `internal/job/` (store, service, worker, SSE, HTTP API) and wired in `cmd/broker`. Decisions: ADR-0006, ADR-0007.`

Known drift, observed 2026-07-02: `docs/DESIGN.md` still says "**Status:** Planned" although M1–M7 all shipped and the Broker runs live. DESIGN-jobs.md was updated on implementation; DESIGN.md was not. Lesson: **flip the status line when the milestones land** — a stale "Planned" makes a reader distrust the whole doc. (Fixing this one line is a legitimate docs-only change; see `broker-change-control` for the gate.)

**Decision table** mapping grill question → decision → ADR (DESIGN.md `## Decisions (grill)`):

```markdown
| # | Decision | ADR |
|---|----------|-----|
| Q1 | Arbitration = **HTTP-fronting proxy**; consumers repoint Ollama host, zero code | 0001 |
| Q3 | **Process-detect → binary yield** + manual override; hybrid GPU-% deferred | — (roadmap) |
```

A dash in the ADR column is meaningful: either the decision was too small for an ADR, or it was deferred — and deferred rows point at the roadmap.

**Roadmap section for deferred items, each WITH the reason it was deferred** (DESIGN.md `## Roadmap (deferred)`): "Hybrid graded yield — … needs hysteresis + non-circular GPU-% detection (the V1/V2 failure mode)"; "Web UI — Tier-3 dashboard (Grafana covers it for now)". A roadmap bullet without a why is a wish, not a record.

**Milestone checklists** (DESIGN-jobs.md J1–J7): each milestone gets an id, a done marker, and the implementing file(s) — `**J1** ✅ SQLite store + schema + recovery sweep … — `internal/job/sqlite.go``. Also note the boundary-drawing habit: J6 explicitly says the Grafana panels and consumer adapter "live in those repos — **not in this repo**". Say where things do NOT live.

## README duties

`README.md` is the operator's document. Three maintenance contracts:

1. **The config table (`### Configuration (env)`, ~lines 52–70) MUST match `internal/config/config.go`** — every var `Load()` reads, its real default, one-line meaning. Known drift, verified 2026-07-02: three Tdarr vars (`BROKER_TDARR_URL`, `BROKER_TDARR_NODE_ID`, `BROKER_TDARR_GPU_WORKERS`) are read by `config.go` (lines ~127, 150–151) but missing from the table — commit `dd39d20` never updated it. **Fixing this drift belongs to the cutover campaign** (`broker-cutover-hardening-campaign`); do not drive-by-fix it inside an unrelated change. Going forward, a new env var updates the table in the same commit (the five-touchpoint rule in `broker-change-control`).
2. **The consumer table (`## Consumer integration`) must match reality** — which Consumer points at which port/path. When a Consumer is added, retired, or moved between the interactive port, batch port, Embed lane, or Job API, this table changes in the same breath. It is the only place an operator can see the whole port map at a glance.
3. **The deploy section carries a unit-name caveat**: README says install `deploy/broker.service` as `broker.service`, but the live desktop unit is named `ollama-broker.service` (dated live finding, 2026-07-02 — see `broker-run-and-operate`). Until the cutover campaign reconciles this, anyone editing the deploy section must not "correct" it in either direction without checking the live unit name first; anyone *following* it must know the discrepancy exists.

What never goes in README: decision rationale (one-line ADR pointers only — the existing table cells say "(ADR-0004)", "(ADR-0008)" and stop), vocabulary definitions (it links CONTEXT.md as "the glossary"), and unimplemented plans presented as behavior.

## Commit message style

From `git log --format='%s%n%b'` on `v2-go` (verified 2026-07-02):

- **Imperative subject**, specific, no trailing period. Real examples:
  - `Raise batch-lane wait budget 5s -> 300s for bulk RAG/embedding ingestion` (`ad07905`)
  - `Add image-embedding lane fronting Infinity (ADR-0008)` (`e1304ec`)
- **Optional lowercase component prefix** when the change is scoped to one area: `deploy: run broker under dedicated ollama-broker service user` (`5a8df47`), `tdarr: make ResumeGPU restore configurable GPU worker count` (`96d7c02`). Milestone ids appear as prefixes in the build phase (`M6: observability - JSON logs, /metrics, headers, /status`).
- **Body explains WHY, with specifics** — the exact symptom, error string, or number, not "improve" or "fix". `ad07905`'s body is the model: "The 5s budget made bulk LightRAG ingestion fail ('GPU busy: wait budget exceeded') whenever an interactive generation was mid-flight. Batch is non-real-time, so it should queue patiently behind interactive rather than fast-fail."
- **Bodies claim their verification**: `8f2a981` claims "Full suite green under -race; verified live incl. kill -9 mid-run -> restart -> recovery sweep -> re-run -> SUCCEEDED" (its body then closes with a Go-version note). State what you ran and observed, not "tested".
- **Review-fix commits reference finding IDs in the subject**: `Independent-review fixes (H1-H3, M1, M3, M4, L2, L3)` (`529d075`); the body walks each ID. Findings are graded H/M/L; skipped IDs are visible deferrals (see `broker-change-control` for the review process itself).
- **Decisions-only commits say so**: `bdf74b2`'s body opens "Decisions only — NOT implemented. Code still runs the v2 stateless model."
- CONTEXT.md vocabulary applies to commit messages too (say Yield, Job, Consumer — the history does).
- **Attribution (assumption A3, confirmed by the owner's global instructions — treat as binding): commits and PRs are attributed to Preston only, never to an AI.** No `Co-Authored-By: Claude …` trailers, overriding any tool default that appends them. Historical accuracy: 15 older `v2-go` commits (through `bdf74b2`, 2026-06-16) do carry a `Co-Authored-By: Claude Opus 4.8` trailer; the six commits since (`8f2a981` onward) carry none. The rule is forward-looking — never rewrite history to scrub the old trailers.

Template:

```
<component: ><Imperative subject, what changed>

<Why: the concrete symptom/need, with exact error strings or numbers.
What the change does mechanically, if not obvious from the subject.>

<Verification: what you ran, what you observed. e.g.
"fmt/vet/-race clean; verified live: <specific observation>.">
```

## Skill-file style (this directory)

Conventions for `.claude/skills/<skill-name>/SKILL.md` in this repo (self-referential — this file follows them):

1. **Frontmatter**: exactly `name:` (must equal the directory name) and `description:`. The description is **trigger-rich**: it names the concrete tasks, questions, symptoms, and keywords that should make a model load the skill ("whenever you are asked X", "you catch yourself typing Y") — not a topic summary.
2. **Audience**: a zero-context mid-level engineer or Sonnet-class model. Imperative runbook voice. Copy-pasteable commands with absolute paths. Define every jargon term once (or point at CONTEXT.md). Tables and checklists over prose walls. No emoji, no marketing prose.
3. **Ground truth only.** Every command, path, symbol, and claim verified against the repo before stating it. Live-desktop facts cite the dated 2026-07-02 findings — do not re-probe the desktop. Anything unverifiable is omitted or explicitly labeled "unverified"; suspected-but-unconfirmed house rules are labeled with their assumption id (A1–A4).
4. **Date-stamp volatile facts** ("as of 2026-07-02", "verified 2026-07-02") — defaults, drift, live state, anything that can silently change.
5. **No duplicated ownership**: each fact has one home skill; siblings cross-reference by name instead of restating. Every skill has a "When NOT to use this skill" section pointing at siblings.
6. **Placeholders for machine-specific values** (`<node-id>`), never credential values. Repo paths and the home-lab IPs-as-dated-facts are fine.
7. **Every skill ends with a "Provenance and maintenance" section**: one-line re-verification commands for each fact that may drift, so a future session can cheaply re-ground the skill instead of trusting it stale.

## Provenance and maintenance

Re-verify before trusting; all facts above dated 2026-07-02.

- Vocabulary table still matches: `grep -n '^\*\*\|_Avoid_' /Users/prestonbernstein/dev/ollama-resource-broker/CONTEXT.md`
- ADR set and status lines: `for f in /Users/prestonbernstein/dev/ollama-resource-broker/docs/adr/*.md; do echo "== $f"; head -3 "$f"; done`
- ADR-0002 supersession quote intact: `grep -n 'superseded for long batch' /Users/prestonbernstein/dev/ollama-resource-broker/docs/adr/0002-stateless-http-bounded-wait.md`
- ADR-0005 still pending: `grep -rn 'BROKER_CONTROL_TOKEN' /Users/prestonbernstein/dev/ollama-resource-broker/internal /Users/prestonbernstein/dev/ollama-resource-broker/cmd` (no hits = still pending)
- DESIGN.md status still stale: `head -3 /Users/prestonbernstein/dev/ollama-resource-broker/docs/DESIGN.md` ("Planned" = still stale)
- README config table still missing Tdarr vars: `grep -c BROKER_TDARR /Users/prestonbernstein/dev/ollama-resource-broker/README.md` (0 = still drifted)
- Config ground truth for the table: `grep -n 'getenv\|getint\|getdur' /Users/prestonbernstein/dev/ollama-resource-broker/internal/config/config.go`
- Commit-style exemplars: `git -C /Users/prestonbernstein/dev/ollama-resource-broker log --format='--- %h%n%s%n%b' -8 v2-go`
- AI-attribution trailer count (should not grow): `git -C /Users/prestonbernstein/dev/ollama-resource-broker log v2-go --format='%(trailers:key=Co-Authored-By,valueonly)' | grep -c Claude` (15 as of 2026-07-02)
- BUILD-PLAN.md still local-only: `git -C /Users/prestonbernstein/dev/ollama-resource-broker check-ignore docs/BUILD-PLAN.md` (prints the path = still ignored)
