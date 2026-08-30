---
name: broker-run-and-operate
description: >
  Operate the deployed resource-broker on the desktop (<broker-host>):
  systemd unit anatomy, deploy/upgrade steps, control plane (/healthz, /metrics,
  /status, /control), durable Job API cookbook (POST /jobs, Idempotency-Key,
  states, SSE events, cancel), consumer port map (:11435/:11436/:11437/:11438),
  restart blast radius, safe operating windows, and Tdarr coexistence. Load this
  when asked to deploy or restart the Broker, submit or inspect a Job, force
  yield/serve, wire a new Consumer to a port, read broker journalctl logs, or
  when someone mentions resource-broker.service, jobs.db, "GPU busy: wait budget
  exceeded", or the Friday estate-scraper window.
---

# Running and operating the Broker

The Broker is a single Go binary that fronts Ollama on a home desktop
(<broker-host>) whose one GPU (AMD RX 9070 XT) is shared, in priority order:
gaming/Plex > Ollama inference > Tdarr transcoding. Consumers point at Broker
ports instead of raw Ollama; the Broker queues, yields, and preempts around
the GPU's real owner.

Vocabulary (CONTEXT.md is the glossary; use these terms exactly): **Broker**
(not gateway), **Yield** (not pause), **Preemption** (not kill), **Job** (not
task), **Consumer** (not client), **Synchronous request** (streams live through
the Fronting Proxy), **Embed lane** (the :11438 Infinity path).

**When NOT to use this skill**
- Measuring performance, reading /metrics deeply, probe scripts, header/trailer
  semantics → `broker-diagnostics-and-tooling`.
- Building the binary, dev environment, test/vet/race commands, macOS-vs-Linux
  traps → `broker-build-and-env`.
- Live misbehavior triage (symptom → cause) → `broker-debugging-playbook`.
- Retiring the legacy arbiter, adding control-plane auth, fixing deploy drift →
  `broker-cutover-hardening-campaign` (gated campaign; do not freelance it).

---

## 1. Deployment anatomy (as of 2026-07-02)

| Item | Value |
|---|---|
| Host | desktop, <broker-host> (Linux) |
| Live systemd unit | `resource-broker.service` — **NOT** `broker.service` (see mismatch below) |
| User/Group | `ollama-broker` (dedicated service user — house rule, see note) |
| Binary | `/usr/local/bin/resource-broker` (dated Jun 30) |
| Unit source in repo | `deploy/broker.service` (drifted vs live — see Standing warnings) |
| Job DB | `/var/lib/ollama-broker/jobs.db` via `Environment=BROKER_JOB_DB=...` + `StateDirectory=ollama-broker` |
| Logs | journald, JSON (`slog` JSONHandler on stdout) |

**Unit-name mismatch.** README's deploy section installs the unit as
`broker.service`, but the live unit is named `resource-broker.service`.
Following the README verbatim on the live host would create a *second* unit
beside the running one. Always target `resource-broker` in `systemctl` /
`journalctl` commands on the desktop.

**Service user (house rule, assumption A3 — unwritten, suspected, not yet
confirmed with the owner):** all desktop services run under dedicated nologin
service users, never a personal account. The unit's `User=ollama-broker` /
`Group=ollama-broker` follows this.

**Hardening directives** (from `deploy/broker.service`, all present in repo
unit):

| Directive | Effect | Why safe here |
|---|---|---|
| `NoNewPrivileges=true` | process cannot gain privileges | Broker needs none: it reads /proc, talks HTTP to localhost, writes its Job DB |
| `ProtectSystem=strict` | entire filesystem read-only to the service | only write target is the Job DB |
| `ProtectHome=true` | /home invisible | no home access needed |
| `PrivateTmp=true` | private /tmp | no shared tmp needed |

**Why `StateDirectory=ollama-broker` is required:** under
`ProtectSystem=strict` the whole filesystem is read-only to the service.
`StateDirectory` makes systemd create and own `/var/lib/ollama-broker` and
keep it *writable* despite the strict sandbox — it is the only place the
Broker can persist `jobs.db`. Removing either half (the directive or the
`BROKER_JOB_DB` path pointing into it) breaks the durable Job system at
startup.

**Restart policy** (repo unit): `Restart=always`, `RestartSec=5`,
`StartLimitIntervalSec=60`, `StartLimitBurst=5` — crash-looping stops after 5
restarts in 60s.

### Deploy / upgrade steps

The binary is cross-compiled on the macOS dev machine (`GOOS=linux
GOARCH=amd64 CGO_ENABLED=0`, static, pure-Go SQLite — see
`broker-build-and-env` for the exact build commands), then copied to the
desktop. README's steps, corrected for the live unit name:

```sh
# on the desktop, with the freshly built linux binary at hand
sudo install -m755 resource-broker /usr/local/bin/resource-broker
# CAUTION: do NOT blindly install deploy/broker.service — the repo unit is
# missing the live Tdarr env vars (see Standing warnings). Diff first:
diff <(systemctl cat resource-broker) deploy/broker.service
sudo systemctl daemon-reload
sudo systemctl restart resource-broker        # upgrade path
# first-time install only:
# sudo systemctl enable --now resource-broker
```

Verify after any deploy:

```sh
systemctl status resource-broker
curl -s localhost:11437/healthz     # -> ok
curl -s localhost:11437/status | python3 -m json.tool
```

---

## 2. Port map and Consumer table

Verified against README consumer map + live listener check (2026-07-02: 11435,
11436, 11437, 11438 all listening on all interfaces).

| Port | Lane | Priority | Consumers |
|---|---|---|---|
| `:11435` | Interactive (Ollama API) | high | open-webui, LightRAG chat |
| `:11436` | Batch (Ollama API) | low | estate-scraper short vision, LightRAG embeddings |
| `:11437` | Control plane + Job API | n/a | fashion-monitor scoring, long vision runs (`POST /jobs`); operators (`/status`, `/control`, `/metrics`) |
| `:11438` | Embed lane (OpenAI `/embeddings` contract) | low, own scheduler | estate-scraper SigLIP image embeddings → Infinity on `127.0.0.1:7997` (lane rewrites the path to `/embeddings_image`) |

**Rule: NOTHING points at raw Ollama `:11434`.** This is a house convention,
not an enforced control — as of 2026-07-02 raw Ollama listens on **all
interfaces** at `:11434`, so a misconfigured Consumer would silently bypass
all arbitration. Treat any config referencing `:11434` (other than the
Broker's own `OLLAMA_URL`) as a bug. Wiring a new synchronous Consumer is one
change: set its Ollama host to the right Broker port.

Interactive requests are granted GPU slots before batch; FIFO within class.
Batch waiters hold for up to `BROKER_BATCH_WAIT` (live: 300s) before a 503;
interactive for 30s. Env var details live in `broker-config-and-flags`.

---

## 3. Control plane cookbook (`:11437`)

Endpoints verified in `internal/admin/admin.go` (2026-07-02).

```sh
curl -s localhost:11437/healthz    # "ok" (text/plain)
curl -s localhost:11437/metrics    # Prometheus text format
curl -s localhost:11437/status     # full JSON snapshot (below)
curl -s localhost:11437/control    # current yield state only
```

### GET /status — actual JSON shape

```json
{
  "yield":    {"mode": "auto", "yielding": false, "reason": "", "auto_reason": ""},
  "queue":    {"busy": false, "inflight": 0, "max_inflight": 1, "interactive": 0, "batch": 0},
  "schedule": {"active_windows": [], "safe_for_tdarr": true},
  "jobs":     {"Queued": 0, "Running": 0, "Succeeded": 0, "Failed": 0, "Canceled": 0},
  "tdarr":    {"gpu_workers": 2, "managed": true}
}
```

- `yield.mode`: operator override — `auto` | `yield` | `serve`. `yielding` is
  the *effective* state; `reason`/`auto_reason` name the detected contention
  (e.g. a gaming process class).
- `queue`: scheduler occupancy — `inflight` of `max_inflight` slots busy,
  `interactive`/`batch` are queued-waiter counts.
- `schedule.active_windows`: entries like
  `{"name":"estate-scraper","description":"..."}` when a window is active;
  `safe_for_tdarr` is false during the estate-scraper window.
- `jobs`: Job counts by state. Keys are Go-capitalized (`Queued`, not
  `queued`) — the `job.Counts` struct has no JSON tags.
- `tdarr`: present only when the Tdarr integration is enabled;
  `gpu_workers: -1` means the status query to Tdarr failed.

### POST /control — force yield mode

**Auth (verified live 2026-08-15, supersedes the 2026-07-02 "unauthenticated"
warning below this section's history): ADR-0005 is now implemented and
enabled live.** `BROKER_CONTROL_TOKEN` is set via
`EnvironmentFile=/home/<broker-user>/resource-broker/broker-control-token.env`
on the live unit — read it with `sudo cat` on that path. Once a token is
configured, POST /control requires it from every caller, loopback included
(GET /control, /healthz, /metrics, /status stay open regardless):

```sh
TOKEN=$(sudo cat /home/<broker-user>/resource-broker/broker-control-token.env | cut -d= -f2)
curl -s -XPOST localhost:11437/control -H "Authorization: Bearer $TOKEN" -d '{"mode":"yield"}'   # force yield
curl -s -XPOST localhost:11437/control -H "Authorization: Bearer $TOKEN" -d '{"mode":"serve"}'   # force serve
curl -s -XPOST localhost:11437/control -H "Authorization: Bearer $TOKEN" -d '{"mode":"auto"}'    # back to detection (default)
```

Returns the resulting yield state. Invalid mode → 400 `mode must be one of:
auto, yield, serve`. Missing/wrong token → 401. Other methods → 405.

Use sparingly and always return to `auto`:

- `"yield"` refuses all inference and unloads VRAM — every Consumer sees 503s
  until you revert.
- `"serve"` disables gaming/Plex detection entirely; forcing it during a game
  defeats the Broker's purpose and directly hurts gaming latency.
- Mode is in-memory only; a Broker restart resets to `auto`.

Implementing the auth belongs to `broker-cutover-hardening-campaign`.

---

## 4. Job API cookbook (`:11437`, durable long batch)

Routes verified in `internal/job/api.go`; lifecycle from `docs/DESIGN-jobs.md`
(status: Implemented 2026-06-16, ADR-0006/0007). Use Jobs for long batch work
(scoring, long vision); short batch (embeddings) stays synchronous on `:11436`.

### Submit — Idempotency-Key is mandatory

```sh
curl -s -XPOST localhost:11437/jobs -H 'Idempotency-Key: run-42' -d '{
  "model": "llama3.1:8b",
  "prompt": "...",
  "source": "fashion-monitor",
  "owner": "profile-7",
  "options": {"temperature": 0.2}
}'
# -> 201 {"job_id":"..."}
```

- **Missing `Idempotency-Key`** → 400
  `{"error":"Idempotency-Key header is required"}`. The Job is not created.
- **Repeated key** → 200 (not 201) with the **same** `job_id` — an idempotent
  replay of the existing Job, whatever its state. Retry a submit safely by
  reusing the key.
- Missing `model` → 400 `{"error":"model is required"}`. `prompt`, `source`,
  `owner`, `options` are optional; `options` is passed through as Ollama
  options; `source`/`owner` exist for list filtering.
- Submit persists to SQLite **before** returning the id (write-before-ack) —
  a 201 means the Job survives a crash.

### Inspect

```sh
curl -s localhost:11437/jobs/<id>
# {"id":"...","state":"QUEUED","source":"fashion-monitor","owner":"profile-7",
#  "attempts":0,"position":1,"created_at":"..."}
```

Fields (`job.Status`): `id`, `state`, `source`/`owner` (if set), `attempts`,
`position` (1-based place in the batch line; omitted when not queued),
`progress` (`{"tokens":N,"elapsed_ms":N}`, present while RUNNING), `error`,
`created_at`, `fetched_at`. No result blob — this endpoint stays small.

### Result — fetching starts the retention clock

```sh
curl -s localhost:11437/jobs/<id>/result
# 200 {"id":"...","result":"..."}
# 409 {"error":"job result not ready"} if not SUCCEEDED
# 404 {"error":"job not found"}
```

First successful fetch stamps `fetched_at`; the prune sweep (every
`BROKER_JOB_PRUNE_INTERVAL`, 10m) deletes the Job after
`BROKER_JOB_FETCHED_GRACE` (1h) past that stamp. Unfetched terminal Jobs are
kept until the hard cap `BROKER_JOB_HARD_CAP` (168h / 7d). **Copy the result
somewhere durable within the grace hour of first fetch.**

### Events — SSE stream

```sh
curl -sN localhost:11437/jobs/<id>/events
```

Event types (verified in `internal/job/events.go`): `state` (lifecycle
change), `progress` (token/elapsed tick while RUNNING), `done` (terminal;
result ready to fetch). A `position` type is DECLARED in events.go but no code
publishes it as of 2026-07-02 — do not wait on position events; poll
`GET /jobs/{id}` for position instead. Payload:
`{"type":"...","state":"...","position":N,"progress":{...}}` (empty fields
omitted). Behavior:

- On connect you get an immediate `state` snapshot; if the Job is already
  terminal you get `done` and the stream closes.
- Comment keepalives (`: keepalive`) every 15s.
- A slow client silently **drops ticks** (buffered fan-out, never stalls the
  worker) — on reconnect, re-read canonical state via `GET /jobs/{id}`.

### Cancel and list

```sh
curl -s -XPOST localhost:11437/jobs/<id>/cancel
# {"id":"...","state":"CANCELED"}   (cancels the upstream call if RUNNING)

curl -s "localhost:11437/jobs?source=fashion-monitor&owner=profile-7&state=QUEUED"
# {"jobs":[{...Status...}]}   — list entries omit position/progress
```

All three query params are optional filters; `state` matches the uppercase
state names exactly.

### Lifecycle (from docs/DESIGN-jobs.md)

```
QUEUED --(slot)--> RUNNING --(ok)--> SUCCEEDED
  ^                   |---(err)----> FAILED (attempts < max -> back to QUEUED@front)
  |                   |---(preempt: interactive-in-quantum | gaming)--> QUEUED@front
  |---(cancel)--> CANCELED
restart: any RUNNING -> QUEUED@front, attempts++ ; attempts>max -> FAILED
```

A Job becomes RUNNING only once it actually holds the GPU slot. A running Job
is protected for `BROKER_BATCH_QUANTUM` (10s), then a waiting interactive
request preempts it; gaming/Plex preempts immediately. Preempted Jobs requeue
at the **front**.

### What a Broker restart does to Jobs

The startup recovery sweep (`internal/job/sqlite.go`) resets every RUNNING Job
to QUEUED at the front of the line (its `position_hint` is set below the
current minimum) with `attempts` incremented. If the incremented count reaches
`BROKER_JOB_MAX_ATTEMPTS` (3), the Job is instead FAILED with error
`exceeded attempts after restart` — this caps a Job that keeps crashing the
Broker. The sweep logs `job recovery sweep` with `running_reset=N`.
Consequence: **each restart costs every RUNNING Job one attempt** — three
restarts during one long Job kills it.

---

## 5. Operations

### Logs

```sh
journalctl -u resource-broker -f          # follow
journalctl -u resource-broker --since -1h -o cat | python3 -m json.tool --json-lines
```

Logs are JSON (one object per line). Key messages (verified in source,
2026-07-02):

| `msg` | Fields | Meaning |
|---|---|---|
| `request` | `class` (interactive/batch), `outcome` (served/deferred), `wait_ms`, `reason` on deferral | one synchronous request; `GPU busy: wait budget exceeded` = wait budget hit |
| `yield start` | `reason`, `action="cancel in-flight + unload VRAM"` | contention detected or forced |
| `yield stop` | `action="resume service"` | contention cleared |
| `yield mode` | `mode` | operator changed /control mode |
| `job done` / `job preempted` / `job retry` / `job failed` / `job canceled` | `id`, plus `by` (gaming/interactive) on preemption | Job lifecycle |
| `job recovery sweep` | `running_reset` | restart recovery ran |
| `job prune` | `deleted` | terminal-Job sweep |
| `tdarr: GPU workers paused` / `resumed` | — | yield-path Tdarr coordination |
| `tdarr schedule pause: estate-scraper window active` / `...resume...` | — | schedule-path Tdarr coordination |
| `broker up` | `upstream`, `detect_interval` | startup complete |

For metric-level measurement (`/metrics` counters, headers/trailers) see
`broker-diagnostics-and-tooling`.

### Restart procedure and blast radius

```sh
sudo systemctl restart resource-broker
journalctl -u resource-broker -n 20 -o cat     # expect "broker up" + "listening" x4
curl -s localhost:11437/status | python3 -m json.tool
```

Blast radius of a restart:

- **In-flight synchronous requests die.** Streams are cut with no in-band
  marker; Consumers must retry.
- **RUNNING Jobs requeue at the front and re-run**, `attempts++` (cap 3 →
  FAILED `exceeded attempts after restart`). No work is lost below the cap,
  but the Job starts over from the beginning.
- **Model reload cost:** the restart's yield/unload path plus Ollama idling
  means the first request after restart pays a model load (seconds).
- **/control override resets to `auto`** (in-memory only).
- Tdarr schedule state resets; the once-a-minute schedule loop re-pauses
  within a minute if restarted inside the scraper window.

### Safe windows (assumption A3 — suspected house rule, confirm with owner)

Avoid restarts and deploys during:

1. **Friday 02:00–07:00 — the estate-scraper window.** Hardcoded in
   `internal/schedule/schedule.go` (edit the `windows` slice to change it):

   ```go
   var windows = []Window{
       {
           Name:        "estate-scraper",
           Weekday:     weekdayPtr(time.Friday),
           StartHour:   2,
           StartMinute: 0,
           DurationH:   5, // 02:00–07:00 with margin
           Description: "estatesales.net Ollama vision scan (GPU via broker, heavy batch)",
       },
       {
           Name:        "safe-batch",
           Weekday:     nil, // every day
           StartHour:   2,
           StartMinute: 0,
           DurationH:   7, // 02:00–09:00
           Description: "preferred window for background GPU batch work (low inference demand)",
       },
   }
   ```

   The scraper is a weekly heavy vision run; a restart mid-window cuts its
   streams and burns Job attempts.
2. **Whenever gaming or Plex is active** — check
   `curl -s localhost:11437/status` (`yield.yielding: true` means the GPU has
   a higher-priority owner right now; a restart adds churn for zero benefit).

The daily **safe-batch window (02:00–09:00, any day)** is the preferred slot
for disruptive maintenance and background GPU work — except Friday, where the
scraper owns 02:00–07:00. Check active windows live:
`curl -s localhost:11437/status | python3 -c "import json,sys; print(json.load(sys.stdin)['schedule'])"`.

### Tdarr coexistence

Enabled when both `BROKER_TDARR_URL` and `BROKER_TDARR_NODE_ID` are set (live
as of 2026-07-02: URL `http://localhost:8265`, node id `<node-id>` —
machine-specific, read it from the live unit, never hardcode). Two
coordination paths, both in `cmd/broker/main.go`:

- **Yield path:** when the Broker yields to gaming/Plex, it pauses Tdarr GPU
  workers (`transcodegpu=0`); on yield stop it restores
  `BROKER_TDARR_GPU_WORKERS` (default 1; **live sets 2**).
- **Schedule path:** a minute-tick loop pauses GPU workers for the whole
  estate-scraper window and resumes after.

`/status`'s `tdarr` section shows the current worker count (`-1` = query
failed). If Tdarr transcodes look stalled, check whether a window or yield is
active before touching Tdarr itself.

---

## 6. Standing warnings (dated 2026-07-02)

1. **Do not stop/start `resource-manager.service` ad hoc.** The legacy Bash
   arbiter (`/usr/local/bin/resource-manager.sh`) is STILL RUNNING alongside
   the Broker — despite `docs/DESIGN.md` forbidding two uncoordinated GPU
   arbiters. Its retirement is a decision-gated campaign:
   `broker-cutover-hardening-campaign`. Touching it outside that campaign
   risks either double-arbitration or losing the only arbiter mid-game.
2. **Deploy drift:** the repo's `deploy/broker.service` has NO Tdarr env vars.
   Reinstalling it as-is over the live unit would silently disable Tdarr GPU
   management. The live unit also carries duplicated
   `INFINITY_URL`/`BROKER_EMBED_ADDR` `Environment=` lines (harmless —
   systemd last-wins — but drift). Always `diff <(systemctl cat
   resource-broker) deploy/broker.service` before installing.
3. ~~`POST /control` is unauthenticated~~ **FIXED as of 2026-08-15 — ADR-0005
   is live**, see section 3's auth block.
4. **Raw Ollama `:11434` listens on all interfaces** — the "nothing talks to
   :11434" rule is convention only; audit new Consumer configs.
5. **Deploy drift now also covers two new drop-ins/rules, neither in this
   repo** (per-model routing is config-only, ADR-0015, so this was a
   deliberate host-side choice matching the existing embed.conf/plex.conf/
   tdarr.conf pattern — not an oversight):
   - `/etc/systemd/system/resource-broker.service.d/route-qwen25-vllm.conf` —
     `BROKER_ROUTE_1_*` env vars for the live qwen2.5→vLLM cutover (see
     section 7).
   - `/etc/polkit-1/rules.d/53-resource-broker-vllm-yield.rules` — scoped
     grant letting `ollama-broker` run `systemctl start/stop vllm` (exactly
     those two verbs, exactly that one unit) for the route's VRAM-yield
     unload. Without it, `BROKER_ROUTE_1_UNIT_NAME` would silently fail every
     yield transition (WARN-logged, not crashed) and gaming/Plex would not
     free vLLM's VRAM.
   - `/etc/systemd/system/vllm.service.d/served-model-name.conf` — adds
     `qwen2.5` as a second `--served-model-name` alongside the full HF id, so
     vLLM accepts the bare name litellm actually sends. This is a vLLM unit
     change, not a resource-broker one, but exists only because of the
     routing cutover — future re-provisioning of `vllm.service` must
     preserve it or the route will 404.

---

## 7. Live per-model routing (vLLM cutover, added 2026-08-15)

`llm-gateway`'s Interactive-lane chat model (`ollama/interactive/qwen2.5` in
`config/config.yaml`, which litellm resolves to the bare model string
`"qwen2.5"` — verified via litellm's own `LiteLLM completion() model=
qwen2.5; provider = ollama` log line) is routed to the desktop's vLLM
instance (`vllm.service`, `127.0.0.1:8000`, serving
`Qwen/Qwen2.5-3B-Instruct`) instead of Ollama. Everything else (batch-lane
models, embeddings, other interactive models) still goes to Ollama unchanged
— this is a single-model cutover, not a backend swap.

Live config (`route-qwen25-vllm.conf` drop-in, item 5 above):
```
BROKER_ROUTE_1_MODELS=qwen2.5
BROKER_ROUTE_1_BACKEND=openai
BROKER_ROUTE_1_URL=http://127.0.0.1:8000
BROKER_ROUTE_1_LANE=interactive
BROKER_ROUTE_1_UNIT_NAME=vllm
```

Verify the cutover is live: `curl -s localhost:11437/status` should show a
`"routing"` key with `qwen2.5`/`interactive`. A real end-to-end test (with
the control token from section 3): fire a chat completion at
`ollama/interactive/qwen2.5` through litellm (`:4000`) and confirm
`journalctl -u vllm` shows the request landing there, not Ollama (`curl -s
127.0.0.1:11434/api/ps` should show nothing loaded).

**vLLM restart needs free VRAM.** vLLM's `gpu_memory_utilization` reserves a
fixed fraction of the card at startup; if Ollama has anything resident when
`systemctl start vllm` runs (during yield-clear, or a manual restart), vLLM's
engine init fails with `ValueError: Free memory on device cuda:0 (...) is
less than desired GPU memory utilization` and the unit crash-loops
(`Restart=on-failure`, `StartLimitBurst=5`) until it either succeeds or hits
the burst limit and gives up. Hit live 2026-08-15 during initial setup — fix
was `curl -s 127.0.0.1:11434/api/generate -d '{"model":"<name>","keep_alive":0}'`
to force-unload the resident Ollama model, then `systemctl start vllm`
again. If a gaming/Plex yield-clear ever races a batch request that just
loaded a model into Ollama, the same failure could recur on the *automatic*
reload path — this is a real, currently-unmitigated edge case, not just a
one-time setup hiccup; worth revisiting if `vllm.service`'s `NRestarts`
climbs unexpectedly after a yield-clear in production.

---

## Provenance and maintenance

- Live unit/env facts, listener map, and resource-manager status: read-only
  ssh audit dated 2026-07-02 (authoring brief). Re-verify: `systemctl cat
  resource-broker`, `systemctl status resource-manager`, `ss -ltnp | grep -E
  '1143[4-8]|7997'` on the desktop.
- Control-plane endpoints/JSON: `internal/admin/admin.go` (re-check `Mux`).
- Job API/behavior: `internal/job/api.go`, `events.go`, `job.go`,
  `sqlite.go` (recovery sweep), `docs/DESIGN-jobs.md`.
- Unit directives and comments: `deploy/broker.service`; deploy steps + port
  map: `README.md` ("Deploy", "Consumer integration").
- Windows: `internal/schedule/schedule.go` `windows` slice.
- Tdarr wiring: `cmd/broker/main.go` (`runTdarrSchedule`), `internal/tdarr/tdarr.go`.
- ADR-0005 status: implemented and live as of 2026-08-15 (`internal/admin/admin.go`'s `authorized()`); token lives in `/home/<broker-user>/resource-broker/broker-control-token.env` on the live host, not in this repo.
- Live per-model route / vLLM cutover state: section 7 above, dated 2026-08-15 — re-verify with `curl -s localhost:11437/status` and `systemctl cat resource-broker`/`systemctl cat vllm` on desktop before trusting it's unchanged.
