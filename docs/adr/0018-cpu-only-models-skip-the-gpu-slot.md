# CPU-only models skip the GPU slot

**Status: accepted; implemented in `internal/queue/gate.go` (`serveCPU`), `internal/queue/scheduler.go` (`SetCPUModels`, `isCPUModel`), `internal/httpx/peek.go` (`RequestModel`, shared with ADR-0015's Router), and `BROKER_CPU_MODELS`.**

The Broker admits every Synchronous request through one scheduler slot pool (ADR-0004), sized by `BROKER_MAX_INFLIGHT` (4 in production). The pool models the GPU. A model built with `num_gpu 0` never touches the GPU, but its requests still took a slot.

The cost was measured on 2026-09-29. `lightrag-trading` embeds through `llm-gateway` with `bge-m3-cpu` (bge-m3 with `num_gpu 0`, VRAM 0) at `EMBEDDING_FUNC_MAX_ASYNC=4`. Four CPU embeds held all 4 slots. Each waited 80–180s and one hit litellm's 300s timeout. The GPU sat at 0% busy with no game and no Plex session. Every interactive request behind them got `503 GPU busy: wait budget exceeded`, including a coding agent's chat call that never started. The same model embeds in about 0.12s when it bypasses the Broker (algo-corpus notes, 2026-07-21), so the wait was all queueing.

**Decision: a request whose `model` is in `BROKER_CPU_MODELS` is served with no slot and no Yield check.** `Gate` peeks the body's `model` field with the same bounded, body-restoring peek ADR-0015's Router uses (now `httpx.RequestModel`). A match with or without Ollama's `:latest` tag skips admission entirely. The client's disconnect still cancels the call, and a lane's `upstreamTimeout` still bounds it. The body is peeked only when the list is non-empty, so an unset `BROKER_CPU_MODELS` is the exact old path.

**The yield-to-gaming invariant is not weakened.** ADR-0004's law is about what the Broker owes a real GPU claim. A CPU model makes no GPU claim, so it has nothing to yield. This is the "refining what counts as GPU work" direction, like ADR-0017 refined what counts as Contention. The list is explicit and operator-owned. A GPU model named in it by mistake would run during a game, so the unit comment names what each entry is and why it is CPU-only.

**Clients are trusted.** The peek reads the first top-level `model` key; Ollama's decoder keeps the last. A client that sends `model` twice could name a CPU model to skip the slot and still load a GPU model. The Broker listens on the LAN for home-lab services, not untrusted callers, and scanning the whole body to reject duplicate keys would defeat the 64KB peek bound (ADR-0015). If the Broker is ever exposed beyond trusted consumers, this needs revisiting.

**Scope: Synchronous requests only.** The durable Job worker (`internal/job/worker.go`) still acquires a slot for every Job, whatever the model. No current consumer submits CPU-model Jobs; extending the bypass there is a separate change.

**Rejected: route the CPU model around the Broker in `llm-gateway` (straight to Ollama `:11434`).** This was the 2026-07-21 fix and it worked, but it breaks the house rule that all inference goes through the Broker, and it fixes one consumer only. The next CPU model would hit the same wall.

**Rejected: detect CPU-only models automatically from Ollama (`/api/show`, `num_gpu 0`).** It needs a call to Ollama per model, a cache, and a rule for models that load partly on GPU. A wrong guess runs GPU work during a game, the one outcome ADR-0003 forbids. An explicit list fails toward the old behavior: an unlisted model just waits for a slot.

**Rejected: raise `BROKER_MAX_INFLIGHT`.** More slots would let GPU requests run concurrently too. The slot count exists to bound GPU concurrency, and CPU work does not belong in that count at all.

**Links**: ADR-0004 (GPU scheduling policy — the pool this bypasses), ADR-0015 (per-model routing — the shared body peek), ADR-0017 (the same "refine the classification" direction), ADR-0003 (why a wrong entry in the list is dangerous).
