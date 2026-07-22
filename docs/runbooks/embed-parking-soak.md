# Chaos/Soak: Embed-Lane Request Parking (AC-14)

**Status**: Deploy-deferred runbook — runs at drain-end of ship-it, not before merge. Requires deployed Broker with parking enabled.

## Prerequisites

- Broker running with `BROKER_PARK_MAX_QUEUE=32` (or configured non-zero value), `BROKER_PARK_HOLD=600s`, `BROKER_PARK_DRAIN_BURST=8`.
- LightRAG instance configured against broker `:11436` Batch lane, `EMBEDDING_FUNC_MAX_ASYNC=10`.
- Prometheus scrape of `/metrics` running, retention `> 5m` (to capture pre/during/post-soak window).
- `BROKER_CONTROL_TOKEN` set (same token used to control `broker.service` via `POST /control` per ADR-0005).
- Local `curl` or equivalent tool to drive control requests.

## Procedure

### Phase 1: Establish baseline (1 min, no yield)

1. Start LightRAG embed job (e.g. via local `llm` CLI or API call, 50–100 documents, small batches to sustain traffic over 5+ minutes).
2. Observe `/metrics` endpoint: `broker_parked_depth` reads `0`, `broker_requests_total{outcome="served",class="batch"}` increments steadily (no parking activity yet).
3. Check LightRAG logs for zero `EMBEDDING_TIMEOUT` failures or connection resets — baseline ingest is healthy.

### Phase 2: Force yield and inject parking (2–3 min active)

1. **Start yield** (force contention):
   ```bash
   curl -X POST http://10.0.0.243:11437/control \
     -H "Authorization: Bearer $BROKER_CONTROL_TOKEN" \
     -d '{"mode":"yield"}'
   ```
   Broker transitions to yield state immediately (see `internal/yield/yield.go:applyLocked`). In-flight embed calls are preempted.

2. **Observe parking buildup** (15–30 sec window, yielding held):
   - LightRAG's next embed-burst requests arrive at broker `:11436` Batch gate.
   - `broker_parked_depth{class="batch"}` rises on `/metrics` — parked requests accumulate (expected: ramps toward 10–20, bounded by `BROKER_PARK_MAX_QUEUE=32`).
   - `broker_requests_total{class="batch",outcome="park_rejected"}` stays at or near 0 (park queue has headroom; rejection should be rare at `MAX_ASYNC=10` × 3x headroom).
   - **Critical**: LightRAG logs show zero ingest failures, zero `EMBEDDING_TIMEOUT` errors — embed calls are blocked in parking goroutines, not failing.

3. **End yield** (release parked requests):
   ```bash
   curl -X POST http://10.0.0.243:11437/control \
     -H "Authorization: Bearer $BROKER_CONTROL_TOKEN" \
     -d '{"mode":"auto"}'
   ```
   Broker transitions out of yield. `RunParkDrain` begins releasing parked requests (up to `BROKER_PARK_DRAIN_BURST=8` per 1s tick).

4. **Observe drain and replay** (30–60 sec window, post-yield):
   - `broker_parked_depth{class="batch"}` falls on each 1s drain tick (expected: 8 requests released per tick, full queue (~20 requests) drains in ~2–3 seconds).
   - Released requests re-enter slot-acquire and proceed to upstream (Ollama embed call).
   - `broker_requests_total{class="batch",outcome="served"}` increments as drained requests complete (durable confirmation: slot was acquired and upstream responded).
   - `broker_requests_total{class="batch",outcome="expired"}` should remain 0 (parked < 600s hold bound, well within window).
   - `broker_requests_total{class="batch",outcome="crash_failed"}` should remain 0 (no shutdown during soak).
   - **Critical**: LightRAG logs show zero ingest batch failures or partial-cancellations — all parked embed calls complete successfully after release.

## Observation Checklist

**During yield (Phase 2, step 2):**
- [ ] `broker_parked_depth` visible on `GET /metrics` and rising (no static `0`).
- [ ] LightRAG embed logs show calls blocking/pending, not failing.
- [ ] `corpus_embed_cascade` counter (LightRAG's own metric, if exposed) stays near 0 (no embed-layer failures cascading).
- [ ] No new errors in broker logs (no panic, no lock contention, no timeouts in park path itself).

**During drain (Phase 2, step 4):**
- [ ] `broker_parked_depth` falls in bursts of `≤ 8` per 1s tick (pacing enforcement).
- [ ] `broker_requests_total{outcome="served"}` increments by the count of drained requests (served count confirms slot acquisition + upstream response).
- [ ] LightRAG ingest batch completes without failure (zero cascading failures from the embed layer).
- [ ] No `expired` or `crash_failed` outcomes in counters (all parked requests released/served within hold bound, no shutdown).

**Post-soak (after Phase 2, step 4):**
- [ ] `broker_parked_depth` returns to 0 (no lingering parked requests, no ghost entries per FR-15).
- [ ] New embed jobs start cleanly (no degraded state, capacity fully recovered).
- [ ] Prometheus alert rule (if provisioned, e.g. `rate(broker_requests_total{outcome="expired"}[5m]) > 0`) does not fire (no expiry-based alert noise).

## Rollback

If parking misbehaves during the soak (e.g., parked requests stuck, `broker_parked_depth` doesn't fall, LightRAG failures occur):

1. **First-line rollback (no code change):**
   ```bash
   # Restart broker with parking disabled:
   BROKER_PARK_MAX_QUEUE=0 systemctl restart broker
   ```
   Broker reverts to today's immediate-503 behavior for Batch during yield. LightRAG ingest failures should resume (pre-parking baseline) to confirm the issue is parking-specific, not an unrelated system problem.

2. **If first-line doesn't stabilize**, revert commits in reverse order (Step 10 back to Step 1) per the Rollback plan in `docs/embed-lane-parking/steps.md`.

## Notes

- **Drain-end timing**: run this soak after all code changes (Step 1–10) are in place and the broker is built/deployed with parking wired (`RunParkDrain` goroutine live per Step 10). Do not run before merge.
- **LightRAG is the test oracle**: zero ingest failures during parking is the acceptance signal. Prometheus metrics confirm the Broker's own state; LightRAG logs confirm end-to-end success.
- **Scaling**: soak can be repeated at higher concurrency (e.g. `EMBEDDING_FUNC_MAX_ASYNC=20`, larger doc batches) to stress-test the ceiling and drain rate, but AC-14's baseline requirement is a single LightRAG instance at default settings.
- **Park-expiry alert**: if Prometheus alerting is provisioned on `rate(broker_requests_total{outcome="expired"}[5m]) > 0`, expect zero firings during the soak (parked < 600s < alert threshold). Post-soak silence confirms the alert rule itself works correctly if needed in the future.
