# Requirements: resource-broker-canonical-logging

## Problem statement
The Broker already writes one JSON object per line to stdout. Each line has `ts`, `level`, `msg`, `schema_version`, `service` and `req_id`. That shape does not meet the fleet logging contract (CONVENTIONS section 18 and the stack pack "logging"). Lines have no stable event name and no per-process run identifier. Request lines use `served` as an outcome, which the contract does not allow. Operators who search logs across the fleet cannot filter Broker events by name or group them by process run. Failed lines carry no error type or message in a standard place. This change brings every Broker log line into the canonical shape so the fleet tooling can check and query it.

## Users / stakeholders
- Preston, who runs and maintains the Broker on a home PC.
- Operators and tools that read fleet logs, including Loki queries and dashboards.
- Maintainers of the stack pack "logging" and its checker, `logging-check.py`.
- Consumers of the Broker, whose request behavior and metrics must not change.
- Contributors who add new log lines to the Broker.

## Functional requirements
R1. The system shall write every log line as one JSON object on one line to stdout.
R2. The system shall include these fields on every log line: `ts`, `level`, `msg`, `schema_version`, `service`, `event`, `run_id`.
R3. The system shall set `event` to a dot-namespaced, stable name on every log line, such as `request.served`, `request.rejected` or `broker.started`.
R4. The system shall keep each `event` value fixed for a given kind of occurrence, so that changing `msg` wording never changes `event`.
R5. The system shall keep `msg` as human-readable text on every log line.
R6. The system shall generate one `run_id` value at process start.
R7. The system shall put that same `run_id` value on every log line the process writes until it exits.
R8. The system shall generate a new `run_id` each time the process starts.
R9. The system shall keep `req_id` as the per-request field on log lines that belong to a request.
R10. The system shall set `outcome` to one of `ok`, `partial` or `failed` on every request log line.
R11. The system shall no longer write `served` as an `outcome` value.
R12. The system shall include `err_type` and `err_msg` on every log line whose `outcome` is `partial` or `failed`.
R13. The system shall set `level` to one of `debug`, `info`, `warn`, `error` or `critical` on every log line.
R14. The system shall log a `broker.started` event once at process start.
R15. The system shall log a `request.served` event for a request the Broker admits and completes.
R16. The system shall log a `request.rejected` event for a request the Broker turns away, such as a `503` during Yield.
R17. The system shall keep the `service` value the same as it is today.
R18. The system shall keep the `schema_version` field on every log line.
R19. The system shall not put any secret, token or request body in any log field. This covers `UPSTREAM_API_KEY`, `BROKER_ROUTE_<N>_API_KEY` and `PLEX_TOKEN`.
R20. The system shall not emit `run_id` or `req_id` as a Loki label.

## Non-functional requirements
- Log output stays one JSON object per line, with no multi-line records.
- No new field carries a secret, token or request body. This holds for error messages in `err_msg` too.
- Logging changes add no new metrics and change no existing metric name, label or value.
- The Broker adds no new runtime dependency to write the new fields. The checker runs only in tests.

## Constraints
- Follow the fleet logging contract: CONVENTIONS section 18 and the stack pack "logging". The pack's rules win where this document is silent.
- The Broker stays a Go program and keeps its systemd unit name, `resource-broker.service`.
- Output stays on stdout.
- Request handling, admission, Yield, Preemption, queueing and Job behavior must not change.
- Existing Prometheus metrics at `/metrics` must not change.
- The checker is `logging-check.py`, run with `--lines`. Its exit code is the pass/fail signal.

## Out of scope
- Changing metrics, `/status`, `/healthz` or any HTTP response.
- Changing request handling, priority, Yield or Job behavior.
- Moving logs to another destination or changing the log transport.
- Changing Loki label configuration or log shipping setup.
- Renaming the binary or the systemd unit.
- Logging request or response bodies for debugging.
- Changing the `outcome` values on HTTP response headers or stream trailers. This work covers log lines only.

## Acceptance criteria
1. A Go test captures real log output from the Broker handlers and pipes it to `logging-check.py --lines`. The checker exits with code 0.
2. The captured output in that test includes at least one `request.served` line and one `request.rejected` line.
3. Every captured line parses as a single JSON object and contains `ts`, `level`, `msg`, `schema_version`, `service`, `event` and `run_id`.
4. Every captured line has a non-empty `event` that contains a dot.
5. All captured lines in one test run carry the same `run_id`. A second process start in a test yields a different `run_id`.
6. Every captured line has `level` set to `debug`, `info`, `warn`, `error` or `critical`.
7. No captured request line has `outcome` set to `served`. Each has `ok`, `partial` or `failed`.
8. Every captured line with `outcome` `partial` or `failed` has non-empty `err_type` and `err_msg`.
9. A test starts the Broker with known values for `UPSTREAM_API_KEY` and `PLEX_TOKEN`, sends a request with a known body, and finds none of those values anywhere in the captured output.
10. The Broker writes a `broker.started` line at startup.
11. The existing test suite passes unchanged, including tests that cover metrics.
12. A test compares `/metrics` output before and after the change for the same request sequence. Metric names and labels match.
13. No log label configuration in the repo lists `run_id` or `req_id` as a label.
