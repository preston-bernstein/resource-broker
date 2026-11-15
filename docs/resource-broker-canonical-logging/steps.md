# Steps: resource-broker-canonical-logging

## Prerequisites
- The resource-broker repo builds clean and its existing Go test suite passes.
- The stack pack "logging" checker, `logging-check.py`, is on the dev machine. Its path is known. Step 5 reads it from an environment variable.

## Implementation steps

### Step 1: Capture the pre-change metrics baseline
**What**: Record the metric names and labels from `/metrics` for a fixed request sequence against the unmodified code, save them as a golden file, and add a Go test that compares live output to it.
**Files**: testdata/metrics.golden (new), a new golden-comparison test in the package that serves /metrics (find it by searching for the /metrics handler)
**Test**: Run the new test with go test against the unchanged code. It passes, and testdata/metrics.golden exists.
**Covers**: none
**Depends on**: none
**Parallelizable**: Yes

### Step 2: Rework the logger core
**What**: In the one place that builds log lines, generate one run_id (16 hex chars from crypto/rand) at logger init, stamp it on every line, add a canonical log function that takes event as a required argument, validate level against the allowed set, support the outcome, err_type and err_msg fields, keep ts, msg, schema_version, service and req_id behavior unchanged, add one file of dot-namespaced event constants, and keep the old function as a thin shim so the build stays green.
**Files**: the logger source file (find it with grep -rn "schema_version" --include="*.go"), a new events.go in the same package, the logger's test file
**Test**: go test on the logger package passes. New unit tests show: run_id on every line and identical across lines, a fresh logger gets a different run_id, a level outside debug/info/warn/error/critical is rejected, every event constant contains a dot, and the service value is unchanged. go build ./... still passes with the shim.
**Covers**: R1, R2, R3, R4, R5, R6, R7, R8, R9, R13, R17, R18
**Depends on**: 1
**Parallelizable**: No

### Step 3: Give request handler lines events and outcomes
**What**: In the interactive, batch and embed lane handlers, log request.served (info, outcome ok) for admitted, completed requests, request.rejected (warn, outcome failed) for turned-away requests such as the 503 during Yield, and outcome partial for a stream cut after headers, pass fixed err_type and err_msg texts (yield, queue_full, wait_timeout, preempted, upstream) on every non-ok line, drop the served outcome value, and log only method, path without query string, class, status and wait time.
**Files**: the Fronting Proxy handler files for the interactive, batch and embed lanes, their test files
**Test**: go test on the handler package passes. Captured output shows a request.served line with outcome ok, and a request.rejected line with outcome failed plus non-empty err_type and err_msg. No line has outcome served. Logged paths carry no query string.
**Covers**: R9, R10, R11, R12, R15, R16, R19
**Depends on**: 2
**Parallelizable**: No

### Step 4: Name all remaining log calls and log broker.started
**What**: In cmd/, build the logger first and log broker.started once after config loads, give every remaining log call in internal/yield/yield.go and the job and queue code a constant event name with fixed err_type and err_msg where a call reports a failure, then delete the shim so event is required everywhere.
**Files**: the main package under cmd/, internal/yield/yield.go, the job and queue source files that log, the logger file (shim removal)
**Test**: go build ./... passes with the shim gone, so a call without an event is now a compile error. A startup test runs the broker and sees a broker.started line. go test ./... passes.
**Covers**: R14
**Depends on**: 2, 3
**Parallelizable**: No

### Step 5: Acceptance test through logging-check.py
**What**: Add a Go test that captures real stdout from broker startup and from served and rejected requests against a fake upstream, pipes the lines to logging-check.py --lines, and asserts the full contract, including one leak case with known secret and body values.
**Files**: canonical_logging_test.go next to the logger
**Test**: go test runs the new test. The checker exits 0. Asserts pass: at least one request.served and one request.rejected line; every line is one JSON object with ts, level, msg, schema_version, service, event and run_id; every event is non-empty and dotted; one shared run_id per start and a different run_id on a second start; levels in the allowed set; no served outcome; non-empty err_type and err_msg on partial or failed lines; a broker.started line; and no occurrence of known UPSTREAM_API_KEY, BROKER_ROUTE_1_API_KEY, PLEX_TOKEN or request body values. The checker path comes from an environment variable, and the test fails with a clear message if it is missing. go test ./... passes, including the step 1 golden test and the existing metrics tests.
**Covers**: R1, R2, R3, R6, R7, R8, R10, R11, R12, R13, R14, R15, R16, R18, R19
**Depends on**: 3, 4
**Parallelizable**: No

### Step 6: Guard the Loki label config
**What**: Add a Go test that walks the log shipping config under deploy/ and fails if any Loki label list contains run_id or req_id.
**Files**: deploy_labels_test.go next to the logger test
**Test**: go test runs the scan. It passes on the current repo and fails against a fixture that adds run_id as a label.
**Covers**: R20
**Depends on**: none
**Parallelizable**: Yes

## Rollback plan
All steps reversible via git.
