# Resource Broker

The Resource Broker (called "the Broker" throughout this repo) decides who gets the GPU on a home PC. Three things compete for it: gaming, Plex video transcoding, and Ollama inference (AI model requests). Gaming and Plex always win. The Broker queues inference requests, pauses them, and resumes them around gaming and Plex.

This is the **v2 Go HTTP-fronting broker**: a single program, written in Go, that sits between every inference Consumer (a service that sends Ollama requests, such as internal-monitor-app or LightRAG) and Ollama itself. Consumers point at the Broker instead of Ollama directly, so no Consumer needs custom code to cooperate with gaming or Plex. The original Bash version of this idea lives in [`legacy/`](legacy/) for reference only — do not run it alongside the Broker.

The Broker's actual upstream is swappable: it defaults to Ollama's own API (zero change from today), but `UPSTREAM_BACKEND=openai` re-points it at any OpenAI-compatible server (e.g. vLLM) instead, translating requests/responses so every Consumer keeps speaking Ollama's API unchanged. See the `UPSTREAM_BACKEND`/`UPSTREAM_URL`/`UPSTREAM_API_KEY` rows below.

Read next:
- [`docs/DESIGN.md`](docs/DESIGN.md) — the design
- [`docs/adr/`](docs/adr/) — the ADRs, one per major decision
- [`CONTEXT.md`](CONTEXT.md) — the glossary of every term this repo uses

## How it works

- **Two listener ports.** Both speak Ollama's own API. The interactive port is high priority; the batch port is low priority. A Consumer picks a port by which one it connects to. Interactive requests jump ahead of batch requests in line.
- **One request at a time.** The Broker sends at most one request to Ollama at once (concurrency 1).
- **Yield.** When the Broker detects a game or a Plex transcode running (by matching its process name), it stops serving inference: new requests get `503 Retry-After`, any request already in flight is canceled, and the Broker forces Ollama to unload its models so the GPU is fully free for the game. When the game or transcode ends, the Broker serves inference again.
- **Two request paths.**
  - *Synchronous requests* stream live through the Broker's proxy. This covers all interactive work and short batch calls like embeddings. They are stateless: each one waits at most a fixed time budget for its class, then gets `503`, and the Consumer must retry.
  - *Durable Jobs* are for long batch work (scoring, vision). A Consumer submits one with `POST /jobs`; the Broker saves it to a SQLite database, runs it through the same GPU rule as everything else, and it survives a Broker restart. A Job interrupted by a crash re-runs; one interrupted by gaming or a burst of interactive traffic goes back to the front of the line. See [`docs/DESIGN-jobs.md`](docs/DESIGN-jobs.md) for detail.
- **Configurable concurrency and protected run time (ADR-0004).** `BROKER_MAX_INFLIGHT` (default 1) sets how many requests may reach Ollama at once. `BROKER_BATCH_QUANTUM` sets how long a running Job is protected from being preempted by an interactive request.
- **Observable.** The Broker exposes Prometheus metrics at `/metrics`, writes JSON logs, and serves `/status`. Each response also carries: an `X-Broker-Request-Id` header, a unique id minted for every Synchronous request (ADR-0011) so one request's admission, log lines, and response can all be tied together — useful when a Consumer needs to match its own failure to a specific line in the Broker's logs; an `X-Broker-Wait-Ms` header (how long the request waited); an `X-Broker-Status` header (`served`, or `deferred` if it got a 503); and, on streamed responses, an authoritative `X-Broker-Status` trailer with the true final outcome (`served` or `preempted`) — a trailer is needed because a stream can still get preempted after its headers are already sent.
- **A real `/healthz` (ADR-0010).** It checks three things, not just whether the process is running: that Ollama is reachable, that the durable Job store can be read, and that the Contention-detection loop is still actively polling (not stuck or dead). If any of the three fails, `/healthz` returns `503` naming which dependency is broken, instead of always reporting healthy.

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
