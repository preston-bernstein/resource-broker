# Ollama Resource Broker

Single-GPU arbitration for a home PC shared by **gaming**, **Plex transcoding**,
and **Ollama inference**. Gaming/Plex take absolute priority; inference is
queued, preempted, and resumed around them.

This is the **v2 Go HTTP-fronting broker**. It sits in front of Ollama and every
inference consumer points at it instead of Ollama directly — no per-consumer
code. The original Bash daemon lives in [`legacy/`](legacy/).

See [`docs/DESIGN.md`](docs/DESIGN.md) for the design and
[`docs/adr/`](docs/adr/) for the decisions; [`CONTEXT.md`](CONTEXT.md) is the
glossary.

## How it works

- **Two listener ports** speak Ollama's API: interactive (high priority) and
  batch (low). Consumers pick a port; interactive jumps ahead of batch.
- **Concurrency 1** — exactly one request reaches Ollama at a time.
- **Yield** — when a game or Plex transcode is detected (by process name), the
  broker refuses new inference (`503 Retry-After`), cancels any in-flight call,
  and forces Ollama to unload models so the GPU is fully free for the game. When
  contention clears, it serves again.
- **Two paths** — *synchronous* requests stream live through the proxy (all
  interactive work + short batch like embeddings) and are stateless: they wait
  at most a per-class budget, then `503`, and the consumer retries. *Durable
  Jobs* (long batch — scoring, vision) are submitted to `POST /jobs`, persisted
  in SQLite, processed through the same GPU gate, and **survive a restart** — a
  Job interrupted by a crash re-runs; one preempted by gaming or an interactive
  burst requeues at the front. See [`docs/DESIGN-jobs.md`](docs/DESIGN-jobs.md).
- **Configurable concurrency + quantum** (ADR-0004) — `BROKER_MAX_INFLIGHT`
  (default 1) caps requests reaching Ollama; a running Job is protected for
  `BROKER_BATCH_QUANTUM` before an interactive request may preempt it.
- **Observable** — Prometheus `/metrics`, JSON logs, `/status`, and per-request
  signals: `X-Broker-Wait-Ms` header; `X-Broker-Status` as a header
  (`served`, or `deferred` on a 503) plus an authoritative **trailer** on
  streamed responses that carries the true final outcome (`served`/`preempted`),
  since mid-stream preemption isn't known when headers are sent.

## Build & run

```sh
make build                 # -> bin/ollama-broker (static, CGO disabled)
OLLAMA_URL=http://127.0.0.1:11434 ./bin/ollama-broker
```

Requires **Go ≥ 1.24** (the durable Job store uses the pure-Go
`modernc.org/sqlite` driver, so the binary stays static / CGO-free).

### Configuration (env)

| Var | Default | Meaning |
| --- | --- | --- |
| `OLLAMA_URL` | `http://127.0.0.1:11434` | Upstream Ollama |
| `INFINITY_URL` | _(unset)_ | Upstream Infinity image-embedding server. Unset disables the embed lane (ADR-0008) |
| `BROKER_INTERACTIVE_ADDR` | `:11435` | Interactive (high-priority) port |
| `BROKER_BATCH_ADDR` | `:11436` | Batch (low-priority) port |
| `BROKER_CONTROL_ADDR` | `:11437` | Control plane (`/control`,`/status`,`/metrics`,`/healthz`) |
| `BROKER_EMBED_ADDR` | `:11438` | Image-embedding lane (fronts Infinity; only listens when `INFINITY_URL` set) |
| `BROKER_INTERACTIVE_WAIT` | `30s` | Interactive slot wait budget |
| `BROKER_BATCH_WAIT` | `5s` | Batch slot wait budget |
| `BROKER_DETECT_INTERVAL` | `3s` | Contention re-check period |
| `BROKER_MAX_WAITERS` | `256` | Max queued requests per class before fast 503 |
| `BROKER_MAX_INFLIGHT` | `1` | Max concurrent requests reaching Ollama (ADR-0004) |
| `BROKER_BATCH_QUANTUM` | `10s` | Min-run window before interactive may preempt a Job |
| `BROKER_JOB_DB` | `broker-jobs.db` | SQLite file for the durable Job queue |
| `BROKER_JOB_MAX_ATTEMPTS` | `3` | Re-runs before a Job is FAILED |
| `BROKER_JOB_PRUNE_INTERVAL` | `10m` | Terminal-Job sweep period |
| `BROKER_JOB_FETCHED_GRACE` | `1h` | Retain a fetched result this long before pruning |
| `BROKER_JOB_HARD_CAP` | `168h` | Max age of any terminal Job before pruning |

### Control plane

```sh
curl localhost:11437/status                              # yield + queue state
curl localhost:11437/metrics                             # Prometheus
curl -XPOST localhost:11437/control -d '{"mode":"yield"}' # force yield | serve | auto
```

## Durable Jobs (long batch)

Long batch work uses the Job API on the control plane instead of streaming
through the proxy. Submit returns immediately; the Job is persisted and runs
when the GPU is free.

```sh
# submit (Idempotency-Key required — a repeated key returns the same job_id)
curl -XPOST localhost:11437/jobs -H 'Idempotency-Key: run-42' \
  -d '{"model":"llama3.1:8b","prompt":"...","source":"fashion-monitor","owner":"profile-7","options":{"temperature":0.2}}'
# -> {"job_id":"..."}

curl localhost:11437/jobs/<id>           # {state, position?, progress?, error?}
curl localhost:11437/jobs/<id>/result    # output (stamps first-fetch retention)
curl localhost:11437/jobs/<id>/events    # SSE: state / progress / done
curl -XPOST localhost:11437/jobs/<id>/cancel
curl "localhost:11437/jobs?source=fashion-monitor&owner=profile-7&state=QUEUED"
```

States: `QUEUED → RUNNING → SUCCEEDED | FAILED | CANCELED`. A Job preempted by
gaming or an interactive request requeues at the **front**; a Job interrupted by
a broker restart re-runs (`attempts++`, capped). Results are retained until
fetched (then pruned after a grace), with a hard age cap.

## Consumer integration

Point each consumer's Ollama host at a broker port — that's the whole change for
synchronous work:

| Consumer | Path |
| --- | --- |
| open-webui / LightRAG chat | interactive `:11435` |
| estate-scraper vision (short) | batch `:11436` |
| estate-scraper image embeddings (SigLIP/Infinity) | embed `:11438` → `/embeddings` (ADR-0008) |
| fashion-monitor scoring, long vision runs | Job API `POST :11437/jobs` |

## Deploy

```sh
sudo install -m755 bin/ollama-broker /usr/local/bin/ollama-broker
sudo install -m644 deploy/broker.service /etc/systemd/system/broker.service
sudo systemctl daemon-reload && sudo systemctl enable --now broker
```

Run it on **other ports alongside** the legacy V3 daemon first; cut consumers
over one at a time; retire V3 only after a soak. See `docs/DESIGN.md`.
