# Steps: resource-broker-canonical-logging

## Prerequisites
- The resource-broker repo builds clean and its existing Go test suite passes.
- The stack pack "logging" checker, `logging-check.py`, is on the dev machine. Tests read its path from the LOGGING_CHECK environment variable.
- CONVENTIONS.md and the logging pack are readable on the dev machine.

## Implementation steps

### Step 1: Confirm the logging seam and the outcome rule
**What**: Confirm there is a single log emission point and record its construction model. State the injected-logger decision. Read CONVENTIONS §18 and the logging pack. Settle whether outcome is optional on non-request lines. Add a test that forces the preempted-stream path and records whether it emits a log today.
**Files**: cmd/broker/main.go, internal/proxy/proxy.go
**Test**: A short design note names the emission point, the construction model, the injected-logger decision, and the outcome rule. The forced-preemption test runs and its result is in the note.
**Covers**: none
**Depends on**: none
**Parallelizable**: No

### Step 2: Capture the pre-change metrics baseline
**What**: First run a fixed request sequence against the unmodified code and capture /metrics. Record metric names and label keys only. Save the stable owned subset as a golden file, excluding runtime metrics such as go_* and process_*. Then add a Go test that compares live output to the golden file.
**Files**: internal/metrics/testdata/metrics.golden, internal/metrics/metrics_golden_test.go
**Test**: Run the new test with go test against the unchanged code. It passes, and testdata/metrics.golden exists. The golden file excludes runtime metrics.
**Covers**: none
**Depends on**: none
**Parallelizable**: Yes

### Step 3: Rework the logger core
**What**: Build the seam as an slog.Handler wrapper, not a custom function. Generate one run_id of 16 hex chars from crypto/rand in a constructor, never in a package-level var or init(). Stamp run_id on every line. Use ReplaceAttr for ts and lowercase level names. Add a canonical log helper that takes event as a required argument. Validate level against the allowed set. Support outcome, err_type and err_msg. Keep ts, msg, service and req_id behavior unchanged. Keep emitting schema_version and bump its value, because the line shape changes. Add one file of dot-namespaced event constants. Keep the old function as a thin shim so the build stays green.
**Files**: cmd/broker/main.go, cmd/broker/events.go, cmd/broker/logging_test.go
**Test**: go test on the logger package passes. New unit tests show: run_id on every line and identical across lines, a fresh logger gets a different run_id, a level outside debug/info/warn/error/critical is rejected, every event constant contains a dot, and the service value is unchanged. go build ./... still passes with the shim.
**Covers**: R1, R2, R3, R4, R5, R6, R7, R8, R9, R13, R17, R18
**Depends on**: 1
**Parallelizable**: No
**Rollback**: Revert the logger changes and events.go. The shim keeps old call sites building.

### Step 4: Give request handler lines events and outcomes
**What**: In the interactive, batch and embed lane handlers, log request.served (info, outcome ok) for admitted, completed requests. Log request.rejected (warn, outcome failed) for turned-away requests such as the 503 during Yield. Log outcome partial for a stream cut after headers. Pass fixed err_type and err_msg texts (yield, queue_full, wait_timeout, preempted, upstream) on every non-ok line. Drop the served outcome value. Log only method, path without query string, class, status and wait time. Use slog.InfoContext and slog.WarnContext so req_id from context survives.
**Files**: internal/proxy/proxy.go, internal/openaicompat/handler.go, internal/proxy/proxy_test.go
**Test**: go test on the handler package passes. Captured output shows a request.served line with outcome ok and req_id present. It also shows a request.rejected line with outcome failed, req_id, and non-empty err_type and err_msg. No line has outcome served. Logged paths carry no query string. One test asserts the literal event strings on a response at header time. Another asserts them on a stream at trailer time. Renaming the shared served constant then breaks a test.
**Covers**: R9, R10, R11, R12, R15, R16, R19
**Depends on**: 3
**Parallelizable**: No
**Rollback**: Revert the handler edits.

### Step 5: Update legacy tests that assert the old log shape
**What**: Find the legacy tests that assert the old log shape. Update them to expect event, outcome and run_id fields. Keep every assertion at least as strict as before.
**Files**: internal/proxy/proxy_test.go, internal/yield/yield_test.go, internal/yield/displace_test.go, internal/job/worker_test.go
**Test**: go test ./... passes with the updated expectations. No assertion on the new fields was removed.
**Covers**: none
**Depends on**: 4
**Parallelizable**: No

### Step 6: Name all remaining log calls and log broker.started
**What**: In cmd/, extract run(cfg, out io.Writer) so tests capture stdout. Build the logger first and log broker.started once after config loads. Give every remaining log call a constant event name. Cover internal/yield/yield.go, the job and queue code, displace.go (triggerDisplace), the idle path (checkIdleLocked), and GPUManager pause/resume. Add fixed err_type and err_msg where a call reports a failure. Remove the raw err field from every call, including yield doUnload/doReload and the Plex detector. Keep the shim for now.
**Files**: cmd/broker/main.go, internal/yield/yield.go, internal/yield/displace.go, internal/job/service.go, internal/job/worker.go, internal/queue/gate.go, internal/queue/park.go, internal/detect/detect.go, internal/httpx/httpx.go, internal/tdarr/tdarr.go
**Test**: go test ./... passes. A startup test runs the broker through run() and sees a broker.started line on captured stdout. Captured failure lines carry fixed err_type and err_msg and no raw err field.
**Covers**: R14
**Depends on**: 3, 4, 5
**Parallelizable**: No
**Rollback**: Revert the call sites. The shim still accepts the old calls, so the build stays green.

### Step 7: Delete the shim
**What**: Delete the old-function shim from the logger. Add a lint gate that rejects raw slog.* calls outside the wrapper.
**Files**: cmd/broker/main.go, cmd/broker/logging_test.go
**Test**: go build ./... passes with the shim gone, so a call without an event is now a compile error. The gate fails on a raw slog.Info call and passes on the current tree. go test ./... passes.
**Covers**: R14
**Depends on**: 6
**Parallelizable**: No
**Rollback**: Re-add the shim first. Then revert call sites in reverse order.

### Step 8: Pin logging-check.py and wire it into CI
**What**: Pin the logging-check.py version. Document its path and the LOGGING_CHECK environment variable in the repo. Add LOGGING_CHECK to CI. Add a make target that runs the checker tests, and make CI always run it.
**Files**: Makefile, docs/runbooks/logging.md
**Test**: CI runs the new target. The target fails with a clear message when LOGGING_CHECK is unset.
**Covers**: none
**Depends on**: none
**Parallelizable**: Yes
**Rollback**: Revert the CI config and Makefile edits.

### Step 9: Acceptance contract test through logging-check.py
**What**: Add a Go test that starts the broker against a fake upstream. Capture real stdout from startup and from served, rejected and preempted requests. Force one partial line with err_type=preempted. Emit non-request lines too: yield start and clear, an unload/reload failure, job and queue lines, and a displace line. Pipe all lines to logging-check.py --lines. Call the checker with exec.Command argv, never a shell, with python -I, and resolve its path inside a trusted directory. Keep the shell-out in test code only. Gate the shell-out test behind the make target from Step 8. Add a fast in-process schema test that always runs under plain go test.
**Files**: cmd/broker/canonical_logging_test.go
**Test**: The make target runs the new test. The checker exits 0. Captured output holds at least one request.served, one request.rejected, and one partial line with err_type=preempted. A broker.started line is present. Yield, unload-failure, reload-failure, job, queue and displace lines are present. Every line is one JSON object with ts, level, msg, schema_version, service, event and run_id. Every event is non-empty and dotted. One run_id is shared per start, and a second start gets a different run_id. Levels stay in the allowed set. No line has outcome served. Partial and failed lines carry non-empty err_type and err_msg. Logged paths carry no query string. The test fails with a clear message if LOGGING_CHECK is missing. The in-process schema test runs under plain go test. go test ./... passes, including the Step 2 golden test and the existing metrics tests.
**Covers**: R1, R2, R3, R6, R7, R8, R10, R11, R12, R13, R14, R15, R16, R18, R19
**Depends on**: 2, 4, 7, 8
**Parallelizable**: No
**Rollback**: Delete the test file.

### Step 10: Secret and redaction test
**What**: Add a Go test that captures broker stdout and scans it for known secret values. Drive leak cases for secrets in a query string and in an Authorization header. Drive leak cases for secrets in an upstream 5xx body, a preempted stream, and a Job failure. Scan for UPSTREAM_API_KEY, BROKER_ROUTE_1_API_KEY, PLEX_TOKEN and request body values.
**Files**: cmd/broker/redaction_test.go
**Test**: go test runs the test. Each leak case emits at least one line, so an empty capture fails. No captured line contains any known secret or body value.
**Covers**: R15, R16
**Depends on**: 9
**Parallelizable**: No
**Rollback**: Delete the test file.

### Step 11: Guard the Loki label config
**What**: Walk the whole repo for log shipping config: Loki, promtail and Alloy files. Fail if any label list contains run_id or req_id. Also fail if a pipeline stage, such as Alloy River stage.labels, promotes run_id or req_id to a label. Fail when the scan finds zero config files. Keep a bad fixture under testdata/. Document that host-level config is outside this test's coverage.
**Files**: cmd/broker/deploy_labels_test.go, cmd/broker/testdata/loki_labels.yml
**Test**: go test runs the scan. It passes on the current repo. It fails against the testdata fixture that adds run_id as a label. It fails when the scan finds zero files.
**Covers**: R20
**Depends on**: none
**Parallelizable**: Yes

## Rollback plan
All steps reversible via git. Risky steps carry their own rollback notes. For Step 7 the order is fixed: re-add the shim first, then revert call sites in reverse order.
