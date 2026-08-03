# Durable Job system (design)

**Status:** **Implemented** (2026-06-16) in `internal/job/` (store, service, worker, SSE, HTTP API) and wired into `cmd/broker`. Decisions: ADR-0006, ADR-0007. Supersedes ADR-0002 for long batch work. Glossary: see `Job`, `Queue`, `Position`, and `Synchronous request` in CONTEXT.md.

This doc describes the Job system: a durable (it survives a Broker restart), observable queue for long batch inference work. It runs alongside the existing Synchronous request path from ADR-0001/0002, and the two share one scheduler.

## Modes (independent of priority class)

| Mode | Path | Used by | Durable? | Feedback |
|------|------|---------|----------|----------|
| Synchronous | Fronting Proxy (ADR-0001/0002) | all interactive requests; short batch calls (embeddings) | no | live token stream |
| Job | Job API (this doc) | long batch work (scoring, vision) | yes | position, progress, result |

Priority and scheduling work the same as everywhere else in the Broker (ADR-0004): a Job is always in the **batch** priority class. An interactive Synchronous request can preempt a running Job once its protected run time (the batch quantum) has passed. Gaming or Plex Contention preempts everything, Jobs included. A preempted Job goes back to the front of the Queue.

## API (served on the batch/control surface)

- `POST /jobs` (the `Idempotency-Key` header is required) → returns `{job_id}`. The request body carries the model, Ollama parameters, a `source` field (which Consumer submitted it), and an optional `owner` field.
- `GET /jobs/{id}` → `{state, position?, progress?, error?, fetched_at?}`. This is the small, durable, canonical status record — it never includes the result itself.
- `GET /jobs/{id}/result` → the actual output. The first successful fetch stamps `fetched_at`.
- `GET /jobs/{id}/events` → an SSE stream: state changes, Position drops, progress ticks, and a final `done` event signaling the result is ready.
- `GET /jobs?source=&owner=&state=` → a filtered list of Jobs, scoped by Consumer or state. This powers the internal-monitor-app health page and Grafana dashboards.
- `POST /jobs/{id}/cancel` → moves the Job to `CANCELED` (and cancels it upstream if it was running).

## Lifecycle

```
QUEUED --(slot)--> RUNNING --(ok)--> SUCCEEDED
  ^                   |---(err)----> FAILED (attempts < max -> back to QUEUED@front)
  |                   |---(preempt: interactive-in-quantum | gaming)--> QUEUED@front
  |---(cancel)--> CANCELED
restart: any RUNNING -> QUEUED@front, attempts++ ; attempts>max -> FAILED
```

## Storage (SQLite, WAL mode, pure Go)

One table: `jobs(id, idempotency_key UNIQUE, source, owner, state, attempts, model, params_json, prompt, result, error, position_hint, created_at, started_at, finished_at, fetched_at)`.

The Broker writes a Job to disk before acknowledging its submission, and writes its result to disk before marking it `SUCCEEDED` — so a crash can never lose an acknowledged Job or a completed result. On startup, a recovery sweep runs, and a periodic prune removes old Jobs (kept until fetched, plus a 1-hour grace period, with a 7-day hard cap regardless).

## Implementation outline (build milestones)

- **J1** done — SQLite storage, schema, and the startup recovery sweep (moves any `RUNNING` Job back to `QUEUED` at the front, caps retry attempts) — `internal/job/sqlite.go`.
- **J2** done — `POST /jobs` with idempotency, plus the worker loop that pulls `QUEUED` Jobs through the existing scheduler/Yield gate and saves the result — `service.go`, `worker.go`.
- **J3** done — `GET /jobs/{id}` and `/result` (kept until fetched), plus the `/jobs` list with `source`/`owner` filters — `api.go`.
- **J4** done — Position calculation, live progress (token count from Ollama's `eval_count`), and the `/events` SSE stream — `events.go`, `api.go`.
- **J5** done — Cancel; preemption that requeues a Job at the front, wired to the ADR-0004 batch quantum; pausing on gaming — `worker.go`'s monitor.
- **J6** done — Metrics (queue depth, Job counts by state, run-time totals, terminal-outcome counters) exposed on `/metrics` and `/status`. The Grafana panels for the new `broker_jobs*` metrics, and the internal-monitor-app adapter that submits Jobs, polls them, and shows Position/status on its per-profile health page, live in those other repos — **not in this repo**.
- **J7** done — The prune sweep (keep-until-fetched plus grace period, hard cap; runs at startup and periodically). A multi-day soak (an extended trial run under real load) is a separate operational step, planned for the M7 cutover in BUILD-PLAN.

The worker only claims a Job once it already holds the GPU slot — so a Job becomes `RUNNING` only once it is truly running, never while it is merely waiting behind a Synchronous request.

## Open items (roadmap)

A Broker-native dashboard (currently the Broker only exposes data, no UI); per-owner request quotas; numeric priorities within the batch class.
