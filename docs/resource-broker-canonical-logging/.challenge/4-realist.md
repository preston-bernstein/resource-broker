1. **Logger "one place" is unconfirmed, and `internal/yield/yield.go` suggests it is false.** The file imports `log/slog`, so at least some calls go straight to slog rather than through a custom function. Step 2 says "find it with grep -rn schema_version", but that grep may only find a `slog.Handler` wrapper, or a second logger. The developer must first map every logging path: slog default, `log.Printf`, `http.Server.ErrorLog`, and any per-package loggers. Anything that bypasses the wrapper will lack `run_id` and `event`.

2. **The "compile error" guarantee in Step 4 doesn't hold for slog.** `slog.Info(msg, args...)` always compiles, so deleting the shim only enforces `event` where code was moved to the new function. Direct `slog.*` calls in `yield.go` and elsewhere stay silent and non-canonical. The plan needs one of these:
   - a `slog.Handler` that rejects or flags records with no `event` attribute
   - a grep or `go vet` style test that bans direct `slog.` calls outside the logger package

3. **Level mapping.** slog has no `critical` level and renders levels uppercase (`WARN`, `ERROR+4`). The allowed set is lowercase `debug/info/warn/error/critical`. The developer must define a custom level and a `ReplaceAttr` that lowercases and names it. This is easy to get subtly wrong, and the plan doesn't mention it.

4. **`run_id` lifetime conflicts with its test.** The approach says "at process start", but Step 2 says "a fresh logger gets a different run_id", and Step 5 needs a second start in one test binary. A package-level singleton or `init()` makes both untestable in-process. The logger needs a constructor that takes the `run_id` source, or Step 5 must spawn subprocesses. The plan never decides.

5. **`cmd/` was not found.** The entry point is probably `cmd/<name>/main.go`. Step 4 says "the main package under cmd/" without a path, and the startup test needs the broker to run in-process or as a subprocess. Nothing says how to do either.

6. **`deploy/` and `testdata/` don't exist.** Step 6's scan will pass vacuously if Loki/Promtail/Alloy config lives elsewhere, such as another repo or the host. The test must fail if it finds no shipping config, otherwise "passes on current repo" proves nothing. The fixture-failure check in Step 6 also needs a scanner that handles the real config format (YAML `labels`, `pipeline_stages` `labels:`, `structured_metadata`), and the plan doesn't say which.

7. **Unlisted log sites in the blast radius.** `yield.go` mentions `displace.go` (ADR-0020) and `checkIdleLocked`, `applyLocked`, `doUnload` and `doReload`. Beyond that there are the GPUManager (Tdarr) pause/resume paths, the config loader, auth and route code, and the metrics handler. The integration list names only yield and "job/queue". Removing the shim in Step 4 will either fail the build in these files or leave them on the old path. Step 4 should say "every call site found by grep", not a file list.

8. **`yield.go` trigger naming collides with `err_type`.** `unloadTrigger` is `yield`, `idle` or `displace`, and the planned `err_type` also includes `yield`. Unload/reload failure lines need their own event names and `err_type` values, such as `unload_failed` or `reload_timeout`. The listed `err_type` set (`yield`, `queue_full`, `wait_timeout`, `preempted`, `upstream`) doesn't cover them. The `unloadReloadTimeout` comment says a client timeout does not prove the unload stopped, so a timeout line must not claim `outcome=failed` as if the action had been cancelled. The `err_msg` text for it needs care.

9. **Raw errors in `yield.go` and GPUManager.** Unloader and GPUManager errors may contain command output, D-Bus text or Tdarr URLs. "Fixed text per `err_type`" must apply to these too, and Step 4's tests cover no leak case for them.

10. **Missing event name for `partial`.** The events list defines only `request.served` (ok) and `request.rejected` (failed). A stream cut after headers has `outcome=partial` but no event. An upstream error mid-request (`err_type=upstream`) is not "rejected" either. The developer will invent `request.partial` and `request.failed`, or misuse the two existing names. Levels for those are also unspecified.

11. **`req_id` on non-request lines.** Lines from the durable `/jobs` path and from yield goroutines have no request context. Whether they carry `req_id` or `outcome` is "decided by the checker", which is unread. Step 3 needs a rule for lines emitted before the request context is populated, for example middleware ordering or early 4xx rejects.

12. **Shared `served` constant.** Step 3 says to remove `served` "from log lines only", but HTTP headers, trailers and possibly metric labels use the same string. If one constant feeds both, changing it breaks the headers and the Step 1 golden metrics. The developer must split the constant first.

13. **Step 1 golden file is flaky and underspecified.** "Fixed request sequence" needs a fake upstream and a defined sequence. Histograms, durations and any `run_id`-like labels must be excluded. Compare names and label keys only, not values. The step doesn't say so. Step 1 also has `Covers: none`, though R20/metrics-unchanged is its purpose.

14. **Ordering.**
    - Step 1 must complete before any edit, not just before Step 2. "Parallelizable: Yes" with Step 6 is fine, but Step 6 must not touch metrics code.
    - Steps 3 and 4 both edit handler and yield files and the logger (shim removal), so they conflict if parallelized. Step 4's startup test also duplicates Step 5's harness. Build the fake-upstream harness once, in Step 2 or 3.
    - During Steps 2 and 3 the shim emits lines with no `event`, so the Step 5 checker can only pass after Step 4.

15. **Test gaps.**
    - R4 (changing `msg` doesn't change `event`) has no test.
    - There is no test that HTTP headers and trailers still say `served`.
    - The leak test doesn't cover tokens in query strings (the plan lists this as a risk).
    - The leak test doesn't cover an upstream error whose text contains the key.
    - The leak test doesn't cover yield, job or unload errors.
    - Step 5 never exercises the `partial` path, or the `queue_full` and `wait_timeout` rejects.
    - There is no test that `req_id` and `run_id` are absent from non-log outputs (metrics labels).

16. **Checker dependency in CI.** Making `go test ./...` fail when the checker path env var is unset breaks every developer and CI run that lacks it, including unrelated packages. Gate it behind a build tag or a dedicated target, while keeping the "no silent skip" rule there.

17. **Single most likely mistake:** trusting "one place adds schema_version" and wrapping only that function. `yield.go` already uses `log/slog` directly, so the first run passes the handler tests while yield, job and displace lines lack `run_id` and `event`. The Step 4 "compile error" check is then a false assurance. Build the enforcement into a `slog.Handler`, and add a test that fails on any record without `event`.
