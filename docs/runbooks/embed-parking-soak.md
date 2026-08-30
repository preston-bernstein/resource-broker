# Chaos/soak test: embed-lane request parking (spec AC-14)

This is a chaos/soak test: a controlled test that forces a real failure condition (here, a Yield) and watches the system run under load for an extended time, to catch problems that only show up over time or under stress. It covers acceptance criterion AC-14 from the embed-lane-parking spec (`docs/embed-lane-parking/`).

**Status:** Deploy-deferred — this runbook runs after a change is deployed, at the drain phase of the `ship-it` delivery pipeline. Do not run it before a merge. It requires a deployed Broker with request parking turned on.

## Prerequisites

- The Broker is running with `BROKER_PARK_MAX_QUEUE=32` (or another non-zero value), `BROKER_PARK_HOLD=600s`, and `BROKER_PARK_DRAIN_BURST=8`.
- A LightRAG instance is configured to send requests to the Broker's `:11436` batch lane, with `EMBEDDING_FUNC_MAX_ASYNC=10`.
- Prometheus is scraping `/metrics`, with retention greater than 5 minutes (so it captures data from before, during, and after the soak window).
- `BROKER_CONTROL_TOKEN` is set — the same token used to control `broker.service` through `POST /control` per ADR-0005.
- `curl` (or an equivalent HTTP tool) is available to send control requests.

## Procedure

### Phase 1: establish a baseline (1 minute, no yield)

1. Start a LightRAG embedding job (for example, through the local `llm` CLI or an API call), with 50–100 documents in small batches, so traffic continues for at least 5 minutes.
2. Watch the `/metrics` endpoint: `broker_parked_depth` should read `0`, and `broker_requests_total{outcome="served",class="batch"}` should climb steadily — no parking activity yet.
3. Check the LightRAG logs for zero `EMBEDDING_TIMEOUT` failures or connection resets. This confirms the baseline ingest is healthy before the test begins.

### Phase 2: force a yield and inject parking (2–3 minutes active)

1. **Start a yield** (force Contention):
   ```bash
   curl -X POST http://<broker-host>:11437/control \
     -H "Authorization: Bearer $BROKER_CONTROL_TOKEN" \
     -d '{"mode":"yield"}'
   ```
   The Broker switches to yield state immediately (see `internal/yield/yield.go:applyLocked`). Any embedding calls already in flight are preempted.

2. **Watch parking build up** (a 15–30 second window, with yield still on):
   - LightRAG's next burst of embedding requests reaches the Broker's `:11436` batch gate.
   - `broker_parked_depth{class="batch"}` rises in `/metrics` as parked requests build up (expect it to ramp toward 10–20, capped by `BROKER_PARK_MAX_QUEUE=32`).
   - `broker_requests_total{class="batch",outcome="park_rejected"}` should stay at or near 0 — the parking queue has headroom (`MAX_ASYNC=10` against roughly 3x that much capacity), so rejections should be rare.
   - **Critical check:** the LightRAG logs show zero ingest failures and zero `EMBEDDING_TIMEOUT` errors. The embedding calls are waiting in parking, not failing.

3. **End the yield** (release the parked requests):
   ```bash
   curl -X POST http://<broker-host>:11437/control \
     -H "Authorization: Bearer $BROKER_CONTROL_TOKEN" \
     -d '{"mode":"auto"}'
   ```
   The Broker leaves yield state. `RunParkDrain` starts releasing parked requests, at up to `BROKER_PARK_DRAIN_BURST=8` per one-second tick.

4. **Watch the drain and replay** (a 30–60 second window, after the yield ends):
   - `broker_parked_depth{class="batch"}` falls on each one-second drain tick (expect 8 requests released per tick, so a full queue of about 20 requests drains in roughly 2–3 seconds).
   - Released requests re-enter the slot-acquire step and proceed to the upstream Ollama embedding call.
   - `broker_requests_total{class="batch",outcome="served"}` climbs as drained requests complete — durable confirmation that each one acquired a GPU slot and got a response.
   - `broker_requests_total{class="batch",outcome="expired"}` should stay at 0 (parked requests are well within the 600-second hold limit for this test window).
   - `broker_requests_total{class="batch",outcome="crash_failed"}` should stay at 0 (no shutdown happens during the soak).
   - **Critical check:** the LightRAG logs show zero ingest batch failures or partial cancellations — every parked embedding call completes successfully once released.

## Observation checklist

**During the yield (Phase 2, step 2):**
- [ ] `broker_parked_depth` is visible on `GET /metrics` and rising (not stuck at a static `0`).
- [ ] LightRAG's embedding logs show calls blocking or pending, not failing.
- [ ] The `corpus_embed_cascade` counter (LightRAG's own metric, if exposed) stays near 0 — no embedding-layer failures cascading elsewhere.
- [ ] No new errors appear in the Broker's own logs (no panic, no lock contention, no timeouts inside the parking code itself).

**During the drain (Phase 2, step 4):**
- [ ] `broker_parked_depth` falls in bursts of `≤ 8` per one-second tick (pacing is working as designed).
- [ ] `broker_requests_total{outcome="served"}` increases by exactly the number of drained requests (confirms each one acquired a slot and got a response from upstream).
- [ ] LightRAG's ingest batch completes with no failures — zero cascading failures from the embedding layer.
- [ ] No `expired` or `crash_failed` outcomes appear in the counters (every parked request was released or served within its hold limit, with no shutdown).

**After the soak (once Phase 2, step 4 is done):**
- [ ] `broker_parked_depth` returns to 0 — no lingering parked requests, no ghost entries (per requirement FR-15).
- [ ] New embedding jobs start cleanly — the Broker is not in a degraded state, and full capacity is available again.
- [ ] If a Prometheus alert rule is set up (for example `rate(broker_requests_total{outcome="expired"}[5m]) > 0`), it does not fire — there should be no expiry-related alert noise.

## Rollback

If parking misbehaves during the soak — for example, parked requests get stuck, `broker_parked_depth` never falls, or LightRAG reports failures — do this:

1. **First, try disabling parking without any code change:**
   ```bash
   # Restart the broker with parking disabled:
   BROKER_PARK_MAX_QUEUE=0 systemctl restart broker
   ```
   This reverts the Broker to its earlier behavior: an immediate `503` for batch requests during a yield, instead of parking them. If LightRAG's ingest failures come back at this point, that confirms the problem is specific to parking, not some unrelated system issue.

2. **If that does not stabilize things**, revert the commits in reverse order (from Step 10 back to Step 1), following the rollback plan in `docs/embed-lane-parking/steps.md`.

## Notes

- **When to run this:** only after all code changes (Steps 1–10) are in place and the Broker is built and deployed with parking wired in (the `RunParkDrain` goroutine live, from Step 10). Do not run this before a merge.
- **LightRAG is the source of truth for this test.** Zero ingest failures during parking is what counts as a pass. Prometheus metrics confirm the Broker's own internal state; the LightRAG logs confirm the request actually succeeded end to end.
- **Scaling the test:** you can repeat this soak at higher concurrency (for example `EMBEDDING_FUNC_MAX_ASYNC=20`, larger document batches) to stress-test the queue's ceiling and drain rate. The baseline requirement for AC-14 is just a single LightRAG instance at its default settings.
- **Park-expiry alert:** if Prometheus alerting is set up for `rate(broker_requests_total{outcome="expired"}[5m]) > 0`, expect it not to fire during the soak (parked requests stay under 600 seconds, below the alert threshold). Silence after the soak confirms the alert rule itself would work if it were ever needed.
