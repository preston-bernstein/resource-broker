# ollama-yield

Stop your local LLM from holding the GPU when you start a game.

## The problem

You run Ollama on the same machine you game on. Ollama loads a model into VRAM and keeps it there. Then you launch a game and it stutters, or it fails to get the VRAM it needs, or a Plex transcode falls back to the CPU.

Ollama has no way to hand the GPU back. As one open feature request on Ollama puts it, killing the process frees the VRAM but loses all context, and leaving it running keeps the GPU locked up. The usual workaround is stopping and starting the service by hand, which defeats the point of having it always available.

`ollama-yield` does that automatically.

## What it does

It sits in front of Ollama and speaks the same API, so nothing that talks to Ollama has to change. Point your client at it instead.

- **Detects a game or a Plex transcode starting.** When one does, it stops serving inference, cancels anything in flight, and makes Ollama unload its models so the GPU is free.
- **Resumes on its own** when the game or transcode ends. Queued work is not thrown away.
- **Keeps two priority lanes.** Interactive requests jump ahead of batch work. Long jobs go to a durable queue that survives a restart.

## It works on any GPU, including AMD

Contention detection reads process command lines. It never calls NVIDIA, CUDA, or ROCm libraries.

That matters if you own an AMD card. Most local-AI tooling assumes NVIDIA, and AMD owners are used to being second class. Here there is nothing vendor-specific to support, so an RX 9070 behaves the same as an RTX 5090.

It detects Steam and Proton games, Lutris, Heroic, and Wine, plus Plex and Tdarr transcodes.

One detail worth knowing: Plex runs its transcoder binary for background maintenance, such as intro detection and thumbnail generation, on its own schedule. A process-name match alone would give false positives and pause your inference for no reason. When you give it a Plex token, it checks Plex's own session list before deciding a transcode is real.

## What it deliberately does not do

Scope, stated up front so a change that fights it can be recognised early.

- **It does not pause and resume a generation in progress.** There is no KV cache serialization. A request interrupted by a game is canceled and has to be retried; a durable job re-runs from the start rather than continuing. Doing it properly needs support inside Ollama, which is [an open request there](https://github.com/ollama/ollama/issues/17298).
- **It arbitrates one machine's GPU.** Detection reads the local `/proc`, so it has no view of other hosts. It is not a cluster scheduler.
- **It does not replace Ollama.** It sits in front of Ollama and speaks the same API. Models, prompts and generation are still Ollama's job.

## Requirements

- **Linux.** Detection reads `/proc`. On other systems it reports no contention and the yield feature does nothing.
- **Go 1.24 or newer** to build. The binary is static, with no C dependencies.
- **Ollama**, or any OpenAI-compatible server such as vLLM.

Contributions are welcome, and [`CONTRIBUTING.md`](CONTRIBUTING.md) says what gets merged quickly. Adding another game launcher or transcoder to the detection rules is a small, self-contained first change.

## Quick start

```sh
make build                 # -> bin/resource-broker (static, CGO disabled)
OLLAMA_URL=http://127.0.0.1:11434 ./bin/resource-broker
```

Then point your client at port `11435` instead of `11434`:

```sh
curl localhost:11435/api/generate -d '{"model":"llama3.1:8b","prompt":"hello"}'
```

Start a game and watch it yield:

```sh
curl localhost:11437/status
```

Note: the built binary and the systemd unit are still named `resource-broker`, from before this project was renamed. Renaming them is a separate change so that existing installs keep working.

## How it works

- **Two listener ports.** Both speak Ollama's own API. The interactive port is high priority; the batch port is low priority. A client picks by which port it connects to, and interactive requests jump ahead of batch requests.
- **One request at a time.** At most one request reaches Ollama at once, by default.
- **Yield.** When a game or transcode is detected, new requests get `503 Retry-After`, any request in flight is canceled, and Ollama is forced to unload its models so the GPU is fully free. When the game ends, normal service resumes.
- **Two request paths.** Synchronous requests stream through the proxy and are stateless: each waits at most a fixed budget, then gets a `503` the client should retry. Durable Jobs are for long batch work, saved to SQLite so they survive a restart. A job interrupted by gaming goes back to the front of the queue. See [`docs/DESIGN-jobs.md`](docs/DESIGN-jobs.md).
- **Swappable upstream.** It defaults to Ollama's own API, and `UPSTREAM_BACKEND=openai` points it at any OpenAI-compatible server such as vLLM instead, translating both ways so clients keep speaking Ollama's API unchanged.
- **Observable.** Prometheus metrics at `/metrics`, JSON logs, and a `/status` endpoint. Every response carries a request id, how long it waited, and whether it was served or deferred. Streamed responses carry a trailer with the true final outcome, because a stream can be preempted after its headers are already sent.
- **A real health check.** `/healthz` verifies three things rather than just that the process is alive: that the upstream is reachable, that the job store is readable, and that the detection loop is still polling. If any fail it returns `503` naming which one.

Read next:
- [`docs/DESIGN.md`](docs/DESIGN.md) — the design
- [`docs/adr/`](docs/adr/) — one architecture decision record per major decision
- [`CONTEXT.md`](CONTEXT.md) — a glossary of every term this repo uses

## Configuration and reference

### Configuration (env)

| Var | Default | Meaning |
| --- | --- | --- |
| `UPSTREAM_BACKEND` | `ollama` | Which upstream API family the Broker speaks: `ollama` (default, current behavior, zero change) or `openai` (an OpenAI-compatible server such as vLLM) |
| `OLLAMA_URL` | `http://127.0.0.1:11434` | Upstream Ollama. Required/validated only when `UPSTREAM_BACKEND=ollama` |
| `UPSTREAM_URL` | _(unset)_ | Upstream OpenAI-compatible server base URL (e.g. a vLLM instance). Required when `UPSTREAM_BACKEND=openai` |
| `UPSTREAM_API_KEY` | _(unset)_ | Bearer token sent to the OpenAI-compatible upstream, if it requires auth. Ignored when `UPSTREAM_BACKEND=ollama`. Never logged |
| `UPSTREAM_UNIT_NAME` | _(unset)_ | Systemd unit name to `systemctl stop` on yield-start and `systemctl start` on yield-clear (openai backend only). Unset/empty (or whitespace-only) disables the Unloader entirely — the pre-existing no-op behavior |
| `BROKER_ROUTE_<N>_MODELS` | _(unset)_ | Comma-separated model names routed to a second upstream instance instead of the default (`OLLAMA_URL`/`UPSTREAM_URL`), `N` = `1..32`. Unset/empty at `N=1` disables all routing — Broker behavior is then byte-for-byte identical to no-routing (ADR-0015). Each model name may appear in at most one route; indices must be contiguous starting at 1 (no gaps); at most 16 routes total |
| `BROKER_ROUTE_<N>_BACKEND` | `openai` | Upstream API family for route `N`: `ollama` or `openai`. Note the default differs from `UPSTREAM_BACKEND` (which defaults to `ollama`) — a route with no `_BACKEND` set is assumed to be an alternate OpenAI-compatible instance such as a second vLLM process |
| `BROKER_ROUTE_<N>_URL` | _(required per route)_ | Base URL for route `N`'s upstream instance. Must not duplicate the default backend's URL or any other route's URL |
| `BROKER_ROUTE_<N>_API_KEY` | _(unset)_ | Bearer token sent to route `N`'s upstream, if it requires auth (`openai` family only). Never logged. Must not contain CR/LF |
| `BROKER_ROUTE_<N>_UNIT_NAME` | _(unset)_ | Systemd unit name to stop/start on yield for route `N`'s instance, independent of the default backend's `UPSTREAM_UNIT_NAME` and every other route's unit. Unset/empty disables the Unloader for this instance only; must not duplicate any other configured unit name |
| `UPSTREAM_IDLE_TIMEOUT` | _(unset)_ | Idle duration (e.g. `1h`) for the default backend before its VRAM is freed via the same systemctl-based Unloader mechanism `UPSTREAM_UNIT_NAME` already uses (symmetric to Ollama's own `OLLAMA_KEEP_ALIVE`). Disabled when unset. Requires `UPSTREAM_UNIT_NAME` to be set; config.Load() fails otherwise |
| `BROKER_ROUTE_<N>_IDLE_TIMEOUT` | _(unset)_ | Idle duration (e.g. `20m`) for route `N`'s backend instance before its VRAM is freed via the same systemctl-based Unloader mechanism `BROKER_ROUTE_<N>_UNIT_NAME` already uses. Disabled when unset. Requires `BROKER_ROUTE_<N>_UNIT_NAME` to be set for that same route index; config.Load() fails otherwise |
| `BROKER_ROUTE_<N>_LANE` | _(unset, both lanes)_ | Optionally scopes route `N` to one lane: `interactive` or `batch`. Empty applies the rule on both lanes |
| `INFINITY_URL` | _(unset)_ | Upstream Infinity image-embedding server. Unset disables the embed lane (ADR-0008) |
| `BROKER_INTERACTIVE_ADDR` | `:11435` | Interactive (high-priority) port |
| `BROKER_BATCH_ADDR` | `:11436` | Batch (low-priority) port |
| `BROKER_CONTROL_ADDR` | `:11437` | Control plane (`/control`,`/status`,`/metrics`,`/healthz`) |
| `BROKER_EMBED_ADDR` | `:11438` | Image-embedding lane (fronts Infinity; only listens when `INFINITY_URL` set) |
| `BROKER_EMBED_TIMEOUT` | `30s` | Bounds how long the embed lane's own upstream call may run once admitted, so a stuck Infinity call can't wedge the lane's single slot forever (ADR-0013). `0` disables the bound |
| `BROKER_INTERACTIVE_WAIT` | `30s` | Interactive slot wait budget |
| `BROKER_BATCH_WAIT` | `5s` | Batch slot wait budget |
| `BROKER_DETECT_INTERVAL` | `3s` | Contention re-check period |
| `BROKER_YIELD_CONFIRM_POLLS` | `2` | Consecutive same-reason detections required before entering yield (filters single-poll false positives; clearing is never debounced) |
| `PLEX_URL` | `http://localhost:32400` | Local Plex Media Server base URL, used to corroborate a "Plex Transcoder" process match against a real playback session |
| `PLEX_TOKEN` | _(unset)_ | Plex API token. Unset disables Plex session corroboration entirely (a process-name match alone is treated as contention, the pre-existing behavior) |
| `BROKER_MAX_WAITERS` | `256` | Max queued requests per class before fast 503 |
| `BROKER_MAX_INFLIGHT` | `1` | Max concurrent requests reaching Ollama (ADR-0004) |
| `BROKER_BATCH_QUANTUM` | `10s` | Min-run window before interactive may preempt a Job |
| `BROKER_PARK_HOLD` | `600s` | Max time a Batch request may stay parked during yield (ADR-0009) |
| `BROKER_PARK_MAX_QUEUE` | `32` | Max parked Batch requests; 0 disables parking (ADR-0009, kill-switch) |
| `BROKER_PARK_DRAIN_BURST` | `8` | Parked requests released per 1s drain tick (ADR-0009) |
| `BROKER_JOB_DB` | `broker-jobs.db` | SQLite file for the durable Job queue |
| `BROKER_JOB_MAX_ATTEMPTS` | `3` | Re-runs before a Job is FAILED |
| `BROKER_JOB_PRUNE_INTERVAL` | `10m` | Terminal-Job sweep period |
| `BROKER_JOB_FETCHED_GRACE` | `1h` | Retain a fetched result this long before pruning |
| `BROKER_JOB_HARD_CAP` | `168h` | Max age of any terminal Job before pruning |

Park-expiry alerting: `rate(broker_requests_total{outcome="expired"}[5m]) > 0` — a parked
request aging out means Yields are outlasting `BROKER_PARK_HOLD`; see ADR-0009.

Detection-blind alerting: `rate(broker_detect_errors_total[10m]) > 0` — a nonzero rate means
Contention detection is failing open: the Broker can't read `/proc` (or lost visibility to
running processes after a hardening change), so the Yield feature may be silently doing
nothing. See `internal/detect/detect.go`'s `Detect()`.

Embed-lane wedge alerting: `rate(broker_requests_total{outcome="upstream_timeout"}[5m]) > 0` —
a nonzero rate means the embed lane's own upstream call to Infinity hit `BROKER_EMBED_TIMEOUT`
instead of returning normally, the exact failure ADR-0013 exists to stop from being silent.

### Control plane

The control plane is the Broker's management interface: check its status, read its metrics, confirm it's actually healthy, or force it into a mode by hand.

```sh
curl localhost:11437/status                              # yield + queue state
curl localhost:11437/metrics                             # Prometheus
curl localhost:11437/healthz                             # readiness: Ollama + job store + detector loop (ADR-0010)
curl -XPOST localhost:11437/control -d '{"mode":"yield"}' # force yield | serve | auto
```

## Durable Jobs (long batch)

Long batch work uses the Job API on the control plane instead of streaming
through the proxy. Submit returns immediately; the Job is persisted and runs
when the GPU is free.

```sh
# submit (Idempotency-Key required — a repeated key returns the same job_id)
curl -XPOST localhost:11437/jobs -H 'Idempotency-Key: run-42' \
  -d '{"model":"llama3.1:8b","prompt":"...","source":"internal-monitor-app","owner":"profile-7","options":{"temperature":0.2}}'
# -> {"job_id":"..."}

curl localhost:11437/jobs/<id>           # {state, position?, progress?, error?}
curl localhost:11437/jobs/<id>/result    # output (stamps first-fetch retention)
curl localhost:11437/jobs/<id>/events    # SSE: state / progress / done
curl -XPOST localhost:11437/jobs/<id>/cancel
curl "localhost:11437/jobs?source=internal-monitor-app&owner=profile-7&state=QUEUED"
```

States: `QUEUED → RUNNING → SUCCEEDED | FAILED | CANCELED`. A Job preempted by
gaming or an interactive request requeues at the **front**; a Job interrupted by
a broker restart re-runs (`attempts++`, capped). Results are retained until
fetched (then pruned after a grace), with a hard age cap.

## Consumer integration

Point each Consumer's Ollama host at a Broker port — that's the whole change for
synchronous work:

| Consumer | Path |
| --- | --- |
| open-webui / LightRAG chat | interactive `:11435` |
| internal-scraper-service vision (short) | batch `:11436` |
| internal-scraper-service image embeddings (SigLIP/Infinity) | embed `:11438` → `/embeddings` (ADR-0008) |
| internal-monitor-app scoring, long vision runs | Job API `POST :11437/jobs` |

## Deploy

```sh
sudo install -m755 bin/resource-broker /usr/local/bin/resource-broker
sudo install -m644 deploy/broker.service /etc/systemd/system/resource-broker.service
sudo systemctl daemon-reload && sudo systemctl enable --now resource-broker
```

Install and run the Broker on ports that don't conflict with the legacy V3
daemon, so both run side by side at first. Move each consumer over to the
Broker one at a time. Retire V3 only after a soak — an extended trial run
under real load that proves the Broker is stable. See `docs/DESIGN.md`.

### Deploy-checkout drift watch (optional, host-level Prometheus setup)

If the deploy host has its own git checkout of this repo (for building from
source rather than a binary copy), a broken remote or stale credentials on
that checkout can go unnoticed for a long time — it did, once, for 30+
commits (see `deploy/check-deploy-drift.sh`'s own comment for the root
cause). `deploy/check-deploy-drift.sh` writes Prometheus textfile metrics
(`resource_broker_deploy_git_fetch_success`, `resource_broker_deploy_checkout_behind_commits`,
`resource_broker_deploy_checkout_dirty`) so this shows up on a dashboard
instead of silently sitting there. Wire it up with:

```sh
sudo install -m644 deploy/resource-broker-deploy-drift-watch.service /etc/systemd/system/
sudo install -m644 deploy/resource-broker-deploy-drift-watch.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now resource-broker-deploy-drift-watch.timer
```

Edit the two paths in `resource-broker-deploy-drift-watch.service`'s `ExecStart`
if this host's checkout or textfile-collector directory differs from the
defaults.
