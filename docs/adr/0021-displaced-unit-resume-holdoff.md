# Displaced units wait out a resume hold-off

**Status: accepted; implemented in `internal/yield/displace.go` (`SetUnitResumeHoldoff`, `reconcileDisplacedLocked`), `internal/config/config.go` (`UnitResumeHoldoff`, `BROKER_YIELD_UNIT_RESUME_HOLDOFF`), `cmd/broker/main.go` (`buildBroker`).**

ADR-0012 never debounces a yield clear, because recovering faster only helps inference. That holds for the Broker's own backends, where a reload is cheap. It does not hold for a displaced unit (ADR-0020). On 2026-11-09 between 12:00 and 13:00 a Plex session yielded and cleared 26 times, for 30-80 s each. Each clear started the AudioMuse-AI GPU worker, and the next yield stopped it about 6 s later, while it was still setting up its GPU sessions. The night before, stopping that worker mid-setup had wedged ROCm compute host-wide.

**Decision: `BROKER_YIELD_UNIT_RESUME_HOLDOFF` sets how long a displaced unit's GPU must stay free before the unit starts.** Free means the Broker is not yielding and every displacer is idle-unloaded. Anything that takes the GPU back resets the clock. Stops are never held off: a yield or a wake stops the unit at once, as before. The default is 0, which keeps the old behaviour. The desktop runs 5m.

**Accepted cost:** the unit stays idle for up to the hold-off after every yield, wake or broker restart. A Plex session in a flapping pattern keeps it off for the whole session.

**Not covered:** plain yield units (with no displacer) still restart on the first clear in `applyLocked`. None run today.
