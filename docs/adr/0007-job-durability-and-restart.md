# Job durability, restart, and delivery semantics

**Status: accepted (design grill 2026-06-16); implemented 2026-06-16 in `internal/job/`.** Companion to ADR-0006. Implementation note: `attempts` increments at the point of restart-recovery and on a genuine run error (both capped by `max_attempts`); a clean preempt (gaming/interactive) or explicit cancel never burns an attempt, so preemption under heavy gaming can't drive a healthy Job to FAILED.

Resilience is the point of the Job path, so:

- **Store:** SQLite (pure-Go `modernc.org/sqlite`, WAL), single file in a data dir — queryable status, keeps the static CGO-free binary, matches the rest of the stack. Write ordering is durability-first: a submitted Job is persisted *before* its `job_id` is acked; a result is persisted *before* the Job flips to `SUCCEEDED`. No ack is ever lost; no `SUCCEEDED` Job lacks its result.
- **Restart of a `RUNNING` Job:** an LLM generation has no checkpoint, so resume is impossible — re-run is the only option. On startup every Job left `RUNNING` resets to `QUEUED` at the front with `attempts++`. A `max_attempts` cap (default 3) → `FAILED("exceeded attempts")` so a Job that *causes* a crash can't loop forever. Re-running is safe because inference is idempotent compute; it may orphan one in-flight Ollama generation (acceptable cost).
- **Submit idempotency:** consumers must send an `Idempotency-Key`; a retried submit with the same key returns the existing Job instead of duplicating work — essential because a consumer may retry submit after a crash before it ever saw the `job_id`.
- **Result retention:** retain-until-fetched with a hard cap. The first successful `GET /jobs/{id}/result` stamps `fetched_at`; the Job is pruned after a short grace (default 1h, so re-fetches still work). Never-fetched Jobs are pruned at a hard cap (default 7d). A periodic + startup sweep enforces both.

We rejected hard time-based TTL alone (a slow/crashed consumer could lose a result it never saw) and resume-from-checkpoint (no such thing for LLM generation).
