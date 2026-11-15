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
R3. The system shall set `event` to a dot-namespaced, stable name on every log line, such as `request.served`, `request.rejected`, `request.failed` or `broker.started`.
R4. The system shall define every `event` value as a constant in one source file. A test shall pass only when every emitted `event` matches one of those constants.
R5. The system shall set `msg` to a non-empty string on every log line.
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
R16. The system shall log `request.rejected` for each request it turns away: missing or wrong API key, or `503` during Yield.
R17. The system shall set `service` to `resource-broker` on every log line.
R18. The system shall keep the `schema_version` field on every log line.
R19. The system shall not put any secret, token, request or response body, or header value in any log field. This ban holds at every level, including `debug`. It covers `UPSTREAM_API_KEY`, `BROKER_ROUTE_<N>_API_KEY`, `PLEX_TOKEN` and the `Authorization` header.
R20. The system shall not emit `run_id` or `req_id` as a Loki label.
R21. The system shall log the request path without its query string.
R22. The system shall count only lines a request handler emits for one request as request log lines.
R23. The system shall log only request metadata on request lines: method, path, class, status and wait time. These fields carry the same facts today's request lines already carry in `msg`.
R24. The system shall JSON-encode and length-cap method, path and class, preferring route template or class over raw path.
R25. The system shall omit `outcome` from non-request log lines.
R26. The system shall set `err_type` to exactly one of `auth_failed`, `preempted`, `client_gone`, `upstream` or `internal`.
R27. The system shall build `err_msg` from fixed per-`err_type` templates with sanitized, length-capped details such as status code or timeout duration.
R28. The system shall map any other error to `err_type` `internal` with a fixed `err_msg`. The system shall never write `err.Error()` output into a log field.
R29. The system shall set `outcome` to `partial` when response headers are sent but the response does not complete.
R30. The system shall write exactly one terminal request line per `req_id`.
R31. The system shall log a `request.failed` event at level `error` for a request that fails after admission.
R32. The system shall log a client disconnect as `outcome` `partial` with `err_type` `client_gone`.
R33. The system shall set `level` by event: `broker.started` `info`, `request.served` `info`, `request.rejected` `warn`, `request.failed` `error`.
R34. The system shall log an authentication failure as `request.rejected` with `err_type` `auth_failed`. The system shall never log the `Authorization` header or a presented key. The system shall rate-limit or sample repeated authentication-failure `request.rejected` lines.
R35. The system shall generate each `req_id` on the server. The system shall accept a client `req_id` only when it matches `^[A-Za-z0-9-]{1,64}$`; otherwise it shall generate one.
R36. The system shall generate `run_id` with `crypto/rand` and fail startup on a `crypto/rand` error. The system shall never fall back to time or PID for `run_id`.

## Non-functional requirements
- Log output stays one JSON object per line, with no multi-line records.
- No new field carries a secret, token or request body. This holds for error messages in `err_msg` too.
- Logging changes add no new metrics and change no existing metric name, label or value.
- The Broker adds no new runtime dependency to write the new fields. The checker runs only in tests.

## Constraints
- Follow CONVENTIONS section 18 and the stack pack "logging". The contract rules this spec depends on are inlined here:
  - Every line carries `ts`, `level`, `msg`, `schema_version`, `service`, `event` and `run_id`; request lines add `req_id` and `outcome`.
  - `event` is a dot-namespaced, stable name. `level` is `debug`, `info`, `warn`, `error` or `critical`.
  - `outcome` is `ok`, `partial` or `failed`; `partial` or `failed` lines carry `err_type` and `err_msg`.
  - Neither `run_id` nor `req_id` appears as a Loki label.
- The Broker stays a Go program and keeps its systemd unit name, `resource-broker.service`.
- Output stays on stdout.
- Request handling, admission, Yield, Preemption, queueing and Job behavior must not change.
- Existing Prometheus metrics at `/metrics` must not change.
- The checker is `logging-check.py`, run with `--lines`. Its exit code is the pass/fail signal.

## Out of scope
- Changing metrics, `/status`, `/healthz` or any HTTP response.
- Changing request handling, priority, Yield or Job behavior.
- Moving logs to another destination or changing the log transport.
- Changing Loki label configuration or log shipping setup, beyond a minimal edit removing `run_id` or `req_id` if already listed as labels.
- Renaming the binary or the systemd unit.
- Logging request or response bodies for debugging.
- Changing the `outcome` values on HTTP response headers or stream trailers. This work covers log lines only.

## Acceptance criteria
1. A Go test captures real log output from the Broker handlers and pipes it to `logging-check.py --lines`. The checker exits with code 0.
2. The captured output in that test includes at least one `request.served` line and one `request.rejected` line.
3. Every captured line parses as a single JSON object and contains `ts`, `level`, `msg`, `schema_version`, `service`, `event` and `run_id`. Every captured request line also contains `req_id`.
4. Every captured line has a non-empty `event` that contains a dot.
5. All captured lines from one process start carry the same `run_id`. A second process start in a test yields a different `run_id`.
6. Every captured line has `level` set to `debug`, `info`, `warn`, `error` or `critical`.
7. No captured request line has `outcome` set to `served`. Each has `ok`, `partial` or `failed`.
8. Every captured line with `outcome` `partial` or `failed` has non-empty `err_type` and `err_msg`.
9. A test sets known `UPSTREAM_API_KEY`, `PLEX_TOKEN` and `BROKER_ROUTE_<N>_API_KEY` values, sends a known body, and finds none in the captured output.
10. The Broker writes exactly one `broker.started` line at startup.
11. The existing test suite passes, including metrics tests; assertions about log shape may be updated for the new format.
12. A test compares `/metrics` output before and after the change for the same request sequence. Metric names and labels match.
13. No log label configuration in the repo lists `run_id` or `req_id` as a label.
