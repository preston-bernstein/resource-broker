# Routes displace yield units

**Status: accepted; implemented in `internal/yield/displace.go` (`ConfigureDisplace`, `reconcileDisplacedLocked`, `stopDisplacedLocked`), `internal/config/config.go` (`RouteBackend.Displaces`, `checkDisplaces`, `BROKER_ROUTE_<N>_DISPLACES`), `cmd/broker/main.go` (`buildBroker`).**

ADR-0019 left one question open: whether yield units compete with the Broker's own backends for the GPU in practice. They do. On 2026-11-08 the AudioMuse-AI GPU worker (a yield unit) began running around the clock and holding about 4 GiB of the 16 GiB card. vLLM, the interactive route's backend, then failed every on-demand start because the free VRAM fell short of its startup check. Trimming vLLM to its real footprint (about 8.4 GiB) made one GPU worker fit beside it. A second GPU worker would not, and vLLM's traffic is a few requests a day, so the card sat a quarter idle to keep room for a backend that is unloaded almost all the time.

**Decision: `BROKER_ROUTE_<N>_DISPLACES` lists yield units that give that route the GPU.** A displaced unit runs only while the Broker is not yielding and every route that displaces it is idle-unloaded. A request that wakes the route (TrackActivity) stops its displaced units first, and the route's reload waits for those stops to finish. When the route idle-unloads, its units start again, after the route's stop has finished. So the two never hold VRAM at once. A Broker yield (gaming, Plex) still stops everything, as before. On yield-clear a displaced unit restarts only if its displacers are idle.

A displacing route must have an idle timeout, or its first request would keep the units off forever. A displaced unit must be a `BROKER_YIELD_UNITS` entry, since only those have a controller of their own. `Load()` rejects both mistakes.

**At start the Broker does not know whether a displacing route's backend is running**, so it stops it once and treats it as idle-unloaded. Its next request wakes it as usual. Displaced units start only after `BROKER_YIELD_CONFIRM_POLLS` detector ticks, so a Broker started during a game does not start a unit just to stop it.

**Accepted cost: the waking request waits longer.** It already races a cold start (ADR-0014). Now it also waits for the displaced unit to stop, a few seconds for a container. And like any yield, stopping the unit loses the work in flight (ADR-0019).

**Rejected: a GPU lease API**, for the reasons ADR-0019 gives. Displacement reuses the idle-unload and wake paths that are already in production, and needs no client inside the workload.

**Not covered: the default Ollama backend.** It loads and evicts models itself and has no unit to stop. When it loads a model while a displaced unit holds VRAM, Ollama puts part of it on the CPU. That is slower, but it works.
