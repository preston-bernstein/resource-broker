# Requirements: Idle-unload for openai-compatible (vLLM) backend instances

## Terminology note

CONTEXT.md has no existing term for "a backend instance releasing its model from VRAM because it has gone unused for a while" — that concept is distinct from **Yield** (the whole-Broker state entered in response to gaming/Plex **Contention**) and must not be called Yield or conflated with it. This document coins:

- **Idle**: the condition of a given backend instance (the default backend, or one `BROKER_ROUTE_<N>` instance) having received no request routed to it for at least its configured idle duration.
- **Idle-unload**: the act of freeing that instance's VRAM (via the existing `systemctl stop`/`start` `yield.Unloader` mechanism) in response to Idle, and the symmetric act of bringing it back on next use.

Idle-unload is a second, independent *event source* into the same `yield.Controller` per-instance action chain that Contention already drives — not a new state, not a new controller, not a new unload mechanism. "Idle" describes one instance's traffic history; "Yield" remains reserved exclusively for the whole-Broker gaming/Plex response.

## Problem statement

Ollama frees VRAM on its own after `OLLAMA_KEEP_ALIVE` (60m on the desktop today) of inactivity. The openai-compatible backend (vLLM) has no equivalent: once loaded, it stays resident in VRAM indefinitely, and today the only thing that frees it is a real gaming/Plex Contention event triggering Yield (ADR-0014's `systemctl stop`/`start` mechanism). On a single ~16GB AMD RX 9070 XT shared between Ollama and vLLM, this asymmetry means whichever of the two was used more recently silently evicts the other from VRAM for as long as the box runs, with no Contention event required to trigger it and no Contention event able to reverse it. This was discovered live on 2026-08-15 during a local-model benchmarking sweep that alternated between Ollama and vLLM models and kept failing to get both loaded at once. The Broker operator needs the vLLM-routed instance(s) to release VRAM after their own inactivity window, symmetric to what Ollama already does, without weakening any existing gaming-preemption or per-instance ordering guarantee.

## Users / stakeholders

- **Broker operator** (runs the desktop, sets `UPSTREAM_UNIT_NAME` / `BROKER_ROUTE_<N>_*` config, reads WARN/INFO logs when something looks stuck) — the direct beneficiary; wants VRAM to free itself without manual intervention or a gaming session as the trigger.
- **Interactive/Batch Consumers** whose requests route to the openai-compatible instance(s) — experience a cold-start delay (systemd unit start + vLLM model load) on the first request after an Idle-unload, exactly as they already do after a Yield-triggered unload.
- **Gaming/Plex** — must continue to get absolute-priority VRAM access (ADR-0003/ADR-0004); Idle-unload must never interfere with or slow down that path.
- **Ollama-routed traffic** — the other consumer of the shared VRAM budget; is the reason this feature exists (freeing vLLM's VRAM when it's not the active workload).

## Functional requirements

1. The system shall support a per-instance Idle duration for every configured openai-compatible instance that already has a `yield.Unloader` wired (the default backend via `UPSTREAM_UNIT_NAME`, and each `BROKER_ROUTE_<N>` route via `BROKER_ROUTE_<N>_UNIT_NAME`).
2. The system shall read the default backend's Idle duration from a new config var, `UPSTREAM_IDLE_TIMEOUT`, parsed with the same `time.ParseDuration`-compatible idiom as `BROKER_BATCH_QUANTUM` (`getdur`/`getdurWarn`).
3. The system shall read each route's Idle duration from a new per-route config var, `BROKER_ROUTE_<N>_IDLE_TIMEOUT`, independent of every other route's and the default backend's value.
4. The system shall treat `UPSTREAM_IDLE_TIMEOUT` / `BROKER_ROUTE_<N>_IDLE_TIMEOUT` unset or empty as "idle-unload disabled for this instance," matching the existing unset-disables-it precedent set by `UPSTREAM_UNIT_NAME` and `BROKER_ROUTE_1_MODELS` — so a deployment that upgrades without setting the new var behaves byte-for-byte as it does today.
5. The system shall treat an explicitly-configured `UPSTREAM_IDLE_TIMEOUT`/`BROKER_ROUTE_<N>_IDLE_TIMEOUT` of exactly zero identically to unset (disabled). The system shall reject any negative duration for either var at `config.Load()` with a descriptive error, the same fail-loudly posture as FR6.
6. The system shall require that an instance have a non-nil `yield.Unloader` (i.e. its `_UNIT_NAME` is set) before its Idle duration config can take effect; an Idle duration configured for an instance with no `_UNIT_NAME` shall fail config validation at `config.Load()` with a descriptive error, the same load-time-fail-loudly posture ADR-0015 already applies to route-index gaps and unit-name collisions.
7. The system shall track, per configured instance, the timestamp of the most recent request actually routed to and proxied by that instance (i.e. a real request dispatch, matching the same per-instance resolution `Router.resolve` / the default backend's proxy already perform) — not the timestamp of the last request to the Broker as a whole, and not the timestamp of the last request to any other instance.
8. The system shall reset an instance's Idle timer only on a request that is actually dispatched to that specific instance; a request routed to a different instance, a request rejected before dispatch (e.g. `503` from Preemption or Yield), and requests to unrelated Broker surfaces (`/status`, `/healthz`, `/metrics`) shall not reset any instance's Idle timer.
9. The system shall trigger Idle-unload for an instance only after that instance's configured Idle duration has elapsed with no request dispatched to it, checked on a recurring basis (e.g. the existing detector poll loop cadence, `DetectInterval`) rather than via a fresh per-request timer goroutine — following the polling shape `yield.Controller.Run` already uses.
10. The system shall route an Idle-triggered unload through the exact same `yield.Unloader.Unload`/`Reload` call path and the exact same per-instance `actionDone` ordering chain (ADR-0014) that a Contention-triggered Yield already uses for that instance, so no new unload mechanism, systemctl invocation path, or ordering primitive is introduced.
11. The system shall not fire an Idle-unload for an instance that is currently Yielding (whole-Broker Yield already active) — an already-stopped/unloaded instance shall not receive a duplicate `Unload` call from the Idle path.
12. The system shall not fire an Idle-unload for an instance for which a Yield-start transition is concurrently in progress; both triggers converging on the same instance shall never produce a conflicting or out-of-order `Unload`/`Reload` pair for that instance — a second, strictly-ordered, idempotent `Unload` call on an already-idle-unloaded instance (Idle fired first, Contention's own unconditional per-instance unload loop fires again later) is acceptable and expected, since Contention's response is intentionally Idle-state-agnostic.
13. The system shall not treat an in-flight request to an instance as eligible for Idle-unload while that request is still executing: an instance actively serving a request from any Consumer shall not be unloaded out from under that request. (Note: today's `yield.Unloader.Unload` call has no notion of "wait for in-flight completion" — Contention-triggered Yield already cancels in-flight work via `serveCtx` before unloading. Idle-unload, having no analogous urgency, shall instead avoid firing while a request is in flight: the Idle check shall treat "currently serving a request" as equivalent to "not idle," resetting/deferring the timer rather than cancelling and unloading mid-request.)
14. The system shall clear (reset) an instance's own Idle timer on a Reload of that instance regardless of whether the Reload was triggered by Yield-clear or by real subsequent usage, so a just-reloaded instance is not immediately eligible for a second unload before it has had a chance to serve anything.
15. The system shall emit the same class of WARN/INFO log lines for an Idle-triggered `Unload`/`Reload` that Contention-triggered ones already emit (`doUnload`/`doReload`'s existing per-instance-labeled log lines), with an additional field or distinct log line identifying the trigger as Idle rather than Yield, so an operator reading logs can tell the two apart.
16. The system shall report each configured instance's Idle-eligibility state (enabled/disabled, configured duration, time since last dispatch) in a status surface consistent with the existing `RoutingSummary()` / `/status` `"routing"` key pattern, so an operator can observe the mechanism without reading logs.
17. For every instance with Idle-unload disabled (duration is unset or explicitly zero), the system shall not wrap that instance's Backend with the activity-tracking decorator, and `checkIdleLocked` shall skip that instance in O(1) with no atomic writes beyond the initial zero-value state.

## Non-functional requirements

- Idle-unload's per-tick check shall perform no blocking I/O and shall not wait on any `Unloader.Unload`/`Reload` call before returning control to the Contention-detection path in the same tick — testable by code inspection / a benchmark showing `checkIdleLocked`'s own execution time is dominated by O(instances) atomic reads only. Yield's absolute-priority response time is unchanged by this feature's presence.
- The per-instance `actionDone` ordering guarantee (ADR-0014) and the per-instance independence guarantee (ADR-0015, one instance's slow/erroring chain never blocks another's) must hold identically whether the triggering event was Yield or Idle.
- Config parsing failures for the new vars must fail at `config.Load()` (startup), not silently at runtime, matching every other malformed-duration case (`getdurWarn`'s existing behavior) or hard-fail (`getdur`'s), whichever this repo's convention dictates for a new required-shape var — to be resolved during implementation planning, not left ambiguous in shipped behavior.
- No new external dependency, polkit grant, or systemctl verb is introduced; Idle-unload must operate entirely within the polkit grant ADR-0014 already established (`systemctl stop <unit>` / `systemctl start <unit>`, unit-specific, no wildcard).

## Constraints

- Must reuse `internal/yield`'s existing `Controller`, `Unloader` interface, and `actionDone` per-instance ordering chain — an idle-triggered unload is a new event source into that machinery, never a parallel or separate controller.
- Must reuse the existing `systemdUnitController` (`internal/backend/openai_backend.go`) `Unload`/`Reload` calls — never vLLM's native `/sleep` + `/wake_up` API, which ADR-0014 explicitly rejected due to the open upstream bug vllm-project/vllm#20627 (corrupted output after repeated sleep/wake cycles on this exact OpenAI-compatible-server code path).
- Must preserve ADR-0003/ADR-0004's gaming-preemption invariant (gaming/Plex Contention always wins, unconditionally) — Idle-unload is strictly lower priority and must never interfere with a Contention-triggered transition.
- Must preserve ADR-0014's flap-ordering fix (`actionDone` predecessor-completion chaining) for every instance, whether the flap involves Yield transitions, Idle transitions, or a mix of both in quick succession.
- Must preserve ADR-0015's per-instance independent unload chains — one instance's Idle-unload state and timer must be fully independent of every other configured instance's.
- Must use the existing config idiom: flat, indexed, scalar `getenv`/`getdur`-style env vars (`UPSTREAM_IDLE_TIMEOUT`, `BROKER_ROUTE_<N>_IDLE_TIMEOUT`), never a new JSON/YAML config blob smuggled into one var.
- Must not require any change to the Ollama backend path; `ollamaBackend`'s existing `keep_alive`-based unload/no-op-reload behavior (ADR-0003, ADR-0014) is out of scope and already symmetric to what this feature adds for the openai-compatible path.
- Cannot rely on an active preflight/health probe of the unit before acting — ADR-0014 and ADR-0015 both deliberately rejected startup-time `systemctl show` probes; Idle-unload's failure mode on a misconfigured unit must be the same operator-visible WARN-log-on-first-use pattern, not a new preflight dependency.

## Out of scope

- Any change to Ollama's own `OLLAMA_KEEP_ALIVE` behavior — that mechanism already exists and works; this feature does not touch it.
- Using vLLM's native `/sleep` + `/wake_up` API as an alternative or fallback mechanism — explicitly rejected per ADR-0014, not reconsidered here.
- A startup-time preflight check that a configured unit exists or that the polkit grant is correctly scoped.
- New Prometheus metrics for Idle-unload events (matching ADR-0014's deliberate no-new-metric decision for the symmetric Yield-triggered unload/reload path); this may be revisited later if logs alone prove insufficient, same as ADR-0014 notes for its own path.
- Full systemd-unit-name-grammar validation of `_UNIT_NAME` values (already out of scope per ADR-0014, unchanged here).
- Predictive or usage-pattern-based unloading (e.g. "unload if VRAM pressure detected" or "unload the least-recently-used instance when a second one is requested") — this feature is a fixed per-instance idle timeout only, not a VRAM-pressure-aware scheduler.
- Any change to request admission, queueing, Park, or Preemption behavior — Idle-unload only affects when an already-idle instance's VRAM is released, never which requests get admitted or in what order.
- Coordinating Idle-unload across multiple physical machines or multiple Brokers — single-desktop, single-Broker-process scope only, consistent with every other yield mechanism in this repo.
- Tracking activity on the durable Job path (`job.Worker.Generate`, `Router.Generate`) — only Synchronous requests proxied through `Router.resolve`/the default backend's `Proxy()` reset an instance's idle clock (FR7). An instance driven exclusively by Job-path traffic with no Synchronous traffic can be idle-unloaded while a Job is actively running against it. This is a known, accepted limitation, not a defect to fix in this feature.

## Acceptance criteria

1. With `UPSTREAM_IDLE_TIMEOUT` and every `BROKER_ROUTE_<N>_IDLE_TIMEOUT` unset, Broker behavior (config shape, goroutines started, log output, `/status` payload) is byte-for-byte identical to the pre-feature build.
2. Given an instance with `_UNIT_NAME` set and an Idle duration configured, when no request is dispatched to that instance for at least the configured duration, the system issues exactly one `Unload` call for that instance and the corresponding systemd unit is stopped.
3. Given an instance mid-Idle-unload-eligibility, when a request is dispatched to that instance before its Idle duration elapses, the Idle timer resets and no `Unload` call is issued at the original deadline.
4. Given two configured instances (default backend + one route) with different Idle durations, activity on one instance never resets or otherwise affects the other instance's Idle timer or state.
5. Given an instance that is Idle-unloaded, when a new request is dispatched to it, the system issues a `Reload` (systemd unit start) via the existing Unloader path, and the request proceeds after the unit comes up (same cold-start behavior as today's Yield-triggered reload).
6. Given an instance already stopped due to whole-Broker Yield being active, the Idle check does not issue a second `Unload` call for that instance while Yield remains active.
7. Given an instance's Idle duration is about to elapse at the same time gaming/Plex Contention begins, the resulting action sequence for that instance never contains a conflicting or out-of-order `Unload`+`Reload` pair (verified via the existing `actionDone`-chain ordering guarantee extended to Idle-sourced actions); a second, later, strictly-ordered `Unload` call from Contention's own unconditional loop on an already-unloaded instance is an acceptable idempotent outcome, not a failure.
8. Given a request is actively in flight against an instance, the Idle check does not fire an `Unload` for that instance until the request completes and the instance's last-dispatch timestamp reflects it.
9. Setting `BROKER_ROUTE_<N>_IDLE_TIMEOUT` for a route whose `BROKER_ROUTE_<N>_UNIT_NAME` is unset fails `config.Load()` with a descriptive error and the process does not start.
10. Setting `UPSTREAM_IDLE_TIMEOUT=-5m` (or the per-route equivalent) fails `config.Load()` with a descriptive error and the process does not start.
11. An Idle-triggered `Unload`/`Reload` produces a log line identifiable as Idle-sourced (distinguishable from a Yield-sourced log line for the same instance) by grepping the logs.
12. `/status` (or the equivalent existing status surface) reports, for every instance with Idle-unload enabled, its configured Idle duration, current idle/active state, and time since its last dispatch.
13. Existing `internal/yield` tests (Contention-only scenarios) and `internal/backend` tests (openai backend, router, parity) pass unmodified, demonstrating no behavioral regression to the pre-existing Yield path.
