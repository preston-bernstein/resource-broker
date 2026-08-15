---
name: broker-build-and-env
description: >
  Recreate the resource-broker development environment from scratch and
  build/test/vet the project. Load this skill when you need to: set up Go for
  this repo, run `make build` / `make test` / `go test ./...`, cross-compile
  the Linux deploy binary from macOS, understand why `go test ./...` or
  `go vet ./...` fails in internal/admin, explain why the build is CGO-free /
  static, debug local-run surprises (yield never triggers on macOS,
  broker-jobs.db appearing in cwd, port bind failures on 11435-11438), or run
  the Broker locally without an Ollama upstream. Keywords: build, compile,
  cross-compile, GOOS=linux, CGO_ENABLED=0, Makefile, go vet, test failure,
  Mux, admin_test, modernc.org/sqlite, local dev, /proc, Detect fail-open.
---

# Broker build and development environment

How to go from a bare machine to a built, tested `resource-broker` binary, and
the traps that bite people doing local development on this repo.

Repo: `/Users/prestonbernstein/dev/resource-broker`, branch `v2-go`
(all v2 work lives here; `main` is stale). All facts below verified against
HEAD `ad07905` on 2026-07-02 unless labeled otherwise.

## When NOT to use this skill

- Deploying to the desktop, systemd unit anatomy, control-plane operations,
  Job API usage → **broker-run-and-operate**
- Writing or extending tests, race/vet discipline as an evidence standard,
  soak criteria → **broker-validation-and-qa**
- What env vars mean and their live overrides → **broker-config-and-flags**
- Whether a change is allowed at all → **broker-change-control**

## 1. Prerequisites

| Requirement | Detail |
|---|---|
| Go | **>= 1.24.0** — `go.mod` declares `go 1.24.0`. Older toolchains refuse to build. |
| C compiler | **None. Ever.** See below. |
| Anything else | No. No Docker, no protoc, no code generation, no Node. `git clone` + Go is the whole environment. |

Check: `go version` — verified working with `go1.24.0 darwin/arm64` on 2026-07-02.

### No CGO — this is a load-bearing constraint, not an accident

The only non-stdlib dependencies (`go.mod`) are `github.com/google/uuid` and
`modernc.org/sqlite` (+ its transitive pure-Go deps). `modernc.org/sqlite` is
a **pure-Go** SQLite driver, which is exactly why:

- `make build` sets `CGO_ENABLED=0` and produces a fully **static** binary;
- a macOS dev can cross-compile the Linux deploy artifact with no cross-C-toolchain;
- the deployed binary has zero shared-library requirements on the desktop.

**Never introduce a dependency that requires CGO** (e.g. swapping in
`mattn/go-sqlite3`). That silently breaks static builds and cross-compilation.
Treat it as a change-control violation — see **broker-change-control**.

## 2. Clone, build, test

```sh
git clone https://github.com/preston-bernstein/resource-broker.git
cd resource-broker
git checkout v2-go
make build
```

### Make targets (the entire Makefile, exactly)

| Target | Runs | Notes |
|---|---|---|
| `make build` | `CGO_ENABLED=0 go build -o bin/resource-broker ./cmd/broker` | Normal build path. Output: `bin/resource-broker` (gitignored). |
| `make test` | `go test ./...` | **FAILS at HEAD** — see known break below. |
| `make race` | `go test -race ./...` | Same known break applies (race builds the same test files). |
| `make vet` | `go vet ./...` | **FAILS at HEAD** — same root cause. |
| `make fmt` | `gofmt -l .` | Lists unformatted files; empty output = clean. Clean at HEAD (verified 2026-07-02). |
| `make clean` | `rm -rf bin` | Removes build output only. |

Direct `go` equivalents work identically; the Makefile adds nothing but
`CGO_ENABLED=0` and the output path. If you must build without touching the
repo tree (e.g. sandboxed agent sessions), build to a temp dir instead of
running `make build`:

```sh
go build -o /tmp/resource-broker ./cmd/broker
```

### Cross-compiling the deploy artifact (macOS -> Linux desktop)

This is how the desktop binary is produced from the Mac. Verified working
2026-07-02; `file` confirms `ELF 64-bit LSB executable, x86-64 ... statically linked`:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/resource-broker-linux ./cmd/broker
```

No extra toolchain needed — this works **because** the module is CGO-free
(section 1). Installation of the artifact is **broker-run-and-operate**'s job.

### Expected test/vet output as of 2026-07-02 (HEAD ad07905)

`go test ./...` result matrix:

| Package | Result |
|---|---|
| `cmd/broker` | no test files |
| `internal/admin` | **FAIL [build failed]** — known break, see below |
| `internal/config` | ok |
| `internal/detect` | ok |
| `internal/job` | ok |
| `internal/metrics` | ok |
| `internal/ollama` | ok |
| `internal/proxy` | ok |
| `internal/queue` | ok |
| `internal/schedule` | no test files (known gap) |
| `internal/tdarr` | no test files (known gap) |
| `internal/yield` | ok |

### KNOWN BREAK: internal/admin (do not be surprised, do not casually "fix")

As of 2026-07-02, `go test ./...` and `go vet ./...` both fail with exactly:

```
internal/admin/admin_test.go:31:11: not enough arguments in call to Mux
	have (Controller, fakeStats, http.HandlerFunc, nil, nil)
	want (Controller, StatsProvider, http.Handler, http.Handler, func() any, TdarrStatusFn)
```

(`go vet` reports the same at column 14.) Cause: the Tdarr integration commit
`dd39d20` (2026-06-29) added a `TdarrStatusFn` parameter to `admin.Mux` without
updating `admin_test.go`. This is a compile error in the test file only — the
production build (`make build`, cross-compile) is unaffected and succeeds.

The fix is a scheduled phase of **broker-cutover-hardening-campaign** (the
"fix broken test at HEAD" phase). If you see this exact error, it is the known
break; if you see a *different* error, that is new damage — stop and triage.

## 3. Platform traps

### Trap: yield never triggers in local dev on macOS/Windows

Process detection (`internal/detect`) reads `/proc/<pid>/comm` and
`/proc/<pid>/cmdline`. `ProcLister` is Linux-only; on any platform without
procfs, `os.ReadDir("/proc")` fails and the lister returns no processes.
`Detector.Detect` treats a listing error as `("", false)` — **fail-open by
design**: never block inference because `/proc` was unreadable.

Consequence: on macOS the Broker always reports no Contention, so **Yield can
never trigger locally**. You cannot integration-test yield behavior on a Mac.
The supported way to exercise it is unit tests with a fake `Lister`/detector:

- `internal/detect/detect_test.go` — `lister(cmds ...string) Lister` fabricates
  process lists; `TestDetectListerErrorFailsOpen` pins the fail-open contract.
- `internal/yield/yield_test.go` — `fakeDet` (settable reason/contention) and
  `fakeUnloader` drive the yield controller without any real processes.

Follow those patterns when adding tests (details in **broker-validation-and-qa**).
Real yield behavior can only be observed on the Linux desktop
(**broker-run-and-operate** / **broker-diagnostics-and-tooling**).

### Trap: SQLite files appear in your cwd

`BROKER_JOB_DB` defaults to the relative path `broker-jobs.db`, so a local run
creates `broker-jobs.db` (plus `-wal`/`-shm` WAL sidecars while running) in
whatever directory you launched from. All three patterns are gitignored
(`*.db`, `*.db-wal`, `*.db-shm`), so they will not dirty `git status`, but
stale local Job state persists across runs — delete the files for a clean slate.

### Trap: port expectations

The Broker listens on `:11435` (interactive), `:11436` (batch), `:11437`
(control plane), `:11438` (embed lane, only when `INFINITY_URL` is set).
Raw Ollama's own port `11434` is the *upstream* — the Broker never binds it, so
a locally running Ollama.app does not conflict. What does conflict: a second
Broker instance, or anything else squatting on 11435-11438 — you get a bind
error at startup. On the desktop itself the live `resource-broker.service`
already holds all four ports (dated live fact, 2026-07-02).

## 4. Running locally

Minimal invocation (from README, verified 2026-07-02):

```sh
OLLAMA_URL=http://127.0.0.1:11434 ./bin/resource-broker
```

Behavior with **no Ollama running at all** (verified by live smoke run):

- Starts fine. Logs structured JSON: `listening` for `:11435`/`:11436`/`:11437`,
  then `broker up` with the upstream URL.
- Control plane on `:11437` immediately serves `/healthz` (returns `ok`),
  `/status` (JSON snapshot: jobs, queue, schedule, yield), and `/metrics`
  (Prometheus). Also `/control` and `/jobs`.
- The upstream is only contacted when a request is actually proxied; without
  Ollama you get `502` and a `WARN upstream error ... connection refused` log line.
- Embed lane `:11438` is absent unless `INFINITY_URL` is set (you will see an
  `embed lane enabled` log line when it is).

Test curls:

```sh
curl -s http://127.0.0.1:11437/healthz          # -> ok
curl -s http://127.0.0.1:11437/status | jq .    # queue/jobs/yield snapshot
curl -s http://127.0.0.1:11435/api/tags         # proxied to Ollama (502 if none running)
```

Full endpoint/operations reference: **broker-run-and-operate**. Env var
meanings and live overrides: **broker-config-and-flags**.

## 5. There is no CI (as of 2026-07-02)

No `.github/` directory exists — no Actions, no automated checks on push. The
Makefile targets **are** the CI. Discipline, enforced by convention only:

```sh
go test ./... && go vet ./... && gofmt -l .
```

before every commit. The internal/admin break at HEAD exists precisely because
commit `dd39d20` skipped this (cross-package signature change, tests not run).
See **broker-change-control** for the full pre-commit gate.

## 6. Repo hygiene

`.gitignore` covers, relevantly:

- `bin/` — `make build` output; never commit binaries.
- `*.db`, `*.db-wal`, `*.db-shm` — local durable Job store sidecars (section 3).
- `docs/BUILD-PLAN.md` — a local-only planning file that may exist untracked
  on a dev machine; its absence in a fresh clone is normal.
- `*.log`, `*.job`, `queue/`, `/tmp/`, `*.zip`, `.DS_Store` — legacy/scratch noise.

A clean dev loop (`build`, local run, `test`) should leave `git status` clean.
If it does not, you created something outside these patterns — investigate
before committing.

## Provenance and maintenance

- Go requirement: `grep '^go ' go.mod` (currently `go 1.24.0`).
- CGO-free claim: `grep -c cgo go.sum || true` and confirm `modernc.org/sqlite`
  (not `mattn/go-sqlite3`) in `go.mod`; `file` on a linux build must say
  `statically linked`.
- Make targets: `cat Makefile` (22 lines; if longer, this skill is stale).
- Known internal/admin break: `go vet ./...` — if it passes, the
  broker-cutover-hardening-campaign fix phase landed; update section 2.
- Test matrix: `go test ./...` and diff against the table above.
- No-CI claim: `ls -d .github 2>/dev/null` — any output invalidates section 5.
- Ignore rules: `cat .gitignore`.
- Fail-open detection: `grep -n 'fail open' internal/detect/detect.go`.
