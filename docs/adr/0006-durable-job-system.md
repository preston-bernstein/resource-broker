# Durable async Job system for long batch work (hybrid with the sync proxy)

**Status: accepted (design grill 2026-06-16); implemented 2026-06-16 in `internal/job/`. Supersedes ADR-0002 for long batch.**

With many services sharing one GPU, the stateless "wait-or-503-and-retry" model (ADR-0002) is no longer enough: long, resilience-critical batch workloads need to **queue durably, report their position in line and live status, and survive a restart**. So the Broker grows a second front door.

**Two modes, orthogonal to priority class:**
- **Synchronous** (Fronting Proxy, ADR-0001/0002, unchanged): all interactive work and *short/cheap* batch (e.g. embeddings) — streamed live, ephemeral, 503-and-retry on contention.
- **Job** (new): long/expensive batch (fashion-monitor scoring runs, estate-scraper vision). `POST /jobs` returns a `job_id` immediately; the work is enqueued durably and processed through the same single-GPU scheduler (ADR-0004 — Jobs are the *batch* class; interactive synchronous requests preempt running Jobs within the quantum; gaming preempts all).

**Job model:** states `QUEUED → RUNNING → SUCCEEDED | FAILED | CANCELED`; a preempted `RUNNING` Job returns to `QUEUED` at the **front** (resume-first, no starvation). **Position** = 1-based index in the batch queue (deterministic), reported while queued, paired with a clearly-soft ETA — never a hard wait promise (interactive/gaming move the line). Live progress (tokens, elapsed) is surfaced while running. Each Job carries `source` + optional `owner` (e.g. profile id) for scoped views.

**Feedback:** `GET /jobs/{id}` is the canonical, durable, restart-safe status read; `GET /jobs/{id}/events` is a thin SSE live layer over the same state; `GET /jobs?source=&owner=&state=` lists. The Broker is the **data plane only** — the human UX lives in the consumer that owns the relationship (fashion-monitor's per-profile health page) and in Grafana; a Broker-native dashboard is roadmap, not built.

We rejected making *all* batch async (sub-second embeddings shouldn't pay submit/poll/persist/prune overhead) and a per-request `async` flag (pushes the mode choice onto every caller, muddies the API). Cost accepted: long-batch consumers stop being zero-code and must adopt the Job protocol (an adapter each).
