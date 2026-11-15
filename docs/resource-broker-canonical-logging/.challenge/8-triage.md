## Accepted: requirements

- [UNTESTABLE] — R4: restate as a checkable rule — event names held as constants in one file, verified by test.
- [UNTESTABLE] — R5: replace "human-readable" with "non-empty string".
- [MISSING] — AC 9: extend the leak test to the `BROKER_ROUTE_<N>_API_KEY` route keys.
- [MISSING] — add a requirement covering path logging policy: log the path without its query string.
- [MISSING] — define the `err_type` vocabulary and the fixed-text rule for `err_msg` in requirements.
- [MISSING] — state whether non-request lines may carry or must omit `outcome`.
- [MISSING] — R9 has no acceptance criterion; add `req_id` to AC 3's field list.
- [MISSING] — AC 10: add a count assertion so `broker.started` is checked to appear once.
- [CONTRADICTION] — AC 5: scope the shared-`run_id` assertion per process start.
- [VAGUE] — R17: name the literal current `service` value instead of "today".
- [VAGUE] — R16: enumerate which rejection conditions count as `request.rejected`.
- [VAGUE] — Constraints: "the pack's rules win" defers to an unsupplied document; inline the field rules this spec depends on.
- [LEAKED] — move the plan's "log only metadata on request lines: method, path, class, status, wait time" policy into requirements.
- [LEAKED] — move the plan's event-to-level mapping (`request.rejected` warn, `broker.started` info, etc.) into requirements.
- [FALSE OUT-OF-SCOPE] — allow a conditional minimal Loki label edit if the scan finds `run_id`/`req_id` already listed as labels.
- [HIDDEN DEP] — AC 11: amend to "existing suite passes, with log-shape assertions updated".
- [UNDER-SCOPED] — define the boundary of "request log line" (e.g., lines emitted by request handlers).
- [UNDER-SCOPED] — enumerate the full closed `err_type` set exactly, not "such as".
- [SCOPE CREEP] — justify method/path/class/status/wait-time fields as preserving today's shape, or cut them.
- Keep `err_type` fixed but make `err_msg` a sanitized, length-capped string; do not delete diagnostic facts like status code or timeout duration.
- Define `partial` as headers-sent-but-not-completed, with `err_type` distinguishing `preempted`, `client_gone`, `upstream`; require exactly one terminal request line per `req_id`.
- Define event and level for errored requests (e.g., `request.failed` at error); give client-cancel a defined outcome.
- Generate `req_id` server-side; if a client value is accepted, validate against a strict regex and length cap.
- Treat logged path/method/class as attacker-controlled: JSON-encode only, cap field length, prefer route template or class over raw path.
- Define the auth-failure event and `err_type`; never log the `Authorization` header or presented key; rate-limit or sample repeated rejections.
- Unmapped errors default to a fixed `err_type`/`err_msg`; never fall back to `err.Error()`.
- Forbid body and header values at all levels, including debug.
- Fail startup on a `crypto/rand` error; never fall back to time or PID for `run_id`.

## Accepted: plan

- [UNLISTED ABSTRACTION] — list the canonical log function and the `events.go` constants file under New abstractions, with handlers/yield/cmd as callers; also the `err_type`/`err_msg` table and the `outcome` enum.
- Add an event naming rule and a required inventory table (call site, event, level, outcome, err_type), approved before implementation; name `request.failed` and `request.partial` or document why one event carries all outcomes.
- Before implementing, grep the repo, `deploy/` and agent-config for consumers of `served` and old level strings; update them in the same change; record the header/metric vs log vocabulary split as dated debt.
- Make `event` a named type constructible only from the constants file; have the logger reject or panic on empty event in tests; drop the "compile error" claim.
- Reserve optional `job_id` (never a Loki label), or state explicitly that jobs correlate via the creating `req_id`.
- `critical` has no `slog` equivalent: define a custom `slog.Level` plus name mapping, and define what "rejected" means for out-of-set levels (not a production panic).
- Resolve startup ordering: state what config-load-error lines look like before the logger exists and what `run_id` they carry.
- Note that `partial` detection requires wrapping `ResponseWriter` to track headers-sent and to distinguish preemption from client disconnect.
- Make the logger accept only an allow-listed set of field keys; never interpolate raw errors into `msg`.

## Accepted: steps

- [HIDDEN DEP] — add Step 1 to Step 5's "Depends on".
- [HIDDEN DEP] — Step 6: locate the Loki/promtail/Alloy label config first, or walk the whole repo.
- [ORPHAN PREREQ] — document the `logging-check.py` path and wire the env var into CI before Step 5.
- [UNDER-SCOPED] — read CONVENTIONS §18 and the logging pack before Step 2 to settle optional `outcome` on non-request lines.
- [UNDER-SCOPED] — verify the preempted-stream path emits a log today, or add a test that forces it.
- Make "confirm there is a single emission point and its construction model" the first task; state the injected-logger decision.
- Gate the checker shell-out test with a build tag or make target that CI always runs; pin the checker; keep a fast in-process schema test that always runs.
- Drop the `/metrics` golden file, or assert only the stable owned subset (metric names and label keys) excluding runtime metrics; make pre-change capture an ordered task.
- Step 2: the seam is an `slog.Handler` or wrapper, not a custom function; use `ReplaceAttr` for `ts` and lowercase level names.
- Step 4: add a handler check or lint gate on raw `slog.*` calls; the build alone does not prove every line carries an event from the constants file.
- Step 4 must say "remove raw `err` from every call", including yield `doUnload`/`doReload` and the Plex detector.
- Step 3: require `slog.InfoContext` so `req_id` from context survives; assert `req_id` present on request lines.
- Extract `run(cfg, out io.Writer)` so tests capture stdout; generate `run_id` in a constructor, not a package-level var or `init()`.
- Step 6 must fail when zero config files are found; add a fixture under `testdata/`; handle non-YAML formats (Alloy River `stage.labels`).
- Add a response-header and stream-trailer assertion so a shared `served` constant rename is caught.
- Include unlisted call sites: `displace.go` (`triggerDisplace`), the idle path (`checkIdleLocked`), and `GPUManager` pause/resume; bump `schema_version` if the line shape changed.
- Step 5 must drive and assert a `partial` line with `err_type=preempted`.
- Step 5 must emit and check non-request lines (yield start/clear, unload/reload failure, job/queue, displace) through the real checker.
- Step 5: assert the logged path carries no query string; add leak cases for a secret in a query string, in an `Authorization` header, in an upstream 5xx body, a preempted stream, and a Job failure.
- Step 2: drop the declared dependency on Step 1; the logger rework is independent of the metrics baseline.
- Split Step 5 into a checker-pipe contract test and a separate secret/redaction test.
- Split Step 4 into "name all remaining calls" and "delete the shim" as its own verified step.
- Add a step between Steps 3 and 4 to update legacy tests asserting the old log shape.
- Step 4 rollback: record the exact revert order (re-add shim, then revert call sites).
- Scan label config for pipeline stages that promote JSON keys to labels, not just literal label lists; document that host-level config is outside the test's coverage.
- Invoke the checker with `exec.Command` argv (never a shell), `python -I`, path resolved inside a trusted directory; keep it test-only.

## Rejected (and why)

- Steps critic — "Step 5 omits Step 1 from depends-on": duplicate of the scope auditor's identical finding, accepted there.
- Design — "parse the label config or drop the test": duplicate; covered by the accepted Step 6 findings on locating config, failing on zero files, and fixtures.
- Design — "New abstractions: None hides contracts": duplicate of the scope auditor's [UNLISTED ABSTRACTION] finding, accepted there with the same fix.
- Design — "promote path-without-query to a design decision": duplicate; covered by the accepted requirements finding on path policy and the accepted Step 5 leak-test additions.
- Realist 9 — "golden metrics file may be flaky": duplicate of the accepted design finding on the same golden file.
- Realist 17 — "contradiction on the checker path": duplicate; covered by the accepted orphan-prerequisite finding and the build-tag gating finding.
- Realist 18 — "`crypto/rand` error handling": duplicate of the security auditor's run_id finding, accepted there.
- Data model [INDEX] — Loki scan-cost flag: informational only; the spec already accepts the no-labels trade-off, so no spec change is required.
