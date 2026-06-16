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
- **Stateless** — a request waits at most a per-class budget, then `503`;
  consumers retry (e.g. internal-monitor-app's PENDING replay). Nothing persists, so
  it is reboot-safe.
- **Observable** — Prometheus `/metrics`, JSON logs, `/status`, and per-request
  signals: `X-Broker-Wait-Ms` (header) and `X-Broker-Status`
  (`served`/`preempted` as an HTTP **trailer**, since the outcome isn't known
  until the streamed response ends; `deferred` is a header on the 503 path).

## Build & run

```sh
make build                 # -> bin/ollama-broker (static, CGO disabled)
OLLAMA_URL=http://127.0.0.1:11434 ./bin/ollama-broker
```

### Configuration (env)

| Var | Default | Meaning |
| --- | --- | --- |
| `OLLAMA_URL` | `http://127.0.0.1:11434` | Upstream Ollama |
| `BROKER_INTERACTIVE_ADDR` | `:11435` | Interactive (high-priority) port |
| `BROKER_BATCH_ADDR` | `:11436` | Batch (low-priority) port |
| `BROKER_CONTROL_ADDR` | `:11437` | Control plane (`/control`,`/status`,`/metrics`,`/healthz`) |
| `BROKER_INTERACTIVE_WAIT` | `30s` | Interactive slot wait budget |
| `BROKER_BATCH_WAIT` | `5s` | Batch slot wait budget |
| `BROKER_DETECT_INTERVAL` | `3s` | Contention re-check period |

### Control plane

```sh
curl localhost:11437/status                              # yield + queue state
curl localhost:11437/metrics                             # Prometheus
curl -XPOST localhost:11437/control -d '{"mode":"yield"}' # force yield | serve | auto
```

## Consumer integration

Point each consumer's Ollama host at a broker port — that's the whole change:

| Consumer | Port |
| --- | --- |
| open-webui / LightRAG chat | interactive `:11435` |
| internal-monitor-app pipeline, internal-scraper-service vision, embeddings | batch `:11436` |

## Deploy

```sh
sudo install -m755 bin/ollama-broker /usr/local/bin/ollama-broker
sudo install -m644 deploy/broker.service /etc/systemd/system/broker.service
sudo systemctl daemon-reload && sudo systemctl enable --now broker
```

Run it on **other ports alongside** the legacy V3 daemon first; cut consumers
over one at a time; retire V3 only after a soak. See `docs/DESIGN.md`.
