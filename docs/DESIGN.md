# Broker v2 design (HTTP-fronting, Go)

**Status:** Planned. Output of a design grill (2026-06-15) — a structured review session that stress-tests a design before it is built. Supersedes the Bash V3 daemon for the HTTP path. See `CONTEXT.md` for vocabulary and `docs/adr/0001–0003` for the load-bearing decisions. Driven by fashion-monitor ADR-0006 (a multi-profile pipeline that needs a shared GPU).

## Why v2

V3 (the existing Bash version, already deployed) detects gaming and Plex, then preempts and queues **CLI batch jobs** run through its own wrapper script. It does not front Ollama's HTTP API — the interface the real Consumers (fashion-monitor, LightRAG, estate-scraper, open-webui) actually use to reach the GPU. v2 closes that gap.

## Decisions (grill)

| # | Decision | ADR |
|---|----------|-----|
| Q1 | Arbitration happens through an **HTTP-fronting proxy**. Consumers just point at the Broker instead of Ollama — no code changes needed. | 0001 |
| Q2 | Written in **Go**, as a single static binary. It folds detection and proxying into one program and retires the Bash daemon. | 0001 |
| Q3 | Detection works by **watching process names, then yielding all-or-nothing** (binary yield), plus a manual override. A finer-grained approach — reading actual GPU utilization percentage, or detecting a person's presence — is deferred. | — (roadmap) |
| Q4 | **Two priority classes, one per listener port**: interactive (high) and batch (low). Interactive requests jump ahead of batch requests in line, but do not interrupt a batch request already running. Interrupting a running request is reserved for gaming/Plex Contention. | — |
| Q5 | **Stateless HTTP**: a request waits in memory up to a time budget, then gets `503 Retry-After`. The Consumer is responsible for retrying. | 0002 |
| Q6 | **Hard yield**: cancel any in-flight request and force VRAM to be freed. | 0003 |
| Q7 | **Observability**: `/metrics` (Prometheus format) plus JSON logs to stdout, plus per-response headers, plus `GET /status` — all feeding the existing Grafana/Loki dashboards. | — |

## Request flow

```
consumer → Broker listener (interactive :PORT_I | batch :PORT_B)
         → if yielding (gaming/Plex): 503 Retry-After
         → else enqueue (interactive jumps ahead of batch)
         → concurrency-1 gate → forward to Ollama :11434 (stream relayed)
         → on contention mid-flight: cancel upstream + force unload → 503 / truncate
         → response + headers: X-Broker-Status, X-Broker-Wait-Ms
```

- **What triggers a Yield:** the Broker matches process names — Plex Transcoder, Steam (`SteamLaunch AppId=`), Lutris, Heroic, Wine/Proton — carried over from `resource-manager-v3.sh`. A manual override also works, through a file or `POST /control`.
- **Wait budgets:** about 30 seconds for interactive requests, about 5 seconds for batch requests (both tunable).
- **Streaming:** Ollama's NDJSON stream passes through unchanged. If a request is preempted mid-stream, the stream just stops — there is no marker inside the stream itself. A Consumer detects this by checking the `X-Broker-Status: preempted` trailer, the `preempted` metric, or noticing that Ollama's terminal `{"done":true}` message never arrived. (An alternative — inserting a marker directly into the stream — was rejected: cutting the connection mid-line would leave broken JSON.)

## Consumer integration

- **fashion-monitor**: point its pipeline's `ollama_host` setting at the batch port. Its existing `PENDING` state already handles a `503`. It records `X-Broker-Status` in its `integration_events` table — the header value (`served`, or `deferred` on a 503) for a plain request, and the trailer's final value (`served`/`preempted`) for a streamed one.
- **estate-scraper vision**: use the batch port; retry on `503`.
- **LightRAG / open-webui chat**: use the interactive port.
- **embeddings** (LightRAG indexing): use the batch port.

## Roadmap (deferred)

- **Hybrid graded yield** — let inference run alongside light games (for example RimWorld) while still fully yielding for heavy games (for example Cyberpunk). Needs hysteresis (a delay before switching states, to avoid flapping back and forth) and a way to read GPU utilization percentage without the circularity that broke the V1/V2 attempts.
- **Presence detection** — use Home Assistant to detect whether anyone is home, as a Yield signal.
- **A durable queue for CLI jobs** — a fire-and-forget command-line job path would need to share the Broker's Yield state. Not built. The Bash V3 code in `legacy/` is kept for reference only and must never run alongside the Broker — running both would mean two GPU arbiters fighting each other.
- **Web UI** — a dashboard is deferred; Grafana covers this need for now.
- **Numeric per-request priority** — only worth building if two priority classes prove too coarse.
