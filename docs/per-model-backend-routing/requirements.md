# Requirements: Per-Model Upstream Backend Routing

## Problem statement

The Broker today has exactly one upstream backend for the whole process: `UPSTREAM_BACKEND` picks `ollama` or `openai` once at startup, and every request on both the Interactive (`:11435`) and Batch (`:11436`) lanes is proxied to that single upstream. A 2026-08-15 desktop inventory shows both lanes actually carry mixed traffic — chat, vision (`qwen3-vl:30b`, `qwen2.5vl:7b`), and embedding (`bge-m3`, `bge-m3-cpu`, `nomic-embed-text`) requests from `llm-gateway`, LightRAG, and `internal-scraper-service` — while a live vLLM instance on desktop (<broker-host>) serves exactly one chat model (`Qwen/Qwen2.5-3B-Instruct`) with no vision or embedding capability, and cannot host additional distinct base models on the same process/port the way Ollama does (each extra model needs its own vLLM process, and the desktop's single 17.1GB GPU — shared with gaming/Plex — has near-zero VRAM headroom beyond one resident vLLM instance). Today's global switch forces an all-or-nothing choice: moving to vLLM for its lower-latency chat serving would break every vision and embedding consumer on the same lane. The operator needs a way to route one specific high-value model (most likely `llm-gateway`'s interactive chat model) to vLLM while everything else keeps flowing to Ollama exactly as it does today, without requiring model parity between backends and without weakening any existing yield/unload safety guarantee (ADR-0003, ADR-0004, ADR-0014).

## Users / stakeholders

- **Operator** (Preston) — configures which model(s) route to which backend instance; needs the config to be safe-by-default and easy to reason about.
- **llm-gateway** (shared litellm proxy) — sends chat, vision, and embedding requests through the Interactive and Batch lanes; must keep working unchanged for every model it does not explicitly move to vLLM.
- **LightRAG** — sends embedding and chat requests; embedding traffic (`bge-m3`, `bge-m3-cpu`, `nomic-embed-text`) must never be silently routed to a backend that cannot serve it.
- **internal-scraper-service** (vision image scoring) — sends vision requests (`qwen3-vl:30b`, `qwen2.5vl:7b`) via the Batch lane; must keep working unchanged.
- **internal-monitor-app and other one-off CLI Consumers** — any Consumer speaking the Ollama API against either lane; unaffected unless the operator explicitly targets their model.
- **Gaming/Plex** — the highest-priority GPU claimant; the yield/preemption law protecting it (ADR-0003/ADR-0004) must hold across every configured backend instance, not just the default one.
- **The Broker's yield Controller / Unloader chain** (ADR-0014) — must continue to resolve unload/reload to a well-defined, race-free sequence per backend instance even when more than one backend instance is active at once, which is new territory (today the process has exactly one active backend).

## Functional requirements

1. The system shall, when no per-model/per-lane routing configuration is set, route 100% of Interactive and Batch traffic to whatever backend `UPSTREAM_BACKEND` already resolves to today, with behavior byte-for-byte identical to the current single-backend implementation (identical request/response bodies, identical error codes, identical `/healthz` and `/control` output shape).
2. The system shall allow the operator to designate one or more specific model names to route to an alternate, explicitly configured backend instance (URL, backend family, and any credentials configured independently of the default `UPSTREAM_BACKEND`/`UPSTREAM_URL`/`OLLAMA_URL`).
3. The system shall route a Synchronous request (through the Fronting Proxy) to the alternate backend instance if and only if the request's target model matches an operator-configured routing rule; all other requests shall continue to the default backend unchanged.
4. The system shall match a request's model name against routing rules using exact, case-sensitive string comparison against the full model identifier as sent by the Consumer (including any `:tag` suffix) — no normalization, case-folding, or tag-stripping is performed.
5. The system shall apply the same routing decision to the durable Job path (`POST /jobs` → `Backend.Generate`) as it applies to the Synchronous path, for a given model — an operator moving a model to vLLM must not have it silently exempted from Job routing.
6. The system shall apply routing rules identically regardless of which lane (Interactive `:11435` or Batch `:11436`) the request arrived on, unless the operator's configuration explicitly scopes a rule to one lane.
7. An unscoped routing rule (no lane restriction configured) applies identically to both the Interactive and Batch lanes.
8. The system shall apply a routing rule to the Job path (`POST /jobs`) whenever its model matches, regardless of the rule's lane scope — a lane restriction (`_LANE`) narrows which Synchronous-path lane triggers the rule, but never exempts the Job path from routing.
9. The system shall forward the model name to the routed backend unchanged (no rewriting, aliasing, or renaming of the model field in the request body).
10. The system shall NOT perform automatic capability detection (vision/embedding/chat) on a routed model; if the operator routes a model to a backend instance that cannot serve it, the system shall propagate that backend's own error response to the Consumer unchanged, rather than silently dropping, retrying against a different backend, or fabricating a response.
11. The system shall reject, at config-load time, a routing configuration that is structurally invalid (e.g. malformed URL, empty model name mapped to a non-empty backend target, or a single model name appearing in more than one routing rule) with a startup error, following the same fail-fast pattern `config.Load()` already uses for `OLLAMA_URL`/`UPSTREAM_URL` — a model may be routed to at most one backend target, full stop, regardless of lane scoping, even if the only difference between two rules for the same model is lane scope or the two rules point at the same target.
12. The system shall reject, at config-load time, a routing configuration containing a gap in the `BROKER_ROUTE_<N>_*` index sequence (e.g. index 3 configured while index 2 is unset) with a startup error — the parse loop must not silently stop at the first gap and treat subsequent indices as absent.
13. The system shall reject, at config-load time, a routing configuration where two configured backend instances (the default backend and/or any route) resolve to the same systemd unit name or the same backend URL — two independent Unloader instances managing the same underlying physical resource would reintroduce the exact out-of-order unload/reload race ADR-0014's ordering fix exists to prevent.
14. The system shall treat an unset or empty per-model routing configuration value as "routing disabled" (identical to the existing `UPSTREAM_UNIT_NAME` empty-disables-it precedent), never as an error.
15. The system shall resolve every configured backend instance (default and each alternate) to exactly one `yield.Unloader` per ADR-0014's typed-nil discipline — a `nil` capability shall be a direct, literal `nil` of the `yield.Unloader` interface type, never a typed-nil concrete pointer boxed into it.
16. The system shall, on a yield transition (contention start), invoke `Unload` on every backend instance's `Unloader` that is non-nil, as resolved once at startup during config load and backend construction — not re-evaluated per yield transition — not only the default backend's — before returning from the yield-start path.
17. The system shall, on a yield-clear transition, invoke `Reload` on every backend instance's `Unloader` that was unloaded, and shall preserve ADR-0014's per-instance ordering guarantee (`actionDone`/`startAction` chain) independently for each backend instance so a fast contention flap cannot race one instance's unload past another's reload or vice versa.
18. The system shall NOT allow a failure to unload/reload one backend instance to block, skip, or silently mask the unload/reload of another configured backend instance during the same yield transition.
19. The system shall continue to honor the existing gaming/Plex yield law (ADR-0003 hard-yield, ADR-0004 GPU scheduling policy) without exception for any request routed to any backend instance — no routed model, and no alternate backend instance, is exempt from yield.
20. The system shall expose the active per-model routing table (which models route to which backend instance) through the existing admin/control surface (specifically `GET /status`) so the operator can verify live configuration without reading environment variables on the host.
21. The system shall log, at startup, a summary of the resolved routing configuration (model → backend instance mapping) at `info` level, and shall never log `UPSTREAM_API_KEY` or any per-instance credential value.
22. The system shall continue to accept and correctly proxy Consumer requests using the Ollama HTTP API on every lane, regardless of which backend instance ultimately serves the request (Consumers never see backend-specific request/response formats).

## Non-functional requirements

- **Routing decision cost**: the per-request routing decision shall be a pure in-memory lookup against config loaded at startup — no additional network call, DNS lookup, or file read is introduced per request.
- **Config reload semantics**: routing configuration changes take effect only on broker restart, consistent with the existing `config.Load()` model (no hot-reload requirement is introduced by this feature).
- **Resource bound**: The system shall support a bounded, finite number of simultaneously configured alternate backend instances (a maximum of 16); `config.Load()` shall reject a routing configuration exceeding this bound with a startup error.
- **Credential handling**: any per-instance API key or credential follows the same non-logging, CR/LF-injection-rejecting discipline `UPSTREAM_API_KEY` already has (see `config.Load()`).
- **Backward compatibility**: existing `UPSTREAM_BACKEND`, `OLLAMA_URL`, `UPSTREAM_URL`, `UPSTREAM_API_KEY`, and `UPSTREAM_UNIT_NAME` env vars retain their current meaning and validation exactly; none are repurposed by this feature.

## Constraints

- Must integrate with the existing `internal/backend.Backend` interface (`Proxy`, `Generate`, `Reachable`, `Unloader`) and its single factory `backend.New(cfg)` — this feature extends how many `Backend` instances the process wires up and how requests are dispatched among them, not the interface itself.
- Must integrate with the existing `internal/yield.Controller`, which today assumes exactly one `Unloader` for the whole process (per `internal/yield/yield.go`); any change to support multiple concurrently active `Unloader`s must preserve the existing per-instance ordering/typed-nil discipline documented in ADR-0014.
- Must integrate with `internal/config`'s existing `Config`/`Load()` pattern (env-var driven, fail-fast on malformed values, empty-disables-it precedent from `UPSTREAM_UNIT_NAME`).
- Must not change the Consumer-facing API surface: both lanes continue to speak Ollama's own HTTP API unchanged (per README), regardless of which backend instance serves a given request.
- Must not assume vLLM (or any OpenAI-compatible upstream) can serve more than one distinct base model per process/port — the design must work when each alternate backend instance is a single-model process.
- Must not assume full feature/model parity between backend instances — an alternate backend instance may legitimately support a strict subset of what the default backend supports.
- Cannot weaken ADR-0003/ADR-0004 (gaming/Plex hard-yield, GPU scheduling policy) under any configuration.
- Cannot weaken ADR-0014's symmetric stop/start unload mechanism or its flap-ordering fix.
- No new external dependencies or third-party services are mandated by this feature; routing is Broker-internal config and dispatch logic only.

## Out of scope

- Automatic detection of a model's required capability (chat/vision/embedding) or automatic validation that a routed backend instance can actually serve a given model — the operator is responsible for correct routing configuration (FR-10 covers the failure path).
- Load balancing, weighted splitting, or A/B routing of a single model's traffic across multiple backend instances of the same kind.
- Automatic failover from an alternate backend instance to the default backend (or vice versa) on upstream error or unavailability.
- Hot/dynamic reload of routing configuration without a broker restart.
- Any UI or dashboard for editing routing configuration; configuration remains environment-variable/config-file driven, matching the rest of `internal/config`.
- Changes to the embed lane (`:11438`, Infinity/SigLIP, ADR-0008) — it already has its own independent upstream and is unaffected by this feature.
- Per-Consumer identity-based routing (routing by which Consumer sent the request, e.g. IP or auth token) — this feature routes by model (and optionally lane), not by Consumer identity, since the glossary's Consumer concept has no existing identity mechanism in the Fronting Proxy to key off of.
- Benchmarking or choosing which specific model(s) should move to vLLM — that is an operational decision made using this feature, not part of the feature itself.
- Provisioning or lifecycle-managing the vLLM process itself (systemd unit creation, model download, etc.) — out of scope; this feature only routes to an already-running upstream, the same way `UPSTREAM_URL` does today.
- Routing the same model to different backend targets differentiated only by lane (e.g., an interactive request for model M going to a fast instance while a batch request for the same model M goes elsewhere) — a natural extension of this design (the per-route `_LANE` field could support it), but deliberately deferred to a future iteration to keep the routing table's uniqueness rule simple: one model, one target, full stop, for now.

## Acceptance criteria

1. With no per-model routing configuration set, all Interactive and Batch requests are served by the backend `UPSTREAM_BACKEND` already resolves to, with identical request/response behavior to the pre-feature build (regression-tested against existing `backend_test.go`/`parity_test.go` coverage).
2. With a routing rule configured for model `M` pointing at an alternate backend instance, a request naming model `M` on either lane is served by the alternate instance, and a request naming any other model is served by the default backend.
3. A request naming a routed model `M` is served identically whether it arrives via the Synchronous Fronting Proxy or via `POST /jobs`.
4. A malformed routing configuration (invalid URL, empty model name with a non-empty target, or any model name appearing in more than one routing rule) causes `config.Load()` (or equivalent startup validation) to return an error and the process to fail to start — it does not start with partially-applied routing.
5. An empty/unset routing configuration value is accepted at startup with no error and resolves to "routing disabled," matching the `UPSTREAM_UNIT_NAME` precedent.
6. When gaming/Plex contention is detected while both a default and an alternate backend instance are configured, `Unload` is invoked on every non-nil `Unloader` across all configured instances before the yield-start path returns, verified by an integration test asserting call order and completion for ≥2 simultaneously configured instances.
7. When contention clears, `Reload` is invoked on every instance that was unloaded, and a rapid unload→reload→unload flap (mirroring the ADR-0014 flap-ordering test) does not race actions out of order for any individual instance, including when multiple instances are configured.
8. A backend instance whose `Unloader()` returns a typed-nil concrete pointer boxed into the `yield.Unloader` interface is caught by the same guard ADR-0014 already ships (or an equivalent), not treated as a valid non-nil `Unloader`, for every configured instance — not only the default one.
9. A request routed to a backend instance lacking the capability to serve it (e.g. an embedding-model name accidentally routed to the vLLM chat-only instance) returns that backend's own error response to the Consumer unchanged; the Broker does not crash, hang, or silently substitute another backend.
10. `GET /status` returns the current model → backend-instance routing table (as an optional `routing` key, present only when routes are configured), verifiable by inspecting the response against the loaded configuration.
11. Startup logs include a routing-configuration summary at `info` level and contain no raw API key or credential value, verified by grepping startup log output.
12. During active gaming/Plex contention, no request — regardless of which model or backend instance it targets — is admitted to any backend instance; this holds under the same test conditions ADR-0003/ADR-0004's existing hard-yield tests use, extended to cover a second configured backend instance.
13. Existing tests in `internal/config`, `internal/backend`, and `internal/yield` (including `parity_test.go` and any ADR-0014 flap-ordering test) continue to pass unmodified in their zero-routing-configured form, demonstrating no regression to today's single-backend behavior.
14. With zero routes configured, no routing logic reads, buffers, or inspects any request body on the Interactive or Batch lane — the pre-feature code path is used unchanged, verified by a test confirming no additional bytes are read for a large (e.g. multi-MB base64 vision) request body when routing is disabled.
