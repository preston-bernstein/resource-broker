---
name: broker-diagnostics-and-tooling
description: How to MEASURE the resource-broker instead of eyeballing it — the complete /metrics inventory with interpretation guides, the /status JSON field guide, per-request signals (X-Broker-Wait-Ms, the X-Broker-Status header vs authoritative trailer), journalctl log-field recipes, and four runnable read-only probe scripts (broker-snapshot.sh, probe-wait.sh, embed-sanity.sh, watch-jobs.sh). Load when asked "is the broker healthy", "why is this slow", "how long are requests waiting", "did that request get preempted", "are embeddings correct", to set up Grafana panels, or before/after any performance-affecting change. NOT for deciding what a bad measurement means causally — that is broker-debugging-playbook.
---

# Broker diagnostics and tooling

Measure, do not eyeball. Every performance or health claim about the Broker must come from one of the instruments on this page. When a measurement looks bad, take it to `broker-debugging-playbook` for triage; this skill only tells you how to read the gauges.

All endpoints are on the control plane, default `:11437` (desktop: `http://10.0.0.243:11437`). Reads are unauthenticated by design (ADR-0005: open reads, gated mutations — the mutation gate is not yet implemented as of 2026-07-02).

## 1. Prometheus metrics inventory (`GET /metrics`)

Ground truth: `internal/metrics/metrics.go` (hand-rolled Prometheus text format, no client library). Exact names as registered:

| Metric | Kind | Labels | Meaning |
|---|---|---|---|
| `broker_requests_total` | counter | `class` (interactive/batch), `outcome` (served/deferred/preempted) | Every synchronous request through a lane. |
| `broker_wait_seconds_sum` | counter | — | Total slot-wait time of SERVED requests only. |
| `broker_wait_seconds_count` | counter | — | Number of served requests (pair with sum for average wait). |
| `broker_yielding` | gauge | — | 1 while yielding to gaming/Plex (or forced), else 0. |
| `broker_busy` | gauge | — | 1 if at least one GPU slot occupied. |
| `broker_inflight` | gauge | — | Requests currently reaching Ollama. |
| `broker_max_inflight` | gauge | — | Configured concurrency cap (`BROKER_MAX_INFLIGHT`). |
| `broker_queue_depth` | gauge | `class` | Waiters parked per class right now. |
| `broker_jobs` | gauge | `state` (queued/running/succeeded/failed/canceled) | Durable Jobs by state (from SQLite counts). |
| `broker_job_outcomes_total` | counter | `outcome` (succeeded/failed/canceled/preempted/retried) | Terminal and transition Job outcomes. |
| `broker_job_run_seconds_sum` / `_count` | counter | — | Run time of completed (succeeded/failed) Job runs. |

Counters reset on broker restart (in-memory registry, no persistence).

### Interpretation guide

- **Average slot wait** = `broker_wait_seconds_sum / broker_wait_seconds_count`. Healthy idle system: near 0. Rising average with `broker_max_inflight` at 1 means real contention between consumers.
- **`deferred` rising during known no-gaming hours** → false-positive detection or a wait budget too small for the workload (see the 5s→300s batch-wait incident in `broker-failure-archaeology`).
- **`broker_inflight` pinned at `broker_max_inflight` with `broker_queue_depth{class="interactive"} > 0`** → a long synchronous call is starving interactive work. Synchronous batch calls are NOT preemptible (only durable Jobs honor the quantum) — this pattern is the argument for moving that workload to the Job API.
- **`broker_yielding` == 1 for long stretches** → someone is gaming or a Plex transcode is running; expected. If it stays 1 with nothing running, check `/status` `yield.mode` for a stuck manual override.
- **`broker_job_outcomes_total{outcome="preempted"}` vs `{outcome="succeeded"}`** → high preempted:succeeded ratio means Jobs run during contested hours; consider the daily 02:00–09:00 safe-batch window.

### Known blind spots (2026-07-02)

- **Embed-lane requests are counted in `broker_requests_total` under `class="batch"`** — the embed lane's Gate shares the same Registry and uses the Batch class (`cmd/broker/main.go`). You cannot distinguish GPU batch from CPU embed traffic in metrics.
- **The embed lane's own scheduler has no gauges at all**: `/status` `queue` and `broker_inflight`/`broker_queue_depth` report the GPU scheduler only. An embed-lane backlog is invisible except as request latency.
- No per-model or per-consumer breakdown; `source`/`owner` exist only on durable Jobs.

## 2. `/status` field guide (`GET /status`)

Live sample (desktop, 2026-07-02):

```json
{"jobs":{"Queued":0,"Running":0,"Succeeded":0,"Failed":0,"Canceled":0},
 "queue":{"batch":0,"busy":true,"inflight":1,"interactive":0,"max_inflight":1},
 "schedule":{"active_windows":[],"safe_for_tdarr":true},
 "tdarr":{"gpu_workers":2,"managed":true},
 "yield":{"mode":"auto","yielding":false,"reason":"","auto_reason":""}}
```

| Field | Meaning |
|---|---|
| `yield.mode` | `auto` (detection-driven), `yield` or `serve` (manual override via POST /control). Anything but `auto` was set by a human — ask why before touching. |
| `yield.yielding` | Effective state now. `reason` explains it (`plex`, `gaming-steam`, `manual`, ...); `auto_reason` is what detection sees regardless of override. |
| `queue.*` | GPU scheduler snapshot: `inflight`/`max_inflight`, `busy`, per-class waiter counts. Point-in-time, not cumulative. **Embed lane not included.** |
| `jobs.*` | Durable Job counts by state. NOTE: keys are Go-capitalized (`Queued`, not `queued`) — `job.Counts` has no JSON tags. |
| `schedule.active_windows` | Which hardcoded calendar windows (estate-scraper Fri 02:00–07:00, safe-batch daily 02:00–09:00) contain now. |
| `tdarr` | Present only when Tdarr integration configured. `gpu_workers` is the live worker count from the Tdarr node API; `-1` means the query failed. |

## 3. Per-request signals

Lane-port responses (`:11435`, `:11436`, `:11438`) carry:

- **`X-Broker-Wait-Ms`** (header, SERVED responses only — a deferred 503 does not carry it; `deferRequest` in gate.go sets only `Retry-After` + `X-Broker-Status`): milliseconds this request waited for a slot before being served. The primary latency-attribution tool: high wait + fast upstream = queueing problem, low wait + slow response = model/upstream problem. The wait is still logged (`wait_ms` field) for deferred requests.
- **`X-Broker-Status`** (header): `served` (optimistic, set before streaming starts) or `deferred` (on a 503, paired with `Retry-After` = the class wait budget, e.g. live 30s interactive / 300s batch).
- **`X-Broker-Status`** (HTTP **trailer**, streamed responses only): the authoritative final outcome — `served` or `preempted`. Mid-stream preemption cannot be known at header time; only the trailer tells the truth. Non-streamed responses never carry trailers, but a preempted non-streamed request surfaces as a 503 instead, so the header is never wrong there. (`internal/queue/gate.go`, `http.TrailerPrefix`.)

See trailers with curl (HTTP/1.1 chunked):

```sh
curl -sN --raw http://10.0.0.243:11435/api/generate \
  -d '{"model":"llama3.1:8b","prompt":"hi"}' | tail -5
# trailer appears after the terminal chunk; alternatively check for the absence of
# Ollama's final {"done":true} NDJSON line — a cut stream without it was preempted.
```

**Trap (verified live 2026-07-02):** the Gate wraps EVERY path on a lane port — even `GET /api/tags` must acquire the GPU slot. While a long generation is in flight (`queue.busy=true, inflight=1`), cheap metadata reads on `:11435`/`:11436` queue until the wait budget or the client's own timeout expires (observed tonight as "HTTP 000" from a probe with an 8s client timeout while `:11437`/`:11438` answered instantly). This is the concurrency-1 design being visible, not an outage. Discriminator: `/status` on `:11437` — `busy:true` plus responsive control plane = working as designed.

## 4. Log field guide (journalctl)

Structured JSON on stdout (`log/slog`). On the desktop: `sudo journalctl -u resource-broker -o cat`.

| Message | Fields | Fired when |
|---|---|---|
| `request` | `class`, `outcome` (served/deferred/preempted), `wait_ms`, `reason` (deferred only) | Every synchronous request completes. |
| `yield start` | `reason`, `action` | Contention detected or forced; in-flight canceled + VRAM unload requested. |
| `yield stop` | `action` | Contention cleared; serving resumes. |
| `vram unload requested` / `vram unload failed` | `err` on failure | Result of the forced Ollama unload. |
| `embed lane enabled` | `addr`, `upstream` | Startup, only when `INFINITY_URL` set — its absence means the lane is off. |
| `tdarr integration enabled`, `tdarr schedule pause/resume`, `tdarr: GPU workers resumed` | | Tdarr coordination events. |
| `broker up`, `listening` | `upstream`, `addr` | Startup. |
| `yield mode` | `mode` | Manual override via POST /control — grep this to find who/when a stuck mode was set. |

Useful recipes:

```sh
# Yield transitions in the last day (how often did gaming/Plex interrupt?)
sudo journalctl -u resource-broker --since yesterday -o cat | grep -E '"msg":"yield (start|stop)"'
# Deferred storm analysis: reasons histogram
sudo journalctl -u resource-broker --since -6h -o cat | grep '"outcome":"deferred"' | grep -o '"reason":"[^"]*"' | sort | uniq -c
# Detection latency: timestamp delta between game launch (known) and "yield start"
```

## 5. Probe scripts (in this skill's `scripts/` dir)

All read-only (GETs; embed-sanity sends two tiny CPU embedding requests). Default target `BROKER_HOST=http://10.0.0.243`; override for local runs. Verified 2026-07-02: executed against the live broker during an active Plex yield — snapshot rendered the yield WARN and real metrics; probe-wait correctly reported `503 deferred` with the live Retry-After budgets (30s/300s); embed-sanity took its DEFER exit (the embed lane 503s during yield BY DESIGN — shared yield controller, ADR-0008 — even though Infinity is CPU-only). PASS paths for lanes/embedding not yet observed live; re-run when the broker is serving.

| Script | What it measures | Healthy output |
|---|---|---|
| `broker-snapshot.sh` | One-shot health: healthz, parsed /status with WARN lines, key metrics, lane reachability | `healthz: ok`, no WARN lines, lanes `OK` |
| `probe-wait.sh [budget]` | Admission latency per lane via cheap `GET /api/tags`, printing `X-Broker-Wait-Ms`/status | `served wait_ms=0` on both lanes when idle |
| `embed-sanity.sh` | Embed lane correctness: red vs blue PNG cosine similarity (text-tower trap detector, ADR-0008) | `cosine(red, blue)` well below 0.99 → `PASS` |
| `watch-jobs.sh [interval]` | Durable-queue drain view: yield state, inflight, waiters, per-Job state/attempts (the `pos=` column reads `-`: the list endpoint does not compute Position as of 2026-07-02 — use `GET /jobs/{id}` for it) | Queued drains to Running to `ok` counts rising |

## When NOT to use this skill

- A measurement is bad and you need the cause → `broker-debugging-playbook`.
- You need to know why the instrument was designed this way → `broker-architecture-contract` (trailer rationale) or `gpu-arbitration-reference` (trailer mechanics).
- You want to change what is measured → `broker-change-control` first.

## Provenance and maintenance

Facts verified 2026-07-02 against branch `v2-go` and the live desktop (read-only). Re-verify:

```sh
# Metric names still exact
grep -o 'broker_[a-z_]*' /Users/prestonbernstein/dev/resource-broker/internal/metrics/metrics.go | sort -u
# /status shape
grep -n '"yield"\|"queue"\|"jobs"\|"tdarr"\|"schedule"' /Users/prestonbernstein/dev/resource-broker/internal/admin/admin.go
# Header/trailer names
grep -n 'X-Broker' /Users/prestonbernstein/dev/resource-broker/internal/queue/gate.go
# Log messages
grep -rn 'slog\.\(Info\|Warn\|Error\)' /Users/prestonbernstein/dev/resource-broker/internal /Users/prestonbernstein/dev/resource-broker/cmd | grep -o '"[a-z][^"]*"' | sort -u | head -30
# Job list JSON shape assumed by watch-jobs.sh
grep -n 'json:' /Users/prestonbernstein/dev/resource-broker/internal/job/job.go
```
