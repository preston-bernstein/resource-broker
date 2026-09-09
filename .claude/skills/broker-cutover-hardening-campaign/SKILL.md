---
name: broker-cutover-hardening-campaign
description: The executable, decision-gated campaign to finish the resource-broker v2 cutover and hardening — fix the test suite broken at HEAD, reconcile deploy/README drift, implement ADR-0005 control-plane auth, retire the legacy resource-manager.service still running on the desktop, and close the v2-go/main branch question. Load when asked to "finish the cutover", "retire the legacy daemon/resource-manager", "implement ADR-0005 / BROKER_CONTROL_TOKEN", "fix the failing admin test", "reconcile deploy drift", or to plan/execute any multi-step hardening of the deployed broker. Every phase has exact commands, expected observations, branch-on-mismatch instructions, and STOP gates for anything touching the live desktop. NOT for routine operations (broker-run-and-operate) or one-off debugging (broker-debugging-playbook).
---

# Broker cutover and hardening campaign

**ASSUMPTION (A1, unconfirmed):** this campaign is framed as the project's hardest live problem — the v2 cutover that docs promised but never completed. Confirm priorities with Preston before executing any desktop-mutating phase (P3-deploy, P4). Repo-only phases (P1, P2, P3-code) are safe to execute under normal change control.

Baseline facts in this file were verified 2026-07-02 (branch `v2-go`; live desktop via read-only ssh). Every phase re-verifies its own preconditions first — if reality differs from "expected", STOP and re-diagnose before proceeding; the drift you find is itself a finding for `broker-failure-archaeology`.

Rules of engagement: all changes route through `broker-change-control` (classification, ADR where required, test+vet green, review). Desktop mutations additionally require: explicit owner confirmation, a written rollback line, and the safe-window discipline from `broker-run-and-operate` (never during gaming/Plex or Friday 02:00–07:00; note that evening batch traffic is real — a 2026-07-02 21:00 check found an in-flight batch generation and Plex-triggered yields).

## P0 — Baseline snapshot (read-only, do first, every time)

```sh
cd /Users/prestonbernstein/dev/resource-broker
git status --short && git log --oneline -3          # expect: clean tree on v2-go
go test ./... 2>&1 | tail -8
go vet ./... 2>&1 | tail -4
ssh <broker-host> 'systemctl is-active resource-broker resource-manager; curl -s localhost:11437/status'
```

Expected (2026-07-02): test+vet FAIL only in `internal/admin` (`admin_test.go` `Mux` arity — `have (Controller, fakeStats, http.HandlerFunc, nil, nil)` / `want (..., func() any, TdarrStatusFn)`); both services `active`; `/status` returns the five-section JSON (see `broker-diagnostics-and-tooling`).

Record the output to a dated log. Branches: extra test failures beyond internal/admin → someone changed code since this was written; triage those FIRST via `broker-debugging-playbook`. `resource-manager` inactive → P4 may already be done; verify with `systemctl is-enabled` and update this skill's status. `/status` unreachable → live incident, not campaign work.

## P1 — Fix the test suite (repo-only, SAFE, no gate beyond change control)

Goal: `go test ./...` and `go vet ./...` fully green.

1. Read the current signature: `grep -n 'func Mux' internal/admin/admin.go` → `Mux(ctrl Controller, stats StatsProvider, metricsHandler http.Handler, jobs http.Handler, jobStatus func() any, tdarrStatus TdarrStatusFn) http.Handler`.
2. Fix `internal/admin/admin_test.go` (~line 29-31, the `newMux` helper): add the one missing trailing arg (`tdarrStatus` — the helper already passes two nils for `jobs`/`jobStatus`). `nil` is valid: `admin.go` guards both optional params (`if jobStatus != nil`, `if tdarrStatus != nil`). Better: also add one test passing a non-nil `tdarrStatus` returning `&TdarrStatus{Managed: true, GPUWorkers: 2}` and assert `/status` contains a `tdarr` section, and a non-nil `jobStatus` asserting a `jobs` section — these paths are untested today.
3. Add first-ever tests for `internal/schedule` (gap since dd39d20): pin the half-open boundary semantics of `contains()` — `tod >= start && tod < end`, weekday-scoped, evaluated in LOCAL time. Use explicit `time.Date(..., time.Local)` constructions: internal-scraper-service Fri 01:59:59→false, 02:00:00→true, 06:59:59→true, 07:00:00→false; a Thursday 03:00→false; `SafeForBackgroundGPU` false only inside internal-scraper-service. Note for the test comment: `contains` cannot represent midnight-spanning windows (end beyond 24h never matches) — current windows don't cross midnight, so this is a documented limitation, not a bug to fix here.
4. Add first-ever tests for `internal/tdarr`: `httptest.NewServer` fake asserting `PauseGPU`/`ResumeGPU` POST `/api/v2/poll-worker-limits` and `/api/v2/update-node` with the expected JSON bodies, and `WorkerLimits` parsing. Read `internal/tdarr/tdarr.go` for exact payload shapes before writing.
5. Gate: `go test ./... && go vet ./... && go test -race ./...` all green. Commit per house style (see `broker-docs-and-writing`); classification: test-only change, no ADR.

If the Mux signature has changed AGAIN since 2026-07-02 → re-derive the fix from `admin.go`, and check `git log internal/admin/` for what happened.

## P2 — Reconcile config/docs drift (repo-only, SAFE)

Goal: one source of truth; a fresh deploy from the repo must not silently change live behavior.

1. `deploy/broker.service`: add the three missing Tdarr lines with a comment block (pattern: the existing INFINITY comment):
   `Environment=BROKER_TDARR_URL=http://localhost:8265`, `Environment=BROKER_TDARR_NODE_ID=<node-id>`, `Environment=BROKER_TDARR_GPU_WORKERS=2` — keep `<node-id>` a placeholder in the repo (machine-specific; live value is set on the desktop unit).
2. `README.md` config table: add the three Tdarr rows (defaults from `internal/config/config.go`: "", "", 1); fix the deploy section to install/enable `resource-broker.service` (the live unit name) instead of `broker.service`, or explicitly document the rename step.
3. Decide the `BROKER_BATCH_WAIT` question EXPLICITLY (do not bury it): code default 5s vs repo-unit/live 300s. Options: (a) leave as-is with a README note (unit overrides are the tuning layer) — cheapest, recommended; (b) raise the code default via ADR. Do NOT silently change the code default (fenced below).
4. Note for next deploy: the LIVE unit has duplicated `INFINITY_URL`/`BROKER_EMBED_ADDR` Environment lines (last-wins, harmless) — clean up when the unit is next reinstalled, not before.
5. Consistency check + gate:
```sh
for v in $(grep -o 'BROKER_[A-Z_]*\|OLLAMA_URL\|INFINITY_URL' internal/config/config.go | sort -u); do
  grep -q "$v" README.md || echo "MISSING FROM README: $v"; done
```
   Expected after fix: no output. Classification: docs/deploy-file change; no ADR needed (records existing reality).

## P3 — Implement ADR-0005 control-plane auth (repo code + gated deploy)

Status precondition: `grep -rn BROKER_CONTROL_TOKEN internal cmd` → empty (unimplemented since ADR acceptance 2026-06-16). If non-empty, someone started it — read before continuing.

Design obligations (from ADR-0005, read it first — `docs/adr/0005-control-plane-auth.md`):
- Reads stay OPEN: `GET /metrics`, `/healthz`, `/status`, `GET /control` (Grafana on the NAS scrapes across the LAN).
- Mutations gated: `POST /control` — and decide explicitly whether `POST /jobs` and `POST /jobs/{id}/cancel` count as mutations under the ADR's intent (the ADR predates the Job API; a LAN device force-canceling Jobs is a real, if smaller, DoS). Whatever you decide, record it as an amendment note on ADR-0005.
- Token set (`BROKER_CONTROL_TOKEN`): require `Authorization: Bearer <token>` on gated routes → 401 (missing/malformed) / 403 (wrong token).
- Token unset: gated routes accepted only from loopback (zero-config-safe default). Determine loopback from the connection's RemoteAddr, NOT from headers (X-Forwarded-For is spoofable).
- Config wiring follows the full checklist in `broker-config-and-flags` (config.go + test + README + deploy unit comment showing the var commented-out by default + this campaign's P2 consistency check).
- Tests: token-set accept/reject, token-unset loopback accept + non-loopback reject (httptest with a faked RemoteAddr), reads always open. Update ADR-0005 status line to implemented per `broker-docs-and-writing` style.

Gate (repo): tests green, review per change control. Classification: behavior change → needs the ADR status amendment, which exists (0005).

DEPLOY sub-phase (desktop, STOP — confirm with Preston):
1. Build `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` (see `broker-build-and-env`), stage alongside a dated backup of the current binary.
2. Pick a safe window (idle, not Friday early morning; check `/status` busy=false and no yield).
3. Install + restart; verify: `curl -s localhost:11437/status` OK from the box; from the Mac WITHOUT token: `curl -XPOST http://<broker-host>:11437/control -d '{"mode":"auto"}'` → expect 401/403; loopback without token still works (if token unset) or with token works.
4. Rollback line: reinstall backed-up binary, `sudo systemctl restart resource-broker`.
5. If a token is provisioned, it lives in a systemd drop-in or environment file on the desktop — NEVER in this repo or any skill file.

## P4 — Retire resource-manager.service (desktop, MOST DANGEROUS, STOP-gated)

The legacy Bash arbiter still runs alongside the Broker (verified 2026-07-02) — DESIGN.md: the legacy daemon "must not be run alongside the Broker (two uncoordinated GPU arbiters)". But do NOT assume it is inert.

1. INVESTIGATE first (read-only):
```sh
ssh <broker-host> 'sudo systemctl cat resource-manager; echo ---; sudo cat /usr/local/bin/resource-manager.sh' | less
```
   Compare against `legacy/resource-manager-v3.sh` in the repo. Questions to answer in writing: Does it kill processes or throttle `ollama.service` (CPU/memory caps) in ways the Broker does NOT replicate? Does it write state files (`/tmp/resource-manager-state`) that anything else reads? Do any cron jobs or legacy batch wrappers (`batch-job-wrapper-v*.sh`) depend on it?
2. BRANCH: if it performs actions the Broker does not cover (the legacy Tier-1 design throttled Ollama to 3GB/1core during gaming — check whether the live script still does) → write a gap ADR: port the behavior, or accept its loss, BEFORE retiring. If it is redundant → proceed.
3. RETIRE (confirm with Preston; not on a Thursday night — the Friday window follows): `sudo systemctl stop resource-manager` (leave ENABLED so a reboot restores it — that is the first-stage rollback), then soak.
4. SOAK (write exit criteria first — see `broker-validation-and-qa` §6): 7 days including ≥1 gaming session, ≥1 Plex transcode, one Friday scraper window; broker-only arbitration must show clean `yield start/stop` pairs, no game stutter reported by the humans, deferred/failed metrics unremarkable.
5. On clean soak: `sudo systemctl disable resource-manager` (keep the unit file and script on disk for rollback); record the retirement in `broker-failure-archaeology` entry 9 (status open→fixed). Rollback at any stage: `sudo systemctl start resource-manager` (and re-enable if disabled).

## P5 — Close the loop (repo)

- Update DESIGN.md's cutover language and README's "run alongside V3 first" paragraph to reflect the completed cutover.
- Raise the `v2-go`→`main` merge with Preston (branch policy UNKNOWN — assumption; `main` is 3 commits, stale since the v2 import). Do not merge without his say-so.
- Update the dated claims in this skill and in `broker-failure-archaeology`/`broker-architecture-contract` known-weak lists.

## Fenced wrong paths (do NOT)

- Do NOT stop `resource-manager.service` without P4's investigation — it may still be load-bearing (Ollama throttling) and it survived the v2 deploy for a reason nobody wrote down.
- Do NOT bind Ollama to loopback as a quick ":11434 exposure" fix — consumer inventory first; it is `ollama.service` config (different unit, different blast radius), and the broker itself reaches Ollama via 127.0.0.1 so the change is tempting but must be its own gated change with its own soak.
- Do NOT change the `BROKER_BATCH_WAIT` code default silently (P2.3) — the 300s value is an operational decision recorded in the unit comment; moving it into code is an ADR-worthy default change.
- Do NOT set `BROKER_MAX_INFLIGHT=0` to "drain" — config rejects values < 1 and the broker will not start.
- Do NOT run any experiment that weakens yield on the shared desktop "just to test" — use unit tests with fake Admissions (`broker-validation-and-qa`).
- Do NOT put a control token or any credential in this repo or any skill file.

## Success criteria (measurable, whole campaign)

1. `go test ./... && go vet ./... && go test -race ./...` green at HEAD.
2. README/deploy/config consistency check (P2.5) emits nothing.
3. From a non-loopback host without token: `POST /control` → 401/403; `GET /metrics` still 200 (Grafana unaffected).
4. `systemctl is-enabled resource-manager` → `disabled`, after a documented 7-day soak with written exit criteria met.
5. Detection-to-yield latency observed ≤ 2× `BROKER_DETECT_INTERVAL` in journalctl during the soak's gaming session.
6. DESIGN.md/README no longer describe the cutover as pending.

## When NOT to use this skill

- Routine deploys/restarts/Job operations → `broker-run-and-operate`.
- A live symptom right now → `broker-debugging-playbook`.
- The graded-yield research work → `broker-graded-yield-frontier` (do not mix hardening and research in one change).

## Provenance and maintenance

Baseline verified 2026-07-02 (repo at `ad07905`; live desktop read-only). This skill's claims go stale fastest of any in the library. Re-verify before each phase:

```sh
cd /Users/prestonbernstein/dev/resource-broker && go test ./... 2>&1 | tail -3
grep -rn BROKER_CONTROL_TOKEN /Users/prestonbernstein/dev/resource-broker/internal /Users/prestonbernstein/dev/resource-broker/cmd
grep -n TDARR /Users/prestonbernstein/dev/resource-broker/deploy/broker.service
ssh <broker-host> 'systemctl is-active resource-broker resource-manager'
```
