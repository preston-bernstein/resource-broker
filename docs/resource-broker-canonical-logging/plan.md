# Plan: Resource Broker Canonical Logging

## Approach
Every log line already passes through one place in the Broker. That place adds `ts`, `level`, `msg`, `schema_version`, `service` and `req_id`. We extend it. It generates one `run_id` at process start and stamps it on every line. Each call site passes a typed `event` and a fixed set of allowed fields. Request lines also carry one of three outcomes. We use only the standard library. A Go test captures real handler output and runs it through `logging-check.py --lines`.

Work runs in this order:
1. Capture the metrics golden file from the unchanged code.
2. Search for every consumer of `served` and of the old level strings.
3. Write the event inventory table and get it approved.
4. Change the logger.
5. Change the call sites.

## Design decisions
- Stamp `run_id` inside the existing logger, not at call sites. Rejected: passing `run_id` at each call. Reason: R7 says every line carries it, and call sites would miss some. Reversibility: two-way. The field stays, but its source can move.
- Generate `run_id` with `crypto/rand` as 16 hex characters. Rejected: a UUID library. Reason: the stdlib is enough, and the Broker adds no dependency.
- Make `event` a named Go type. Only the constants file can build a value of it. Rejected: a plain string argument, and deriving `event` from `msg`. Reason: R4 says `msg` wording may change without changing `event`.
- Reject an empty `event` at run time. Tests panic on it. Production writes `log.invalid` at level `error` and keeps the line. Rejected: claiming a compile error. Reason: Go cannot make a named string type reject an empty value at compile time.
- Name events `<area>.<action>`. Use lowercase words, one dot, and a past-tense or state word. Reversibility: one-way. Dashboards and alerts will key on these names, so rename only with a migration note.
- Require an event inventory table before implementation. It lists call site, event, level, outcome and `err_type` for every log call. Preston approves it first. Reason: the names are a stable contract.
- Keep event names as Go constants in one file. Rejected: inline strings. Reason: one file lets a test check that each name contains a dot.
- Use three request events: `request.served` for `ok`, `request.partial` for `partial`, `request.failed` for `failed`. Rejected: one event for all outcomes. Reason: alerts can then filter on `event` alone.
- Map outcomes this way:
  - `ok` is a completed request.
  - `failed` is a request turned away or errored.
  - `partial` is a stream cut after headers were sent.
  - Rejected: keeping `served` as an alias. Reason: R11 forbids it. Reversibility: one-way for log consumers. Update them in the same change.
- Wrap `ResponseWriter` to track whether headers were sent. Compare the context cancel cause to tell preemption from client disconnect. Preemption gets `err_type` `preempted`. A client disconnect gets `client_gone`.
- Build `err_msg` from a short fixed text per `err_type`. The types are `yield`, `queue_full`, `wait_timeout`, `preempted`, `client_gone` and `upstream`. Rejected: writing `err.Error()` into the field. Reason: upstream errors can carry URLs, headers or body fragments. R19 forbids those.
- Never put a raw error in `msg`. Reason: `msg` is free text and bypasses the allow-list.
- Accept only allow-listed field keys in the logger. The keys are `method`, `path`, `class`, `status`, `wait_ms`, `err_type` and `err_msg`. Tests fail on any other key. Production drops it. Rejected: free-form field maps. Reason: R19.
- Log the request path without its query string. Reason: a query string can carry a token.
- Add `critical` as a custom `slog.Level` above `error`. Map level names in one table. Production coerces an out-of-set level to `error`. Tests fail on it. Rejected: a production panic.
- Correlate jobs through the `req_id` of the request that created them. Rejected: a reserved `job_id` field. Reason: no spec caller needs it yet.
- Create the logger before config loads. Config-load errors use event `broker.config_failed` at level `critical`. They carry the same `run_id`. They use the same `service` constant as today.
- Leave HTTP response headers, stream trailers and metrics unchanged. Those still say `served`. Record this as debt dated 2026-11-15: log vocabulary and wire vocabulary now differ.
- Update every consumer of `served` and of old level strings in the same change. Search the repo, `deploy/` and agent-config first. Reason: a silent break in a dashboard or alert is worse than a larger diff.
- Check Loki labels with a test that scans the shipping config under `deploy/`. Rejected: changing the label config. Reason: out of scope. We only prove `run_id` and `req_id` are absent from it.

## Architecture
None - no new components.

Data flow:

```
main (start) --> logger init: run_id = rand(16 hex)
                    |
 handlers / yield / jobs --log(level, event, msg, fields)--> logger
                    |                                          |
                    |                      adds ts, schema_version, service, run_id
                    |                      checks level and field allow-list
                    v                                          v
              req_id from request context                 JSON line on stdout
```

## Data model
No data model changes. `run_id` lives in memory and resets on each start. The log line shape changes, so downstream consumers are a migration hazard. The consumer search covers them.

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
| `method`, `path`, `class`, `status`, `wait_ms` | Request metadata only. `path` has no query string. |

Fixed error texts:

| `err_type` | Used when |
| --- | --- |
| `yield` | The Broker turned a request away during Yield. |
| `queue_full` | The queue had no room. |
| `wait_timeout` | The request waited too long. |
| `preempted` | A stream was cut by preemption. |
| `client_gone` | The client disconnected mid-stream. |
| `upstream` | The upstream call failed. |

Events this change defines:
- `broker.started` (info, once at startup, no `outcome`).
- `broker.config_failed` (critical, no `outcome`).
- `request.served` (info, `outcome` `ok`).
- `request.partial` (warn, `outcome` `partial`, `err_type` `preempted` or `client_gone`).
- `request.failed` (warn or error, `outcome` `failed`, `err_type` such as `yield` or `queue_full`). The `503` during Yield uses this event.
- `log.invalid` (error, production only, for an empty event).
- Existing non-request lines get names in the same style, such as `yield.started` and `yield.cleared`. The approved inventory table lists them.

Where an existing line has no stable `outcome` meaning, it carries no `outcome`. The checker's rules decide whether that is allowed. See Risk areas.

No HTTP, flag or metric changes.

## Integration points
- The package that builds log lines today (the code that writes `schema_version`). Add `run_id` generation, the `event` type, the level table, the field allow-list and `err_type`/`err_msg` handling. Its path was not in the supplied context. Find it by searching for `schema_version`.
- Events file next to the logger. It holds the `event` type and all event constants. Build it before any call site changes.
- `cmd/` (Broker entry point). Build the logger first. Log `broker.config_failed` on a config error. Log `broker.started` once, after config loads.
- Fronting Proxy request handlers (interactive, batch and embed lanes). Pass an event and an `outcome` of `ok`, `partial` or `failed`. Wrap `ResponseWriter` to track headers sent. Remove `served` from log lines only.
- `internal/yield/yield.go` and the Job/queue code. Give each log call an event name. Add `err_type`/`err_msg` wherever a call reports a failure.
- Consumer search, run before any edit. Search the repo, `deploy/` and agent-config for `served` and the old level strings. Update each hit in this change. List any hit left alone in the debt note.
- New test file next to the logger. Run handlers against a fake upstream. Capture stdout. Pipe it to `logging-check.py --lines`. Assert:
  - exit code 0;
  - at least one `request.served` line and one `request.failed` line;
  - one `request.partial` line from a cut stream;
  - dotted events, and one shared `run_id`;
  - a different `run_id` on a second start;
  - allowed levels, and no `served` outcome;
  - `err_type`/`err_msg` on non-ok lines;
  - the test logger panics on an empty event;
  - the test logger fails on a field key outside the allow-list.
  
  A leak case sets known `UPSTREAM_API_KEY`, `BROKER_ROUTE_1_API_KEY` and `PLEX_TOKEN` values, a known request body and a known query-string token. It then greps the output.
- `testdata/` golden file for metrics. It holds names and labels of `/metrics` for a fixed request sequence. Capture it from the code before this change. A test compares the current output to it.
- A repo-scan test over `deploy/`. It fails if any Loki label config lists `run_id` or `req_id`.

## New abstractions
- Canonical log function in the logger package. It takes level, `event`, `msg` and allow-listed fields. It stamps `run_id`. Second caller: `internal/yield/yield.go` logs yield transitions through it, alongside the request handlers.
- `event` type and constants file. Only this file can build an `event` value. Second caller: the Job/queue code uses the same constants as the request handlers.
- Fixed `err_type` and `err_msg` table. Second caller: `internal/yield/yield.go` reports `yield` failures with the same table the request handlers use.
- `outcome` enum with `ok`, `partial` and `failed`. Second caller: the batch and embed lane handlers use it as the interactive lane does.

## Technology choices
- Go standard library only. Use `crypto/rand` for `run_id` and `log/slog` for levels, with a custom `critical` level. No new dependency.

## Risk areas
- I did not read the Go stack guide, the logging pack, `CONVENTIONS.md` or the checker. The supplied context has none of them. The exact field rules are unknown. One example is whether non-request lines may omit `outcome`. Another is whether `schema_version` must change. The checker's exit code decides. Expect one round of fixes after the first real run.
- The path to `logging-check.py` is unknown. The test reads it from an environment variable. It fails with a clear message if the variable is missing. A skipped test would hide a broken contract.
- The path and shape of the current logger are unknown. If lines are built in several places, the work grows. Any line that bypasses the logger misses `run_id`.
- The source of `service` is unknown. If config sets it, config-error lines need the same constant as today. Check this first.
- Secret leaks through `err_msg`, `msg` or path fields. Fixed texts, the field allow-list and path trimming prevent most of it. The leak test covers the rest.
- Telling preemption from client disconnect depends on how Yield cancels streams. If it sets no cancel cause, add one.
- A missed consumer of `served` breaks silently. The consumer search must run before edits. Rerun it before merge.
- The metrics golden file must come from the pre-change code. If it is captured after edits, the test proves nothing. Capture it first.
