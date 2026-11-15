# Plan: Resource Broker Canonical Logging

## Approach
Every log line already passes through one place in the Broker that adds `ts`, `level`, `msg`, `schema_version`, `service` and `req_id`. We extend that place. It generates one `run_id` at process start and stamps it on every line. Each call site passes a fixed `event` name and, for request lines, one of three outcomes. We use only the standard library, so there is no new dependency. A Go test captures real handler output and runs it through `logging-check.py --lines`.

## Design decisions
- Stamp `run_id` inside the existing logger, not at call sites. Rejected: passing `run_id` at each call. Reason: R7 says every line carries it, and call sites would miss some.
- Generate `run_id` with `crypto/rand` as 16 hex characters. Rejected: a UUID library. Reason: the stdlib is enough, and the Broker adds no dependency (non-functional requirement).
- Make `event` a required argument of the log function. Rejected: deriving it from `msg`. Reason: R4 says `msg` wording may change without changing `event`. A required argument also makes a missing event a compile error.
- Keep event names as Go string constants in one file. Rejected: inline strings. Reason: the names are a stable contract, and one file lets a test check that each contains a dot.
- Map outcomes this way: `ok` for a completed request, `failed` for a request turned away or errored, `partial` for a stream cut after headers were sent (a preempted stream). Rejected: keeping `served` as an alias. Reason: R11 forbids it.
- Build `err_msg` from a short fixed text per `err_type`, such as `yield`, `queue_full`, `wait_timeout`, `preempted`, `upstream`. Rejected: writing `err.Error()` straight into the field. Reason: raw errors from upstream calls can carry URLs, headers or body fragments. R19 forbids those.
- Log only metadata on request lines: method, path, class, status, wait time. Rejected: logging any body or header map. Reason: R19.
- Leave HTTP response headers and stream trailers unchanged. Reason: out of scope. Those still say `served`.
- Check Loki labels with a test that scans the repo's shipping config under `deploy/`. Rejected: changing the label config. Reason: out of scope. We only prove `run_id` and `req_id` are absent from it.

## Architecture
None - no new components.

Data flow:

```
main (start) --> logger init: run_id = rand(16 hex)
                    |
 handlers / yield / jobs --log(level, event, msg, fields)--> logger
                    |                                          |
                    |                      adds ts, schema_version, service, run_id
                    v                                          v
              req_id from request context                 JSON line on stdout
```

## Data model
No data model changes.

## API / interface contract
Log line shape. Every line is one JSON object:

| Field | Rule |
| --- | --- |
| `ts`, `msg`, `schema_version`, `service` | As today. `service` value unchanged. |
| `level` | One of `debug`, `info`, `warn`, `error`, `critical`. |
| `event` | Required. Dot-namespaced, fixed per kind of occurrence. |
| `run_id` | Required. Same value for the life of the process. Never a Loki label. |
| `req_id` | On request lines only. Never a Loki label. |
| `outcome` | On request lines only. `ok`, `partial` or `failed`. |
| `err_type`, `err_msg` | Required when `outcome` is `partial` or `failed`. Fixed text only. |

Events this change defines:
- `broker.started` (info, once at startup, no `outcome`).
- `request.served` (info, `outcome` `ok`).
- `request.rejected` (warn, `outcome` `failed`, `err_type` such as `yield` or `queue_full`, for example the `503` during Yield).
- Existing non-request log lines get their own names in the same style, such as `yield.started` and `yield.cleared`. The implementer lists them by searching for every log call.

Where an existing line has no stable `outcome` meaning (for example a yield transition), it carries no `outcome`. The checker's rules decide whether that is allowed. See Risk areas.

No HTTP, flag or metric changes.

## Integration points
- The package that builds log lines today (the code that writes `schema_version`) - add `run_id` generation, require `event`, enforce the level set, and add `err_type`/`err_msg` handling. Its path was not in the supplied context. The implementer finds it by searching for `schema_version`.
- `cmd/` (Broker entry point) - build the logger first and log `broker.started` once, after config loads.
- Fronting Proxy request handlers (interactive, batch and embed lanes) - change each request log call to pass an event and an `outcome` of `ok`, `partial` or `failed`. Remove the `served` value from log lines only.
- `internal/yield/yield.go` and the Job/queue code - give each existing log call an event name, and add `err_type`/`err_msg` wherever a call reports a failure.
- New test file next to the logger - run handlers against a fake upstream, capture stdout, and pipe it to `logging-check.py --lines`. Asserts: exit code 0, at least one `request.served` and one `request.rejected` line, dotted events, one shared `run_id`, a different `run_id` on a second start, allowed levels, no `served` outcome, and `err_type`/`err_msg` on non-ok lines. A leak case sets known `UPSTREAM_API_KEY`, `BROKER_ROUTE_1_API_KEY` and `PLEX_TOKEN` values and a known request body, then greps the output.
- `testdata/` golden file for metrics - names and labels of `/metrics` for a fixed request sequence, captured from the code before this change. A test compares the current output to it.
- A repo-scan test over `deploy/` - fails if any Loki label config lists `run_id` or `req_id`.

## New abstractions
None.

## Technology choices
None.

## Risk areas
- I did not read the Go stack guide, the logging pack, `CONVENTIONS.md` or the checker. The supplied context has none of them. The exact field rules, such as whether non-request lines may omit `outcome`, are unknown. The checker's exit code decides. Expect one round of fixes after the first real run.
- The path to `logging-check.py` is unknown. The test reads it from an environment variable and fails with a clear message if it is missing. A skipped test would hide a broken contract.
- The path and shape of the current logger are unknown. If lines are built in several places instead of one, the work grows. Any line that bypasses the logger would miss `run_id`.
- Secret leaks through `err_msg` or path fields. Fixed texts prevent most of it. Request paths could still carry a token in a query string, so log the path without its query string.
- The metrics golden file must come from the pre-change code. If it is captured after edits, the test proves nothing. Capture it first.
