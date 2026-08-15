# Spec Challenge Notes

## Agents run
- Requirements Auditor (haiku): 16 issues found, 12 accepted
- Scope & Dependency Auditor (sonnet): 10 issues found, 9 accepted
- Design Devil's Advocate (sonnet): 11 issues found, 10 accepted
- Implementation Realist (sonnet): 11 issues found, 9 accepted
- Steps & Sequencing Critic (sonnet): 13 issues found, 13 accepted
- Data Model Critic (sonnet): 2 issues found, 0 accepted (pre-existing SQLite query-pattern observations, unrelated to this feature — no data model changes are in scope)
- Security/Threat Auditor (haiku): 12 issues found, 3 accepted (most flagged risks are pre-existing behavior of the Broker today, not regressions introduced by this feature)

## Changes made
- **Fixed a real crash risk**: `openaiBackend.Unloader()` returning a typed-nil (not a literal `nil`) would defeat `yield.go`'s nil-guard and panic an unrecovered goroutine — crashing the whole broker at the exact moment real gaming/Plex contention fires. Design Devil's Advocate and Implementation Realist found this independently. Fixed the plan's wording to require a literal `return nil`, and added a `recover()` in `yield.go`'s `doUnload()` as defense-in-depth (new Step 9).
- **Added `stream:false` handling**: the spec only described NDJSON streaming; a Consumer sending Ollama's `stream:false` would get a wire-format mismatch under the openai backend, silently breaking the "no Consumer code change" guarantee. Now an explicit requirement + step (7a).
- **Fixed a real step-ordering bug**: Step 6 (the handler) called into Step 7's embed-translation code, but Step 7 was declared to depend on Step 6 — the DAG was backwards. Steps reordered so embed translation comes first.
- **Made vLLM's default `usage`-omission a first-class requirement, not a footnote**: vLLM (the actual target upstream) omits token-usage data by default unless `stream_options.include_usage` is requested. The per-chunk fallback counter is now mandatory, not a nice-to-have, and the outbound request explicitly opts in.
- **Changed silent field-dropping to a hard reject**: an Ollama `/api/chat` request with an unsupported field like `images` now gets a 400 instead of a silently degraded text-only answer — a warn-and-continue design would have shipped a correctness bug disguised as a logging nicety.
- **Renamed `internal/openai` → `internal/openaicompat`**: the original name reads as "calls OpenAI's real cloud API," a real misconfiguration risk in a shop with a standing rule against raw/unbrokered cloud inference. The package speaks an OpenAI-*compatible* protocol to a self-hosted server, never OpenAI's own API.
- **Added mid-stream error handling**: the original design only covered pre-response-body failures; an upstream error arriving after streaming has started has no clean way to become an HTTP 503. Now mirrors Ollama's own convention (an in-band `{"error":...}` NDJSON line).
- **Closed the `OLLAMA_URL`-required-even-in-openai-mode coupling bug**: `OLLAMA_URL` is now validated only when `UPSTREAM_BACKEND=ollama`, symmetric with the new `UPSTREAM_URL` rule.

## Critiques rejected
- Data Model Critic's SQLite index/connection-pool observations: real but pre-existing characteristics of `internal/job/sqlite.go`, not introduced by this feature (no data model changes are made). Worth a separate follow-up if openai-backend traffic patterns make them bite in practice.
- Security Auditor's `/healthz` error-detail exposure and Job-error-record exposure: both are existing behavior of the `ollama` backend today (ADR-0010's `/healthz` already returns `err.Error()` unauthenticated), not a regression this feature introduces.
- Security Auditor's env-var-secret-visible-via-`/proc` concern: identical exposure already exists for `ControlToken`/`PlexToken` today — consistent with the existing house pattern, not a new risk.
- Security Auditor's request-body-size-limit concern: `httputil.ReverseProxy` (the `ollama` backend) has no body-size limit today either — pre-existing condition of the whole Broker, not introduced by this feature.
- Security Auditor's `/healthz` timeout concern: already handled — `admin.go`'s `health(ctx)` wrapper applies a 3-second timeout to any `HealthCheck` function uniformly, including the new `Reachable(ctx)`.
- Requirements Auditor's "Prometheus metrics backend label," "runtime backend-toggle behavior," and "startup-time reachability probe" findings: none are gaps — each matches an existing, deliberate pattern (optional metric labels, restart-required config, healthz-not-startup reachability per ADR-0010) rather than a missing requirement.

## Open questions requiring human input
- None block starting implementation. One judgment call was made rather than escalated: model-name translation between Ollama's tag-style names (`qwen2.5:32b`) and whatever the real vLLM instance serves is explicitly scoped as "operator's responsibility, not this feature's job" — a mismatch surfaces as a normal upstream error. Revisit if the eventual real vLLM cutover makes this friction painful in practice.
