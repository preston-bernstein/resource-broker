# Broker v2 design (HTTP-fronting, Go)

**Status:** Planned. Output of a design grill (2026-06-15). Supersedes the Bash V3 daemon for the HTTP path. See `CONTEXT.md` for vocabulary and `docs/adr/0001–0003` for the load-bearing decisions. Driven by fashion-monitor ADR-0006 (multi-profile pipeline → shared GPU).

## Why v2

V3 (Bash, deployed) detects gaming/Plex and preempts + queues **CLI batch jobs** run through its wrapper. It does **not** front Ollama's HTTP API, which is how the actual consumers (fashion-monitor, LightRAG, estate-scraper, open-webui) reach the GPU. v2 closes that gap.

## Decisions (grill)

| # | Decision | ADR |
|---|----------|-----|
| Q1 | Arbitration = **HTTP-fronting proxy**; consumers repoint Ollama host, zero code | 0001 |
| Q2 | **Go**, single static binary; folds detection + proxy; retires Bash daemon | 0001 |
| Q3 | **Process-detect → binary yield** + a manual force-yield/force-serve override; hybrid GPU-% and presence deferred | — (roadmap) |
| Q4 | **Two priority classes by listener port** (interactive high, batch low); interactive jumps the queue but does not preempt in-flight; preemption is reserved for gaming/Plex | — |
| Q5 | **Stateless HTTP**: bounded in-memory wait → `503 Retry-After`; consumers own retry | 0002 |
| Q6 | **Hard yield**: cancel in-flight + force VRAM unload | 0003 |
| Q7 | **Observability**: `/metrics` (Prometheus) + JSON stdout logs + per-response headers + `GET /status`, into existing Grafana/Loki | — |

## Request flow

```
consumer → Broker listener (interactive :PORT_I | batch :PORT_B)
         → if yielding (gaming/Plex): 503 Retry-After
         → else enqueue (interactive jumps ahead of batch)
         → concurrency-1 gate → forward to Ollama :11434 (stream relayed)
         → on contention mid-flight: cancel upstream + force unload → 503 / truncate
         → response + headers: X-Broker-Status, X-Broker-Wait-Ms
```

- Yield trigger: process-name detection (Plex Transcoder, Steam `SteamLaunch AppId=`, Lutris, Heroic, Wine/Proton) — ported from `resource-manager-v3.sh` — plus a manual override (file or `POST /control`).
- Wait budgets: interactive ~30s, batch ~5s (tunable).
- Streaming: Ollama NDJSON relayed transparently; truncation marker on preemption.

## Consumer integration

- **fashion-monitor**: point pipeline `ollama_host` at the batch port. `PENDING` already handles 503. Record `X-Broker-Status` in `integration_events` — header (`served`, or `deferred` on a 503); for streamed calls the authoritative final outcome (`served`/`preempted`) is in the response trailer `X-Broker-Status`.
- **estate-scraper vision**: batch port; retry on 503.
- **LightRAG / open-webui chat**: interactive port.
- **embeddings** (LightRAG indexing): batch port.

## Roadmap (deferred)

- **Hybrid graded yield** — run inference concurrently with light games (RimWorld), full-yield heavy games (Cyberpunk); needs hysteresis + non-circular GPU-% detection (the V1/V2 failure mode).
- **Presence/Home Assistant** yield signal.
- **CLI durable queue** — keep the legacy file-based fire-and-forget job path (separate from HTTP).
- **Web UI** — Tier-3 dashboard (Grafana covers it for now).
- **Numeric per-request priority** — only if two classes prove too coarse.
