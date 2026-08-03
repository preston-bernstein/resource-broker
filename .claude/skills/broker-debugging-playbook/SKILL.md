---
name: broker-debugging-playbook
description: >
  Symptom-to-triage playbook for live misbehavior of the Broker (ollama-resource-broker).
  Load when you see: every request returning 503 "yielding GPU"; 503 "GPU busy: wait budget
  exceeded"; an interactive stream cut mid-response; Jobs stuck QUEUED; a Job FAILED with
  "exceeded attempts"; image embeddings that are nearly identical for different images;
  embed lane connection refused or 404; the Broker NOT yielding while a game runs; go test
  failing at HEAD; Tdarr GPU workers not restored after yield; suspected double GPU
  arbitration; lane ports (11435/11436) hanging or timing out (curl HTTP 000) while the
  control plane responds; or all Broker ports down. Gives exact first-check commands,
  ranked causes, discriminating experiments, and fixes.
---

# Broker debugging playbook

Live triage for the Broker: the single-binary Go proxy at `/Users/prestonbernstein/dev/ollama-resource-broker` (branch `v2-go`) that arbitrates one GPU between gaming/Plex (absolute priority), Ollama inference, and Tdarr transcoding.

**When NOT to use this skill:**
- You want the *history* of an investigation, not a live fix → `broker-failure-archaeology`.
- You want to build probe scripts or understand `/metrics` internals → `broker-diagnostics-and-tooling`.
- You want to deploy, restart, or operate the service routinely → `broker-run-and-operate`.
- You want to retire the legacy daemon or harden the deployment → `broker-cutover-hardening-campaign` (do NOT freelance that here).
- You want env var meanings and live overrides → `broker-config-and-flags`.

## Orientation (facts as of 2026-07-02)

| Thing | Value |
|---|---|
| Desktop LAN IP | `desktop.example.internal` (run curl from anywhere on LAN; `journalctl`/`systemctl` only on the desktop) |
| Interactive lane | `:11435` (high priority) |
| Batch lane | `:11436` (low priority) |
| Control plane | `:11437` — `/status`, `/control`, `/metrics`, `/healthz`, `/jobs` |
| Embed lane | `:11438` → Infinity SigLIP at `127.0.0.1:7997` (loopback-only) |
| systemd unit | `ollama-broker.service` — **NOT** `broker.service` (README drift) |
| Legacy daemon | `resource-manager.service` still running alongside (see symptom 11) |

Vocabulary (from `CONTEXT.md`, enforced): **Yield** = Broker surrenders the GPU to gaming/Plex. **Preemption** = an in-flight request or Job is aborted to free the slot. **Job** = durable batch work item on `:11437`. **Consumer** = anything pointing at a Broker port.

First command for almost everything:

```bash
curl -s http://desktop.example.internal:11437/status | python3 -m json.tool
```

Fields: `yield.mode` (`auto|yield|serve`), `yield.yielding`, `yield.reason`, `yield.auto_reason`, `queue.{busy,inflight,interactive,batch}`, `jobs` counts, `tdarr.{managed,gpu_workers}`.

Logs (on the desktop, structured JSON on stdout):

```bash
journalctl -u ollama-broker -n 200 --no-pager
journalctl -u ollama-broker -f          # follow live
```

## Triage table

| # | SYMPTOM | FIRST CHECK | LIKELY CAUSES (ranked) | DISCRIMINATING EXPERIMENT | FIX / ESCALATION |
|---|---|---|---|---|---|
| 1 | Every request 503 `broker: yielding GPU: <reason>` | `curl -s http://desktop.example.internal:11437/status` → `yield` block | 1. Real contention (game/Plex running). 2. False-positive detection (`wine.*\.exe` matches a non-game wine app). 3. Stuck manual `ModeForceYield` override. | `yield.mode` = `yield` → override. `mode` = `auto` → read `auto_reason`, then find the matching process (see detail). | Override: `POST /control {"mode":"auto"}`. False positive: kill/exclude the process; rule change goes through `broker-change-control`. |
| 2 | Batch 503 `broker: GPU busy: wait budget exceeded` | `journalctl -u ollama-broker \| grep BATCH_WAIT` is nothing — check unit env: `systemctl cat ollama-broker \| grep BATCH_WAIT` | 1. Wait budget too small for queue depth behind interactive traffic. 2. Genuine sustained interactive storm. 3. `max_inflight` slot leak (rare; check `queue.busy` when idle). | `/status` → `queue.interactive` high while batch defers = budget vs storm. Idle system but `busy: true` = slot leak. | Raise `BROKER_BATCH_WAIT` (live is already 300s per commit `ad07905`); or move the work to a durable Job (`POST /jobs`). Slot leak → restart + file issue. |
| 3 | Interactive stream cut mid-response | Trailer: `curl --raw` against `:11435` (see detail) | 1. Preemption by Yield (game/Plex started mid-generation). 2. Client-side disconnect/timeout. 3. Ollama crash. | Trailer `X-Broker-Status: preempted` = Yield. Trailer `served` but truncated = client/Ollama side. Missing `{"done":true}` final line also indicates a cut stream. | Expected behavior under contention — retry after `Retry-After`. If no contention existed, check symptom 1 causes and Ollama logs. |
| 4 | Jobs stuck QUEUED | `curl -s http://desktop.example.internal:11437/jobs/<id>` → `state`, `position` | 1. Yield active (worker holds the line). 2. Interactive storm holding the single slot. 3. Worker/broker not running. | `/status`: `yield.yielding: true` → cause 1; `queue.interactive > 0` persistently → cause 2; control plane unreachable → cause 3. | 1–2: wait, it self-drains (preempted Jobs requeue at FRONT). 3: `systemctl status ollama-broker` + journalctl. |
| 5 | Job FAILED, attempts exhausted | `curl -s http://desktop.example.internal:11437/jobs/<id>` → `attempts`, `error` | 1. Genuine repeated run error (bad model name, prompt too big, Ollama OOM). 2. Broker restart-loop burning attempts. | `error` field: `"exceeded attempts after restart"` = restart-recovery path; any other text = the last real generation error. Cross-check `journalctl -u ollama-broker \| grep -c "broker up"`. | Real error: fix the request, resubmit with a NEW Idempotency-Key. Restart-loop: find why the unit is crashing first. |
| 6 | Image embeddings nearly identical across different images | Where is the consumer pointed? Must be `http://desktop.example.internal:11438/embeddings` | 1. Consumer bypasses the lane and hits Infinity `:7997/embeddings` directly (text-tower trap, ADR-0008). 2. Consumer hits Ollama ports (`11435/11436`) instead of the embed lane. | Embed the SAME image via `:11438/embeddings` and via `127.0.0.1:7997/embeddings` (on desktop); cosine-compare. The lane rewrite to `/embeddings_image` gives a different (correct) vector. | Repoint the consumer at the Broker embed lane `:11438`. Never call Infinity's unified `/embeddings` with images. |
| 7 | Embed lane connection refused / 404 | `curl -s http://desktop.example.internal:11438/health` | 1. `INFINITY_URL` unset → lane never started (connection refused). 2. Infinity down on `127.0.0.1:7997` (502/503 from lane). 3. Yield active (503 `yielding GPU`). | `journalctl -u ollama-broker \| grep "embed lane enabled"` — absent = cause 1. Present but 502 = cause 2. | 1: set `INFINITY_URL` in the unit, restart. 2: restart Infinity on the desktop. 3: symptom 1. |
| 8 | Broker does NOT yield while a game runs | `/status` → `yield.auto_reason` empty while game visibly running | 1. New launcher's cmdline matches no rule. 2. Running on macOS/dev box (`/proc` absent — detection silently disabled, fail-open). 3. `ModeForceServe` override stuck. | On desktop: `cat /proc/<game-pid>/cmdline \| tr '\0' ' '` and test against the rule list (see detail). `yield.mode` = `serve` → cause 3. | Force manually now: `POST /control {"mode":"yield"}`. Adding a detection rule = code change → `broker-change-control`. |
| 9 | `go test ./...` fails at HEAD | Look at the failing package name | KNOWN as of 2026-07-02: `internal/admin/admin_test.go:31` calls `Mux` with 5 args; signature grew a `TdarrStatusFn` (commit `dd39d20`). | `go test ./... 2>&1 \| grep -v internal/admin` — everything else passes. | Not your bug. Do not "fix" it in passing; see `broker-validation-and-qa`. |
| 10 | Tdarr GPU workers not restored after yield | `/status` → `tdarr` section | 1. Tdarr integration disabled (needs BOTH `BROKER_TDARR_URL` and `BROKER_TDARR_NODE_ID`). 2. Tdarr API call failed (`gpu_workers: -1`). 3. Estate-scraper window active (Fri 02:00–07:00 pauses GPU workers by schedule). | No `tdarr` key in `/status` = cause 1. `gpu_workers: -1` = cause 2. It's Friday early morning = cause 3. | 1: set both vars (repo `deploy/broker.service` LACKS them — deploy drift; live sets `BROKER_TDARR_GPU_WORKERS=2`). 2: check Tdarr at `:8265`. 3: wait for 07:00. |
| 11 | Weird contention / GPU state flapping / double arbitration | On desktop: `systemctl status resource-manager` | The legacy Bash daemon `resource-manager.service` STILL runs alongside the Broker (verified 2026-07-02) — an uncoordinated second arbiter, explicitly forbidden by `docs/DESIGN.md`. | Correlate flap timestamps in `journalctl -u ollama-broker` vs `journalctl -u resource-manager`. | Treat it as a prime suspect but do NOT stop/disable it ad hoc — that is the `broker-cutover-hardening-campaign` skill's job. |
| 12 | All lanes down (connection refused on 11435–11438) | `systemctl status ollama-broker` on the desktop | 1. Unit stopped/crashed. 2. Config error at startup (`config` error log then exit 1). 3. Port conflict (`listen ... : address already in use`). | `journalctl -u ollama-broker -n 50` — look for `"broker up"`, `"config"`, or `"listen"` errors. | Fix config/port, `sudo systemctl restart ollama-broker`. Remember: the unit is `ollama-broker.service`, not `broker.service`. |
| 13 | Lane ports (11435/11436) hang or time out (curl `HTTP 000`) while `:11437`/`:11438` answer instantly | `curl -s http://desktop.example.internal:11437/status` → `queue.busy`, `queue.inflight` | 1. A long in-flight generation holds the single GPU slot — the Gate wraps EVERY lane path, so even `GET /api/tags` queues behind it (`BROKER_MAX_INFLIGHT=1`). 2. Only if `busy:false` too: real network/listener issue → symptom 12. | `busy:true, inflight:1` + responsive control plane = working as designed under load, NOT an outage (observed live 2026-07-02: probes with an 8s client timeout got `000` on both lanes mid-generation). | Wait it out or raise the client's timeout past the wait budget. Do NOT restart — that kills the in-flight work to fix a non-problem. Recurring pattern → move the long workload to the Job API. |

## Symptom detail

### 1. Every request 503 "yielding GPU"

`internal/queue/gate.go` refuses admission with body `broker: yielding GPU: <reason>` (HTTP 503, `Retry-After` set, `X-Broker-Status: deferred`) whenever the yield controller reports yielding — checked both before AND after slot acquisition.

```bash
curl -s http://desktop.example.internal:11437/status | python3 -m json.tool
```

Decision tree on the `yield` block:

- `mode: "yield"` — a manual `ModeForceYield` override is stuck (someone posted it and forgot). Reset:

```bash
curl -s -X POST http://desktop.example.internal:11437/control -d '{"mode":"auto"}'
```

- `mode: "auto"`, `auto_reason` set — detection fired. Reasons map to rules in `internal/detect/detect.go` (first match wins): `plex`, `gaming-steam`, `gaming-lutris`, `gaming-heroic`, `gaming-wine`. The `gaming-wine` rule is regex `wine.*\.exe` with only `protonmail` and `protonvpn` excluded — **any other wine-based app (installers, Office-under-wine, launcher stubs) is a false positive**. Find the culprit on the desktop:

```bash
for p in /proc/[0-9]*/cmdline; do tr '\0' ' ' < "$p" | grep -E 'wine.*\.exe|Plex Transcoder|SteamLaunch AppId=|lutris.*runner|heroic.*game' && echo "  <- $p"; done 2>/dev/null
```

Stopgap while the false-positive process must keep running: `POST /control {"mode":"serve"}` — but this also disables REAL gaming yield; set it back to `auto` promptly. Rule changes are code changes → `broker-change-control`.

### 2. 503 "GPU busy: wait budget exceeded"

Exact body: `broker: GPU busy: wait budget exceeded` (gate.go). Emitted when a request waited its full class budget (`BROKER_INTERACTIVE_WAIT` default 30s; `BROKER_BATCH_WAIT` default 5s) without getting the single GPU slot.

History that matters: on 2026-07-01 (commit `ad07905`) the live unit raised `BROKER_BATCH_WAIT` from 5s to **300s** because bulk RAG ingestion fast-failed behind ordinary interactive generations — a 5s budget cannot outlive one interactive response. If you see this symptom on batch, first confirm the live value:

```bash
# on the desktop
systemctl cat ollama-broker | grep BATCH_WAIT
```

If the budget is already generous and batch still defers, the work is too long-running for the synchronous batch lane — submit it as a durable Job instead (Jobs queue instead of failing, and preempted Jobs requeue at the front):

```bash
curl -s -X POST http://desktop.example.internal:11437/jobs \
  -H 'Idempotency-Key: my-unique-key-001' \
  -d '{"model":"<model>","prompt":"<prompt>"}'
```

### 3. Interactive stream cut mid-response

Mechanism: on yield the controller cancels the serve context; the gate aborts the in-flight upstream call. For a streamed (chunked) response the header `X-Broker-Status: served` was already sent, so the **authoritative outcome is the HTTP trailer** `X-Broker-Status`, which flips to `preempted` (gate.go sets it via `http.TrailerPrefix`). There is NO in-band error marker in the body — the NDJSON just stops, missing Ollama's terminal `{"done":true}` line. A NON-streamed request preempted before any body surfaces as a plain 503 instead.

See the trailer (note `--raw` shows the chunked framing and the trailer after the last chunk; plain `-i` will not show trailers):

```bash
curl --raw -is http://desktop.example.internal:11435/api/generate \
  -d '{"model":"<model>","prompt":"count to 100 slowly"}' | tail -5
```

Check for a complete stream instead:

```bash
curl -sN http://desktop.example.internal:11435/api/generate \
  -d '{"model":"<model>","prompt":"hi"}' | tail -1 | grep -c '"done":true'
```

`0` = the stream was cut. Correlate with `journalctl -u ollama-broker | grep '"yield start"'` timestamps. Preemption during real contention is by design; the Consumer must retry.

### 4. Jobs stuck QUEUED

```bash
curl -s http://desktop.example.internal:11437/jobs/<id> | python3 -m json.tool   # state, position, attempts
curl -s "http://desktop.example.internal:11437/jobs?state=QUEUED"   # states are UPPERCASE; the SQL match is case-sensitive — lowercase silently returns {"jobs":[]}
```

The worker loop (`internal/job/worker.go`) deliberately does NOT claim work while yielding ("hold the line"), and it acquires the batch slot BEFORE claiming — so a Job stays QUEUED (never RUNNING-but-idle) while a game plays or interactive traffic saturates the slot. Check in order:

1. `/status` `yield.yielding: true` — gaming/Plex session; Jobs drain afterward.
2. `/status` `queue.interactive` persistently > 0 — interactive storm; batch waits (a RUNNING Job also gets preempted after its 10s quantum, `BROKER_BATCH_QUANTUM`).
3. Control plane unreachable / no worker logs — the process itself:

```bash
# on the desktop
systemctl status ollama-broker
journalctl -u ollama-broker -n 100 | grep -E '"job |broker up'
```

### 5. Job FAILED "exceeded attempts"

Attempts semantics (ADR-0007, verified in `internal/job/sqlite.go` + `store.go`):

- **Never burns an attempt:** clean preemption (gaming or interactive) and explicit cancel — the Job requeues at the front with `attempts` unchanged.
- **Burns an attempt:** a genuine generation error (`FailOrRetry`, attempts++), and each broker-restart recovery of a RUNNING Job (`RecoverRunning`, attempts++). Cap is `BROKER_JOB_MAX_ATTEMPTS` (default 3).

The `error` field discriminates:

- `"exceeded attempts after restart"` — the exact string written ONLY by restart recovery (sqlite.go). The broker restarted (or crash-looped) 3+ times while this Job was RUNNING. Count restarts: `journalctl -u ollama-broker | grep -c '"broker up"'` and find out why the unit is cycling before resubmitting.
- Any other text — the LAST real generation error (bad model name, upstream failure). Fix the cause and resubmit with a fresh `Idempotency-Key` (the old key idempotently returns the FAILED Job).

Suspicious pattern: a Job whose runtime approaches Ollama's own limits crashing Ollama each attempt will burn all 3 attempts "legitimately" — check Ollama's service logs too.

### 6. Image embeddings nearly identical for different images

The trap (ADR-0008): Infinity's unified `POST /embeddings` tokenizes a base64 `data:` URI as **text**, returning near-identical text-tower vectors for every image. The Broker embed lane exists to prevent exactly this: `internal/proxy.NewEmbed` rewrites `/embeddings` and `/v1/embeddings` to Infinity's `/embeddings_image`, keeping the OpenAI wire contract for Consumers.

So if vectors collapse, the Consumer is almost certainly NOT going through the lane. Correct target: `http://desktop.example.internal:11438/embeddings`. Wrong targets that produce this symptom: Infinity `127.0.0.1:7997/embeddings` directly (only reachable from the desktop — loopback-only), or an Ollama port.

Discriminating experiment (on the desktop): embed two clearly different images through `:11438` and through `:7997/embeddings`; the lane's vectors differ per image, the direct ones barely do. Then audit the consumer's base URL config.

### 7. Embed lane connection refused / 404

The lane is optional: `cmd/broker/main.go` starts it only when `INFINITY_URL` is set, logging `"embed lane enabled"` with addr and upstream at startup.

```bash
# on the desktop
journalctl -u ollama-broker | grep "embed lane enabled"
curl -s http://desktop.example.internal:11438/health        # passes through to Infinity
curl -s http://127.0.0.1:7997/health           # desktop only: Infinity direct
```

- No startup log line → `INFINITY_URL` unset in the unit (live value as of 2026-07-02: `http://127.0.0.1:7997`). Fix the unit env, restart.
- Lane up but 502 (`upstream error` in logs) → Infinity itself is down on `:7997`.
- 503 `yielding GPU` → the lane SHARES the yield controller (by design, even though Infinity runs on CPU); see symptom 1.

### 8. Broker does NOT yield while a game runs

Detection (`internal/detect/detect.go`, ported verbatim from Bash V3, first match wins) matches process **cmdlines** only:

| Reason | Match |
|---|---|
| `plex` | substring `Plex Transcoder` |
| `gaming-steam` | substring `SteamLaunch AppId=` |
| `gaming-lutris` | regex `lutris.*runner` |
| `gaming-heroic` | regex `heroic.*game` |
| `gaming-wine` | regex `wine.*\.exe`, excluding cmdlines containing `protonmail` or `protonvpn` |

Consequences:

- A new launcher (native games, Bottles, itch, emulators, bare `gamescope`) matches nothing → no yield. Verify on the desktop: `tr '\0' ' ' < /proc/<game-pid>/cmdline` and eyeball against the table.
- Detection reads `/proc` and is **Linux-only**; on macOS `ProcLister` silently returns no processes (fail-open by design — never block inference because `/proc` was unreadable). A dev Mac will NEVER detect contention; that is not a bug.
- `yield.mode: "serve"` in `/status` means a `ModeForceServe` override is suppressing detection entirely.

Immediate mitigation for an unmatched game:

```bash
curl -s -X POST http://desktop.example.internal:11437/control -d '{"mode":"yield"}'
# ... after gaming:
curl -s -X POST http://desktop.example.internal:11437/control -d '{"mode":"auto"}'
```

Permanent fix = new detection rule = code change with tests → `broker-change-control`.

### 9. `go test ./...` fails at HEAD

Known-broken as of 2026-07-02: `internal/admin/admin_test.go:31` still calls `Mux` with 5 args; the signature is now `Mux(Controller, StatsProvider, http.Handler, http.Handler, func() any, TdarrStatusFn)` — broken by the Tdarr commit `dd39d20` (2026-06-29). `go vet ./...` fails identically. Every other package passes. If you are debugging something else, ignore it (`go test $(go list ./... | grep -v internal/admin)`); fixing it belongs to the test-discipline work, see `broker-validation-and-qa`.

### 10. Tdarr GPU workers not restored after yield

Tdarr integration activates only when BOTH `BROKER_TDARR_URL` and `BROKER_TDARR_NODE_ID` are set (main.go). On yield the controller calls `PauseGPU` (sets the node's `transcodegpu` workers to 0); on yield-stop `ResumeGPU` restores `BROKER_TDARR_GPU_WORKERS` (default 1; live unit sets 2 as of 2026-07-02). Separately, the internal-scraper-service schedule (Fri 02:00–07:00, hardcoded in `internal/schedule`) pauses GPU workers regardless of yield.

```bash
curl -s http://desktop.example.internal:11437/status | python3 -c "import json,sys; print(json.load(sys.stdin).get('tdarr'))"
```

- `None` (no `tdarr` key) → integration disabled: a var is missing. Deploy trap: repo `deploy/broker.service` contains NO Tdarr vars, so reinstalling the unit from the repo silently disables Tdarr management.
- `{'gpu_workers': -1, 'managed': True}` → the Tdarr API query failed; check Tdarr at `http://localhost:8265` on the desktop and `journalctl -u ollama-broker | grep tdarr`.
- `gpu_workers: 0` on a Friday 02:00–07:00 → schedule window, expected; auto-resumes at 07:00 (log: `tdarr schedule resume`).
- `gpu_workers: 0` outside the window, not yielding → look for `"tdarr: resume GPU failed"` in logs (Resume has a 10s timeout and only warns); manually set workers in Tdarr UI, then investigate.

### 11. Weird contention behavior / double arbitration

Verified 2026-07-02: the legacy Bash daemon **`resource-manager.service`** (`/usr/local/bin/resource-manager.sh`) is still running on the desktop alongside the Broker. `docs/DESIGN.md` explicitly forbids two uncoordinated GPU arbiters; the promised retire-after-soak never happened. It can independently unload models, fight over Ollama state, and produce flapping the Broker's own logs cannot explain.

Treat it as a suspect whenever behavior contradicts what `journalctl -u ollama-broker` says happened. Correlate:

```bash
# on the desktop
journalctl -u resource-manager --since "-1h" --no-pager
journalctl -u ollama-broker --since "-1h" --no-pager | grep -E 'yield|unload'
```

**Do NOT stop or disable `resource-manager.service` from this playbook.** Retiring it is a decision-gated operation with rollback — that is `broker-cutover-hardening-campaign`.

### 12. All lanes down

The live unit name is `ollama-broker.service` (README's deploy section says `broker.service` — doc drift; binary at `/usr/local/bin/ollama-broker`).

```bash
# on the desktop
systemctl status ollama-broker
journalctl -u ollama-broker -n 50 --no-pager
```

Startup landmarks in the log: one `"listening"` line per lane, then `"broker up"`. Failure signatures: `"config"` + exit (bad env var — durations use Go `time.ParseDuration`; all int vars must be >= 1), `"open job store"` (SQLite path/permissions — live DB at `/var/lib/ollama-broker/jobs.db`), `"listen <addr>: address already in use"` (port conflict — remember raw Ollama already owns `:11434` on all interfaces).

## Traps that cost real time

One line each; the full stories live in `broker-failure-archaeology`.

- **5s batch wait (fixed 2026-07-01, commit `ad07905`):** the default `BROKER_BATCH_WAIT=5s` made bulk RAG ingestion fast-fail with "GPU busy: wait budget exceeded" any time one interactive generation was mid-flight — live raised it to 300s.
- **Text-tower vectors (ADR-0008):** Infinity's unified `/embeddings` tokenized base64 images as text, returning near-identical vectors for every image — the embed lane's `/embeddings_image` rewrite exists because this was actually hit.
- **Circular GPU-% detection (V1/V2 Bash era):** detecting gaming by GPU utilization made Ollama's own inference look like contention; V3/Go switched to process-name detection ("detect WHAT is using resources, not THAT resources are high").

## Provenance and maintenance

Facts above verified 2026-07-02 against branch `v2-go` and the shared live-deployment brief. Re-verify before trusting, from `/Users/prestonbernstein/dev/ollama-resource-broker`:

- 503 strings and trailer logic: `grep -n 'yielding GPU\|wait budget exceeded\|TrailerPrefix' internal/queue/gate.go`
- Detection rules: `grep -n 'Plex Transcoder\|SteamLaunch\|lutris\|heroic\|wine' internal/detect/detect.go`
- Yield modes and `/control` contract: `grep -n 'ParseMode\|ModeForce' internal/yield/yield.go internal/admin/admin.go`
- Attempts semantics: `grep -n 'exceeded attempts\|FailOrRetry\|RecoverRunning' internal/job/sqlite.go internal/job/store.go`
- Embed lane rewrite: `grep -n 'embeddings_image\|/v1/embeddings' internal/proxy/proxy.go`
- Tdarr gating + schedule window: `grep -n 'TdarrURL\|TdarrNodeID\|SafeForBackgroundGPU' cmd/broker/main.go internal/schedule/*.go`
- Defaults (waits, quantum, attempts): `grep -n 'BROKER_' internal/config/config.go`
- Known-broken test still broken? `go test ./internal/admin/ 2>&1 | head -5`
- Live unit name / legacy daemon / port map / 300s override: these are dated live-desktop findings from the 2026-07-02 brief — re-verify on the desktop (`systemctl status ollama-broker resource-manager`, `systemctl cat ollama-broker`) before relying on them; do not assume they still hold.
