# Agent rules for resource-broker

Run the gates before you say a task is done. CI and `.githooks/pre-commit` run the same commands.
Enable the hook once per clone: `git config core.hooksPath .githooks`. Never commit with `--no-verify`.
Install the gate tools with `bash ci/install-gate-tools.sh <dir>`, then put `<dir>/bin`, `<dir>/venv/bin` and `ci/node_modules/.bin` on PATH.

## Core rules

- The core gate is `bash scripts/core-validate.sh` (file size, names, complexity, function length, parameters, nesting, duplication).
- Reuse before you write. Extend an existing function instead of adding a near copy.
- Put variants in data and generic code. Extract on the second real occurrence. No boolean flag arguments.
- Group code by the domain it serves. Dependencies point from the edge to the domain only.
- A function does one thing. Use guard clauses. Keep it under 50 lines, under 10 branches, at most 4 parameters.
- No name is just `data`, `info`, `tmp`, `handle` or `process`.
- Fail loudly. Comments say why, not what. Delete dead code.
- A behavior change ships with a test that fails without it.
- Never edit a limit or a config to make a run pass. Fix the code. A waiver goes in `.core-allow` with a reason.
- There is no `.core-baseline.json`. Do not add one. `.core-allow` is for generated or vendored files only.
- `scripts/` holds files copied unchanged from the stack packs. Do not edit them. Update them by copying the new pack version.

## Go rules

- The Go gate is `bash scripts/go-validate.sh` (go mod verify, gofmt, go vet, golangci-lint, go test -race).
- Wrap errors with `fmt.Errorf("doing x: %w", err)`. Compare with `errors.Is` and `errors.As`.
- `ctx context.Context` is the first parameter. Use `http.NewRequestWithContext`.
- Every goroutine has a way to stop and something waits for it. No `init()` functions.
- Log with `log/slog` and key-value pairs. No `fmt.Print*` or `log.Print*` in library code.
- `//nolint` names the linter and gives a reason.
- `.golangci.yml` has no baseline: every finding fails the gate. A line-level `//nolint:<linter> // <reason>` is the only exception.

## GitHub Actions rules

- The workflow gate is `bash scripts/gha-validate.sh` (workflow-check.py, actionlint, zizmor).
- Pin every `uses:` to a full 40-character SHA with the version in a trailing comment. Dependabot moves the pins.
- Set `permissions: contents: read` at the top. Raise it per job only where needed.
- Pass `${{ github.event... }}` and `${{ inputs.x }}` through `env:`, never inside `run:`.
- No `pull_request_target`. `actions/checkout` sets `persist-credentials: false`. Every job sets `timeout-minutes`.
- `.github/zizmor.yml` lists the allowed actions. Add one only with human review.

## Shell rules

- The shell gate is `bash scripts/shell-validate.sh` (bash -n, shell-check.py, shellcheck, shfmt, bats).
- Start with `#!/usr/bin/env bash` and `set -euo pipefail`. Quote every expansion.
- Errors go to stderr. Check tools with `command -v`. Pair every `mktemp` with a `trap` cleanup.
- Never `curl | sh`. Pip installs use `--require-hashes -r <lockfile>`.
- `.shell-allow` lists vendored paths with a reason.

## Writing rules

- The writing gate is `bash scripts/writing-validate.sh`. It lints the files that changed against main.
- Use short sentences, plain words and American spelling. Put the point first. No AI filler.
- A code comment says why. Three sentences at most.
- `.writing-allow` lists vendored paths with a reason.

## Logging rules

- The logging check is `python3 scripts/logging-check.py .`.
- The JSON logger is `newLogger` in `cmd/broker/main.go`. It sets `schema_version`, `ts`, `level` and `service`.
- Known gap: the `slog` call sites do not set `event` or `run_id` yet. Do not add log lines without them.
- Never log tokens, passwords, request or response bodies, or email addresses.
