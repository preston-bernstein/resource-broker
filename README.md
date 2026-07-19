# ollama-resource-broker

Bash daemon that arbitrates a single shared GPU between gaming/Plex transcoding and Ollama inference on a home PC — detects contention by process name and queues/preempts batch inference jobs around it.

[![CI](https://github.com/preston-bernstein/ollama-resource-broker/actions/workflows/ci.yml/badge.svg)](https://github.com/preston-bernstein/ollama-resource-broker/actions/workflows/ci.yml)  [![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **Branch note:** `main` holds the V3 Bash prototype described below. The
> current, actively developed implementation — a Go HTTP-fronting proxy that
> every inference consumer points at directly (no CLI wrapper required) — lives
> on the unmerged [`v2-go`](https://github.com/preston-bernstein/ollama-resource-broker/tree/v2-go)
> branch. See [`docs/DESIGN.md`](docs/DESIGN.md) and [`docs/adr/`](docs/adr/)
> for the design that branch implements.

## How it works (V3, this branch)

```
resource-manager.sh (daemon, 20s poll)
      │  pgrep: Plex Transcoder, Steam, Lutris, Heroic, Wine/Proton, native .x86_64
      ▼
 contention detected? ──yes──▶ kill running batch jobs, requeue at priority 1
      │ no                         Ollama gets 0% — gaming/Plex get 100%
      ▼
 queue drained (priority 1: interrupted, priority 2: new)
      ▼
 batch-job-wrapper.sh submits jobs, tracks metadata/metrics
```

Detection is by **process name**, not GPU percentage — GPU-% detection was tried in V1/V2 and broke as soon as Ollama itself was allowed to use the GPU (circular: is that busy-% a game or Ollama?). See the [evolution history](#evolution-history) below and [ADR 0001](docs/adr/0001-http-fronting-proxy-in-go.md) for why the project moved on from this daemon to the HTTP-fronting proxy on `v2-go`.

### Evolution history

| Version | Detection | Outcome |
|---|---|---|
| V0 | Specific game process names | Missed process-name variations, high maintenance |
| V1 | GPU utilization % | Circular — broke once Ollama itself used the GPU |
| V2 | GPU % + hysteresis | Reduced flapping, same circular conflict |
| V3 (this branch) | Process name + job queue | Deployed; Ollama gets full GPU when idle, gaming/Plex get 100% when active |
| v2 (Go, `v2-go` branch) | Process name, hybrid detection planned | HTTP-fronting proxy — consumers repoint their Ollama host, no per-caller wrapper |

## Stack

| Layer | Tech |
|---|---|
| Daemon | Bash, `pgrep`-based process detection, 20s poll loop |
| Job queue | Flat files under `/var/lib/batch-jobs/queue/`, priority-ordered filenames |
| Job wrapper | Bash CLI (`batch-job-wrapper.sh`) — callers submit through it, not Ollama directly |
| Deploy target | systemd services (`resource-manager`, `ollama`) on the GPU host |

## Repo layout

```
resource-manager-v3.sh       daemon — detection + queue draining (deployed as V3)
batch-job-wrapper-v3/v4/v5.sh  CLI wrapper versions; v5 is current
batch-metrics-export.sh      exports job metrics
CLEANUP-AND-DEPLOY.sh        archives old files, installs deps, deploys V3, restarts services
examples/                    sample callers (code-reviewer, email-summarizer, home-automation, research-analyzer)
docs/adr/                    ADRs 0001-0003 — the decisions behind the v2 Go redesign
docs/DESIGN.md               v2 (Go, HTTP-fronting) design doc
archive/                     deprecated V2 daemon/wrapper and superseded planning docs
GO-MIGRATION-*.md, START-HERE-MIGRATION.md, MIGRATION-QUICK-REFERENCE.md
                              handoff package used to start the v2-go branch
```

## Quick start (V3 daemon)

**Prerequisites:** Linux host with systemd, `jq`, GPU-accelerated Ollama already installed.

```bash
git clone https://github.com/preston-bernstein/ollama-resource-broker
cd ollama-resource-broker
./CLEANUP-AND-DEPLOY.sh
```

This archives deprecated files, installs `jq`, creates the queue directory, deploys the V3 scripts to `/usr/local/bin/`, and restarts the `resource-manager` and `ollama` systemd services.

## Usage

```bash
# Submit a batch job — runs immediately if idle, queues if gaming/Plex active
batch-job-wrapper.sh --job-id "email-summary-$(date +%s)" --model llama3.1:70b "Summarize these emails..."

# Check queue state
ls -la /var/lib/batch-jobs/queue/

# Watch the daemon
tail -f /var/log/resource-manager.log
```

Callers must go through `batch-job-wrapper.sh` to get queue/preemption behavior — Ollama's HTTP API itself is not fronted on this branch (that's what `v2-go` adds). See [`examples/`](examples/) for sample integrations and [`APPLICATION-INTEGRATION-GUIDE.md`](APPLICATION-INTEGRATION-GUIDE.md) / [`METRICS-REFERENCE.md`](METRICS-REFERENCE.md) for the full wrapper contract and metadata schema.

## Troubleshooting

GPU detection issues on AMD (RX 9070 XT) are documented in [`GPU-TROUBLESHOOTING.md`](GPU-TROUBLESHOOTING.md).

## Architecture decisions

Three ADRs in [`docs/adr/`](docs/adr/) cover the v2 redesign: the HTTP-fronting proxy in Go, stateless HTTP with bounded wait, and hard-yield VRAM unload.

## License

MIT — see [LICENSE](LICENSE).
