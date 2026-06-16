# Durable Job system (design)

**Status:** Designed (grill 2026-06-16), **not implemented**. Decisions: ADR-0006, ADR-0007. Supersedes ADR-0002 for long batch. Glossary: `Job`, `Queue`, `Position`, `Synchronous request` in CONTEXT.md.

Adds a durable, observable, restart-surviving queue for long batch inference, alongside the existing synchronous proxy. Two modes, one scheduler.

## Modes (orthogonal to priority class)

| Mode | Path | Used by | Durable? | Feedback |
|------|------|---------|----------|----------|
| Synchronous | Fronting Proxy (ADR-0001/0002) | all interactive; short batch (embeddings) | no | live token stream |
| Job | Job API (this doc) | long batch (scoring, vision) | yes | position, progress, result |

Priority/scheduling unchanged (ADR-0004): Jobs are the **batch** class; interactive synchronous requests preempt a running Job within the batch quantum; gaming/Plex preempts everything. A preempted Job requeues at the front.

## API (on the batch/control surface)

- `POST /jobs` (header `Idempotency-Key` required) → `{job_id}`. Body: model + Ollama params + `source`, optional `owner`.
- `GET /jobs/{id}` → `{state, position?, progress?, error?, fetched_at?}` — canonical, durable, small (no result blob).
- `GET /jobs/{id}/result` → the output; stamps `fetched_at` on first success.
- `GET /jobs/{id}/events` → SSE: state changes, position drops, progress ticks, `done` (signals result ready).
- `GET /jobs?source=&owner=&state=` → scoped list (powers fashion-monitor health page + Grafana).
- `POST /jobs/{id}/cancel` → `CANCELED` (cancels upstream if running).

## Lifecycle

```
QUEUED --(slot)--> RUNNING --(ok)--> SUCCEEDED
  ^                   |---(err)----> FAILED (attempts < max -> back to QUEUED@front)
  |                   |---(preempt: interactive-in-quantum | gaming)--> QUEUED@front
  |---(cancel)--> CANCELED
restart: any RUNNING -> QUEUED@front, attempts++ ; attempts>max -> FAILED
```

## Persistence (SQLite, WAL, pure-Go)

`jobs(id, idempotency_key UNIQUE, source, owner, state, attempts, model, params_json, prompt, result, error, position_hint, created_at, started_at, finished_at, fetched_at)`. Write-before-ack on submit; write-result-before-SUCCEEDED. Startup recovery sweep + periodic prune (retain-until-fetched + grace 1h; hard cap 7d).

## Implementation outline (follow-up milestones)

- **J1** SQLite store + schema + recovery sweep (RUNNING→QUEUED@front, attempts cap).
- **J2** `POST /jobs` (idempotency) + worker loop pulling QUEUED through the existing scheduler/yield gate; result persistence.
- **J3** `GET /jobs/{id}` + `/result` (retain-until-fetched) + `/jobs` list with source/owner filters.
- **J4** Position computation + live progress (Ollama eval_count) + `/events` SSE.
- **J5** Cancel; preemption→requeue-front wired to ADR-0004 quantum; gaming pause.
- **J6** Metrics (queue depth, job states, wait/run histograms) + Grafana; fashion-monitor adapter (submit + poll, surface position/status on the per-profile health page).
- **J7** Prune sweep + soak.

Open/roadmap: Broker-native dashboard (data plane only for now); per-owner quotas; numeric job priorities within batch.
