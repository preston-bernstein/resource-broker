# Yield units for GPU workloads the Broker does not front

**Status: accepted; implemented in `internal/config/config.go` (`YieldUnits`, `parseYieldUnits`, `BROKER_YIELD_UNITS`), `internal/backend/openai_backend.go` (`NewUnitUnloader`), `cmd/broker/main.go` (`buildBroker`).**

The Broker's law is to yield the GPU to gaming and Plex (ADR-0003, ADR-0004). It can only enforce that on GPU work it can reach: the default backend, through Ollama's unload or `UPSTREAM_UNIT_NAME`, and each route, through `BROKER_ROUTE_<N>_UNIT_NAME` (ADR-0014, ADR-0015). Some GPU work on the box is not inference behind an HTTP endpoint the Broker fronts. The first case is AudioMuse-AI's ROCm worker, which runs music-analysis models (ONNX Runtime on MIGraphX) from a job queue of its own. Nothing tells it a game started, so it keeps the GPU busy and its models resident through the game.

**Decision: `BROKER_YIELD_UNITS` lists extra systemd units. The Broker stops each one on yield-start and starts it on yield-clear.** Each entry gets the same `systemdUnitController` ADR-0014 built for vLLM, through a new exported `backend.NewUnitUnloader`. The entries go after the default backend and every route in the controller's unloader list, so existing orig indexes keep their meaning, and the `actionDone` chain orders their stop/start across a flap exactly as it does for vLLM. They never idle-unload: their idle timeout is always 0, because idle-unload is driven by Synchronous traffic and these units get none.

A unit may have only one controller. `Load()` rejects a repeat in the list and a unit that `UPSTREAM_UNIT_NAME` or a route already stops and starts, because two controllers would race each other on every yield. An empty or unset `BROKER_YIELD_UNITS` is the exact old wiring.

**Rejected: a GPU lease API.** (Revisited in ADR-0020, which answers the competition case with displacement.) A workload could instead ask the Broker for a batch-class lease, so interactive inference preempts it too (ADR-0004's tiers). That needs a new endpoint, renewal and revocation, and a client inside each workload. The problem in hand is only the gaming/Plex invariant, which this solves with the mechanism already in production. A lease stays open if workloads turn out to compete with Ollama for the GPU in practice.

**Accepted cost: work in flight is lost on each yield.** Stopping the unit kills whatever the workload was doing. The workload must tolerate that. AudioMuse-AI's queue requeues the interrupted album, and its CPU workers keep going while the GPU worker is down.

**Security scope** is the same as ADR-0014's: the Broker's service account needs a polkit grant for exactly `start` and `stop` on exactly each listed unit, never a wildcard. The unit names come from operator-set config with no validation beyond trimming, so a wider grant would let that config stop or start anything the grant covers.
