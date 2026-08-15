# Spec Challenge Notes

## Agents run
- Requirements Auditor (haiku): 10 issues found, 10 accepted
- Scope & Dependency Auditor (sonnet): 8 issues found, 8 accepted
- Design Devil's Advocate (sonnet): 7 issues found, 5 fully accepted (redesign + timeout + concurrency guard + narrowed seam + sudoers scope), 2 accepted-with-lighter-fix (preflight check and new metric downgraded to "document as accepted gap" rather than built)
- Implementation Realist (sonnet): 10 issues found, 10 accepted
- Steps & Sequencing Critic (sonnet): 10 issues found, 10 accepted
- Data Model Critic (sonnet): 0 issues found (confirmed: no data model in this feature)
- Security/Threat Auditor (haiku): 3 issues found, 3 accepted

## Changes made

- **Architecture redesign (the headline finding):** the original spec had the openai backend's `Unloader.Unload()` run `systemctl restart <unit>` on yield-start only. Design review caught that `restart` = stop-then-start bundled as one systemd job, so a `vllm serve` unit eagerly reloads its model and re-claims GPU memory a few seconds later — regardless of whether gaming contention is still active. That silently reproduces ADR-0003's already-rejected "cancel-but-leave-loaded" failure, just delayed. Fixed by widening `yield.Unloader` to two methods (`Unload`/`Reload`), called symmetrically on yield-start (`systemctl stop`) and yield-clear (`systemctl start`) — mirroring how Ollama actually behaves (unload now, lazy-reload only when serving resumes).
- **Caught a real compile-break dependency:** widening `yield.Unloader` breaks `internal/yield/yield_test.go`'s four existing test doubles, which only implement `Unload`. This is now an explicit, sequenced integration point (Step 4) instead of a surprise discovered mid-implementation.
- **Caught a real test-coverage gap (Implementation Realist's "most likely mistake" finding):** every test in the original draft constructed the Unloader by hand, so a forgotten/nil command-runner field in the real `newOpenAIBackend` constructor would have been invisible to the whole test suite. Added a required factory-constructed test (Step 11) as the one shape that would catch it.
- **Caught test duplication:** `internal/backend/openai_backend_test.go` already has two passing tests covering the nil/unset case (`TestOpenAIBackendUnloaderIsTrueNilInterface`, `TestOpenAIBackendUnloaderDoesNotTriggerDoUnload`) — a prior draft would have written near-duplicates. Steps now say "verify unchanged," not "write new."
- **Fixed a broken test-trigger reference:** steps previously referenced the unexported `Controller.applyLocked()` as a test trigger, which is unreachable from `internal/backend`'s test package. Corrected to `Controller.SetMode(yield.ModeForceYield)`, the pattern the existing tests already use.
- **Narrowed the command-runner seam and added concurrency safety:** the original design's fully-generic "run any command with any args" function was flagged as inviting future misuse. Narrowed to `func(ctx, verb string) error` (only "stop"/"start" against the one configured unit), plus a mutex so a yield-clear/yield-start flap can't race `systemctl start` against `systemctl stop`.
- **Widened and documented the shared timeout:** `yield.go`'s hardcoded 10s (sized for Ollama's near-instant HTTP unload) is bumped to a single shared 30s constant covering both `doUnload`/`doReload`, with an explicit comment/ADR note that this is a client-side give-up bound only — `systemctl` talks to PID 1 over D-Bus, so a timed-out `exec.CommandContext` does not cancel the already-queued systemd job.
- **Named real accepted costs instead of leaving them silent:** the ADR must now explicitly document (a) connection errors during the stop/restart window (handled by existing retry/backoff, no new coordination built), (b) no active startup preflight check, (c) no new Prometheus metric — with an explicit note on the tension with ADR-0013's precedent (which did add observability for a comparable problem) rather than pretending it doesn't exist.
- **Sudoers/polkit privilege-escalation scope:** security review flagged that a wildcard sudoers/polkit grant would let a misconfigured `UPSTREAM_UNIT_NAME` value control what the broker's service account can stop/start on the host. ADR must recommend a unit-specific grant explicitly, not leave scoping to operator discretion silently.
- **Sequencing fixes:** the ADR now hard-depends-before the code steps (was previously unordered/parallelizable, contradicting this repo's own change-control gate); the oversized original "Step 4" (6 sub-changes bundled into one file) is split into a define step and a wire-up step; three separate single-line doc edits are merged into one mechanical step; a same-file race between two steps is fixed.
- **Whitespace-only `UPSTREAM_UNIT_NAME` is now explicitly defined:** trimmed at the `config.Load()` parse boundary and treated identically to unset/empty, closing a previously-undefined third state that AC14 implicitly tested but no requirement addressed.

## Critiques rejected

- A brand-new Prometheus metric for unload/reload outcomes — rejected to keep scope bounded (this is a lower-traffic, operator-configured path, unlike ADR-0013's per-request hot-path embed lane); documented as a deliberate, named scope cut in the ADR instead of silently omitted.
- An active startup-time preflight check (`systemctl show <unit>`) — rejected for the same scope-discipline reason; documented as an accepted gap in the ADR.
- Full systemd-unit-name syntax validation beyond whitespace-trimming — stays operator responsibility, unchanged from the original scope cut.
- A per-backend configurable timeout — a single shared constant bump is enough; per-backend tuning would be gold-plating nothing in the requirements asks for.

## Open questions requiring human input

None blocking. The sudoers/polkit host provisioning itself (out of repo scope) is Preston's call on timing, already tracked as separate work against the desktop vLLM systemd unit being provisioned in parallel.
