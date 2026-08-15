---
name: broker-config-and-flags
description: >
  Complete reference for every Broker configuration axis: all environment
  variables (defaults, parsers, validation, which component consumes each),
  live-vs-repo config drift, and the checklist for adding a new config var.
  Load this when you need to know what an env var does, why a value in
  deploy/broker.service differs from code defaults, why "invalid BROKER_X"
  or "must be >= 1" errors appear at startup, why the embed lane (:11438)
  or Tdarr integration is silently off, how BROKER_BATCH_WAIT ended up at
  300s, or when adding/renaming/removing a config variable. Keywords:
  OLLAMA_URL, INFINITY_URL, BROKER_TDARR_URL, BROKER_BATCH_WAIT,
  BROKER_MAX_INFLIGHT, getenv, getint, getdur, config.Load, Environment=.
---

# Broker configuration and flags

The Broker (single Go binary, `cmd/broker`) is configured **exclusively via
environment variables**. There are no CLI flags — `cmd/broker/main.go` does not
import `flag` and takes no arguments; "flags" in this skill's name means config
knobs, not command-line flags. All parsing lives in one place:
`internal/config/config.go`, function `Load()`, called once at startup. Any
invalid value makes the process exit immediately with a `config` error log
(exit code 1) — there is no partial startup.

The only other env-adjacent production code is nothing: `grep` confirms
`os.Getenv`/`os.LookupEnv` appear only inside `internal/config/config.go`
(as of 2026-07-02). If you need to know why the Broker reads an env var,
config.go is the whole story.

## When NOT to use this skill

- **"Why is the architecture shaped this way?"** (why two ports, why one
  inflight slot, why the embed lane has its own scheduler) →
  `broker-architecture-contract`.
- **Deploy mechanics** (installing the systemd unit, StateDirectory, control
  plane operations, consumer port map) → `broker-run-and-operate`.
- **Build/test commands and dev-env setup** → `broker-build-and-env`.
- **A config value seems right but behavior is wrong** →
  `broker-debugging-playbook`.

## 1. The complete env var table

Ground truth: `internal/config/config.go` `Load()` (defaults come from **code**,
not the README — the README table is incomplete, see the drift map below).
"Live override" = the running `resource-broker.service` unit on the desktop as
observed 2026-07-02 (from the shared authoring brief; do not re-probe).

Parsers (see section 2 for their gotchas):

- **getenv** — string with default; set-but-empty counts as unset.
- **getint** — `strconv.Atoi`, then **rejects any value < 1**.
- **getdur** — Go `time.ParseDuration` (`"300s"`, `"10m"`, `"168h"`).
- **URL** — `url.Parse` plus an explicit scheme+host check.

| Var | Default (code) | Parser | Consumed by | Meaning | Status | Live override (2026-07-02) |
|---|---|---|---|---|---|---|
| `OLLAMA_URL` | `http://127.0.0.1:11434` | URL (scheme+host required) | `proxy.New` (reverse proxy) and `ollama.New` (upstream client: Generate, Unload) in `main.go` | Upstream real Ollama base URL | Production | — (default) |
| `INFINITY_URL` | unset (`nil`) | URL, optional; empty ⇒ `InfinityURL == nil` | `main.go`: if nil the embed lane is **not started at all** — no listener on `EmbedAddr`; else `proxy.NewEmbed` | Upstream Infinity SigLIP image-embedding server (ADR-0008) | Production | `http://127.0.0.1:7997` (line duplicated in live unit — harmless, systemd last-wins) |
| `BROKER_INTERACTIVE_ADDR` | `:11435` | getenv | `main.go` interactive server (`sched.Gate(queue.Interactive, ...)`) | Listen address, high-priority class | Production | — |
| `BROKER_BATCH_ADDR` | `:11436` | getenv | `main.go` batch server (`sched.Gate(queue.Batch, ...)`) | Listen address, low-priority class | Production | — |
| `BROKER_CONTROL_ADDR` | `:11437` | getenv | `main.go` control-plane server (`admin.Mux`: `/control`, `/status`, `/metrics`, `/healthz`, `/jobs`) | Listen address, admin/control plane + Job API | Production | — |
| `BROKER_EMBED_ADDR` | `:11438` | getenv | `main.go` embed-lane server (only when `INFINITY_URL` set) | Listen address, image-embedding lane | Production | `:11438` (also duplicated in live unit) |
| `BROKER_EMBED_TIMEOUT` | `30s` | getdur | `embedSched.Gate(queue.Batch, cfg.BatchWait, cfg.EmbedTimeout, ...)` — the only `Gate` call site that passes a nonzero `upstreamTimeout` | Bounds how long the embed lane's own upstream call may run once admitted; `0` disables (ADR-0013). Interactive/batch Ollama lanes stay unbounded (0) on purpose — a real generation can run minutes | Production (added ADR-0013) | — |
| `BROKER_INTERACTIVE_WAIT` | `30s` | getdur | Interactive `sched.Gate` wait budget | How long an interactive request may queue for the GPU slot before 503 | Production | — |
| `BROKER_BATCH_WAIT` | `5s` | getdur | Batch `sched.Gate` AND the embed lane's gate (`main.go` passes `cfg.BatchWait` to both) | How long batch/embed requests may queue before 503 | Production | **`300s`** — repo unit and live unit both override; see drift map |
| `BROKER_DETECT_INTERVAL` | `3s` | getdur | `yield.NewWithConfirm(detector, oc, cfg.DetectInterval, cfg.YieldConfirmPolls)` | How often the yield controller polls process detection for Contention | Production | — |
| `BROKER_YIELD_CONFIRM_POLLS` | `2` | getint (>= 1) | `yield.Controller.debounceLocked` | Consecutive same-reason detections required before entering yield (ADR-0012); filters single-poll false positives. Clearing is never debounced | Production (added ADR-0012) | — |
| `PLEX_URL` | `http://localhost:32400` | getenv | `plex.New(cfg.PlexURL, cfg.PlexToken)`, wired in `main.go` only when `PLEX_TOKEN` is set | Local Plex Media Server base URL, queried for `/status/sessions` to corroborate a "Plex Transcoder" process match (ADR-0012) | Production (added ADR-0012) | — |
| `PLEX_TOKEN` | `""` (disabled) | getenv | same as `PLEX_URL` | Plex API token. Unset disables session corroboration entirely — a bare process-name match is then treated as contention, same as before ADR-0012 | Production (added ADR-0012); deploy unit ships this commented out until a token is provisioned | — |
| `BROKER_MAX_WAITERS` | `256` | getint (>= 1) | `sched.SetMaxWaiters` on the Ollama scheduler AND the embed lane's own scheduler | Max queued requests **per class** before fast 503 | Production | — |
| `BROKER_MAX_INFLIGHT` | `1` | getint (>= 1) | `sched.SetMaxInflight` (Ollama scheduler only; the embed scheduler is hardcoded to 1 — Infinity saturates all CPU cores per request) | Max concurrent requests reaching Ollama (ADR-0004) | Production | — |
| `BROKER_BATCH_QUANTUM` | `10s` | getdur | `job.NewWorker(..., cfg.BatchQuantum, 0)` | Min-run window protecting a RUNNING Job before an interactive request may trigger Preemption | Production | — |
| `BROKER_JOB_DB` | `broker-jobs.db` | getenv | `job.OpenSQLite(cfg.JobDBPath)` | SQLite file backing the durable Job queue (ADR-0007). Note: bare default is relative to the process CWD | Production | `/var/lib/ollama-broker/jobs.db` (via `StateDirectory=ollama-broker`, set in repo unit too) |
| `BROKER_JOB_MAX_ATTEMPTS` | `3` | getint (>= 1) | `job.NewService(store, cfg.JobMaxAttempts)` | Re-runs of a restart-interrupted Job before it is FAILED | Production | — |
| `BROKER_JOB_PRUNE_INTERVAL` | `10m` | getdur | `jobSvc.RunPrune` | How often terminal Jobs are swept | Production | — |
| `BROKER_JOB_FETCHED_GRACE` | `1h` | getdur | `jobSvc.RunPrune` | How long a fetched Job result is retained before pruning | Production | — |
| `BROKER_JOB_HARD_CAP` | `168h` (7d) | getdur | `jobSvc.RunPrune` | Max age of any terminal Job before pruning | Production | — |
| `BROKER_TDARR_URL` | `""` (disabled) | getenv | `main.go`: gates the entire Tdarr block; `tdarr.New` | Tdarr server base URL (e.g. `http://localhost:8265`). Empty disables cooperative GPU management | Production **but NOT in the README table — doc drift** | `http://localhost:8265` |
| `BROKER_TDARR_NODE_ID` | `""` | getenv | `tdarr.New` | Tdarr node `_id` whose GPU workers the Broker manages | Production **but NOT in the README table** | `<node-id>` (machine-specific; read it from the Tdarr UI/API, never hardcode) |
| `BROKER_TDARR_GPU_WORKERS` | `1` | getint (>= 1) | `tdarr.New` (restore target; note `/status` `tdarr.gpu_workers` shows the LIVE count queried from Tdarr, not this configured value) | How many `transcodegpu` workers to restore after Yield or the estate-scraper window ends | Production **but NOT in the README table** | **`2`** |
| `BROKER_PARK_HOLD` | `600s` | getdur | `Scheduler.parkFor` (Batch requests during yield); wired via `sched.SetParkConfig` in `main.go` | Max time a Batch request may remain parked before 503/expired (per ADR-0009) | Production | — |
| `BROKER_PARK_MAX_QUEUE` | `32` | getintMin (>= 0; 0 = kill-switch) | `Scheduler.park` queue ceiling; wired via `sched.SetParkConfig` in `main.go` | Max parked Batch requests before new arrivals rejected with 503/park_rejected; 0 disables parking entirely (fail-closed, immediate 503 during yield) | Production | — |
| `BROKER_PARK_DRAIN_BURST` | `8` | getint (>= 1) | `Scheduler.RunParkDrain` burst loop (release pacing) | Batch requests released per drain tick (~1s polling); controls throughput of parked→waiting transition (per ADR-0009) | Production | — |

**Loud flag — README doc drift (as of 2026-07-02):** the README config table
(README.md lines ~54–70) documents 17 vars and **omits all three Tdarr vars**,
which shipped in the Tdarr commit `dd39d20` (2026-06-29) without a README
update. Until someone fixes the README (that fix routes through
`broker-change-control` + `broker-docs-and-writing`), **this table is the
complete one and the README is not.**

There is no `BROKER_CONTROL_TOKEN` or any auth-related var: ADR-0005
(control-plane auth) was accepted but never implemented — the var appears
nowhere in code as of 2026-07-02.

## 2. Gotchas — read before setting anything

1. **You cannot set an int to 0.** `getint` rejects values < 1 for **every**
   int var (`BROKER_MAX_WAITERS=0` → startup failure:
   `BROKER_MAX_WAITERS must be >= 1, got 0`). "Disable" is never expressed
   numerically. Features are disabled via **empty/unset string vars**:
   unset `INFINITY_URL` kills the embed lane; empty `BROKER_TDARR_URL` (or
   `BROKER_TDARR_NODE_ID`) kills Tdarr management.

2. **Set-but-empty equals unset.** `getenv` treats `ok && v != ""` as the only
   "set" case; `getint`/`getdur` likewise fall back to the default on empty.
   `Environment=BROKER_BATCH_WAIT=` in a unit file silently gives you the
   **code default (5s)**, not an error. Conversely this means an empty string
   can never *override* a non-empty default — to disable Tdarr you rely on the
   default already being `""`, i.e. you simply omit the lines.

3. **Durations are Go `time.ParseDuration` syntax.** Valid: `300s`, `5m`,
   `1h30m`, `168h`, `1500ms`. Invalid: `300` (bare number — no unit), `5 s`
   (space), `7d` (**no day unit in Go**; write `168h`). Failure is fatal at
   startup: `invalid BROKER_BATCH_WAIT "soon": ...`.

4. **URLs must carry scheme AND host.** `OLLAMA_URL=127.0.0.1:11434` fails
   (`must include scheme and host`) — write `http://127.0.0.1:11434`. Same
   check for `INFINITY_URL` when set.

5. **`INFINITY_URL` unset means no listener on `:11438` at all.** `main.go`
   only appends the embed server when `cfg.InfinityURL != nil`. Symptom of
   forgetting it: `curl http://host:11438/...` → connection refused, no error
   in Broker logs (the "embed lane enabled" log line is simply absent).

6. **Tdarr needs BOTH `BROKER_TDARR_URL` and `BROKER_TDARR_NODE_ID`.** The
   gate in `main.go` is `cfg.TdarrURL != "" && cfg.TdarrNodeID != ""`. Setting
   only one silently disables the whole integration — no warning is logged.
   When both are set you get an explicit startup log:
   `tdarr integration enabled`. Verify via `GET /status` on `:11437`
   (`tdarr.managed: true`).

7. **`BROKER_BATCH_WAIT` also budgets the embed lane.** `main.go` reuses
   `cfg.BatchWait` for the embed lane's gate; there is no separate embed wait
   var. Raising batch wait raises embed wait too.

8. **`BROKER_MAX_INFLIGHT` does not apply to the embed lane.** The embed
   scheduler is hardcoded `SetMaxInflight(1)` in `main.go`. Only
   `BROKER_MAX_WAITERS` is shared with it.

9. **Relative `BROKER_JOB_DB` default.** `broker-jobs.db` resolves against the
   process working directory — fine for `make run` in the repo, wrong under
   systemd. The unit files pin it to `/var/lib/ollama-broker/jobs.db` with
   `StateDirectory=ollama-broker` (required: `ProtectSystem=strict` makes the
   rest of the filesystem read-only).

## 3. Drift map (as of 2026-07-02 — re-verify before relying on it)

| # | Drift | Detail | Risk |
|---|---|---|---|
| 1 | Code default vs deployed `BROKER_BATCH_WAIT` | Code (and README) say `5s`; **both** the repo `deploy/broker.service` and the live unit set `300s`. Rationale (commit `ad07905`, 2026-07-01, quoted in the unit comment): a 5s budget made bulk RAG/embedding ingestion fast-fail with "GPU busy: wait budget exceeded" whenever an interactive generation was mid-flight; batch must wait patiently, not fast-fail. | Anyone "resetting to defaults" reintroduces the ingestion failure. The code default was deliberately left at 5s; the fix lives in deployment config. |
| 2 | Repo unit vs live unit: Tdarr vars | `deploy/broker.service` in the repo has **no** Tdarr `Environment=` lines. The live unit sets `BROKER_TDARR_URL=http://localhost:8265`, `BROKER_TDARR_NODE_ID=<node-id>`, `BROKER_TDARR_GPU_WORKERS=2`. | **Reinstalling from the repo unit would silently disable Tdarr cooperative GPU management** (silent per gotcha 6). Repo unit needs the lines added before any redeploy. |
| 3 | Duplicated lines in live unit | Live unit repeats the `INFINITY_URL` and `BROKER_EMBED_ADDR` `Environment=` lines. systemd applies last-wins; values are identical, so harmless — but it is drift and invites a future divergent edit. | Cosmetic today; clean up on next unit change. |
| 4 | Unit name mismatch | README's deploy section installs `broker.service`; the live unit is named **`resource-broker.service`** (User/Group `ollama-broker`, binary `/usr/local/bin/resource-broker`). | `systemctl status broker` on the desktop finds nothing; scripts following the README target the wrong unit name. Deploy mechanics → `broker-run-and-operate`. |
| 5 | README table missing Tdarr vars | See section 1. | Readers of the README believe Tdarr is not configurable. |
| 6 | Live `BROKER_TDARR_GPU_WORKERS=2` vs default `1` | Deliberate live tuning; not reflected anywhere in the repo. | Same reinstall hazard as #2. |

## 4. How to add a config axis (checklist)

Adding an env var is a code change: **classify it via `broker-change-control`
first** (it touches config, deploy, docs, and possibly ADR territory — do not
route around that). Then follow the wiring pattern every existing var uses.
Verified against how `INFINITY_URL`/`BROKER_EMBED_ADDR` and the Tdarr trio are
wired (the Tdarr trio is also the cautionary tale: it skipped steps 4 and got
the README drift above).

1. **Field on `Config`** in `internal/config/config.go` — exported, with a doc
   comment stating meaning and, where relevant, the disable semantics
   (house style: `// TdarrURL is ... Empty string disables ...`) and the ADR
   number if one applies.
2. **Parse in `Load()`** with an explicit default:
   - string → `getenv("BROKER_X", "default")` inline in the struct literal;
   - int → `getint(...)` above the literal with the `if err != nil { return nil, err }` pair (remember: >= 1 enforced);
   - duration → `getdur(...)` same shape;
   - URL → follow the `INFINITY_URL` block: optional means empty ⇒ `nil` pointer, set ⇒ parse + scheme/host check.
3. **Test in `internal/config/config_test.go`.** House style there: plain
   table-free tests using `t.Setenv` (auto-restores env per test), one test per
   concern — default resolution (set var to `""`, assert code default:
   `TestLoadDefaults`), explicit override (`TestLoadOverrides`), and an
   invalid-value case asserting `Load()` returns an error
   (`TestLoadInvalidMaxWaiters`, `TestLoadInvalidDuration`,
   `TestLoadInvalidInfinityURL`). Add all three shapes for a new var.
4. **README config table row** (README.md, the `| Var | Default | ... |`
   table) — default must match the **code** default, not any deployed value.
5. **`deploy/broker.service` `Environment=` line** with a comment explaining
   any non-default value (see the `BROKER_BATCH_WAIT=300s` comment block for
   the expected comment style). If the live unit needs the var too, that is a
   deploy action → `broker-run-and-operate`.
6. **Consume in `cmd/broker/main.go`** — the Config field is dead weight until
   `main.go` (or something it constructs) reads it. If the var enables an
   optional feature, log an explicit `slog.Info("... enabled", ...)` line at
   startup like the embed lane and Tdarr do, so operators can confirm wiring
   from journal output.
7. **Add a row to this skill's table** (section 1) with parser, consumer, and
   status.
8. Run `go test ./internal/config/` (must pass) and `go test ./...`
   (expect `internal/admin` to fail at HEAD as of 2026-07-02 — known broken,
   not yours to fix in a config change; everything else must pass).

Renaming or removing a var is the same list in reverse plus a deprecation
decision — definitely `broker-change-control` territory.

## 5. Re-verification one-liners

Run these to regenerate the ground truth instead of trusting this file's
snapshot:

```sh
# Every parser call = every env var the Broker reads, with defaults:
grep -n 'getenv\|getint\|getdur' /Users/prestonbernstein/dev/resource-broker/internal/config/config.go

# Confirm config.go is the ONLY production env-read site:
grep -rn 'os.Getenv\|os.LookupEnv' /Users/prestonbernstein/dev/resource-broker --include='*.go' | grep -v _test.go | grep -v /legacy/

# README table rows vs config.go vars (any var in the 2nd list but not the 1st is doc drift):
grep -oE 'OLLAMA_URL|INFINITY_URL|BROKER_[A-Z_]+' /Users/prestonbernstein/dev/resource-broker/README.md | sort -u
grep -oE '"(OLLAMA_URL|INFINITY_URL|BROKER_[A-Z_]+)"' /Users/prestonbernstein/dev/resource-broker/internal/config/config.go | tr -d '"' | sort -u

# Repo unit's env lines (compare against the live unit via broker-run-and-operate procedures):
grep -n '^Environment=' /Users/prestonbernstein/dev/resource-broker/deploy/broker.service

# Where each Config field is consumed:
grep -n 'cfg\.' /Users/prestonbernstein/dev/resource-broker/cmd/broker/main.go

# Config package tests still green:
go test /Users/prestonbernstein/dev/resource-broker/internal/config/
```

## Provenance and maintenance

- Written 2026-07-02 against branch `v2-go` (clean, synced with origin).
- Sources: `internal/config/config.go` + `config_test.go` (read line by line),
  `cmd/broker/main.go`, `deploy/broker.service`, README.md config table,
  plus the dated live-deployment findings in the shared authoring brief
  (read-only ssh check, 2026-07-02). Live facts (`300s`, Tdarr trio values,
  duplicated lines, unit name) are a snapshot — re-check the live unit via
  `broker-run-and-operate` procedures before acting on them.
- Volatile items most likely to rot: the drift map (any of #1–#6 may be fixed),
  the README-omits-Tdarr claim, the "ADR-0005 unimplemented / no auth var"
  claim, and the "internal/admin fails at HEAD" note. The section 5 one-liners
  regenerate the table and the README-vs-code diff; for the rest, grep for
  `BROKER_CONTROL_TOKEN` and run `go test ./...`.
- If you add/change a var and do not update this table, you have recreated
  drift #5. Step 7 of the checklist exists for a reason.
