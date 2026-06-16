# Ollama Resource Broker

Single-GPU arbitration for a home PC shared by gaming, Plex transcoding, and Ollama inference. Gaming/Plex take absolute priority; inference work is queued, preempted, and resumed around them.

## Language

**Broker**:
The arbiter sitting between inference callers and Ollama. Decides whether a request runs now, queues, or is preempted, based on current contention. Single source of truth for GPU access.
_Avoid_: Manager, Orchestrator, Gateway

**Contention**:
A high-priority claim on the GPU — gaming or Plex transcoding — detected by process name. While contention is present the Broker yields the GPU entirely; inference does not run.
_Avoid_: Load, Pressure, Busy

**Yield**:
The Broker's response to contention: stop/admit no inference work and let gaming/Plex have 100% of the GPU. The opposite of yielding is serving the queue.
_Avoid_: Throttle, Pause, Preempt (preempt is the act on one job; yield is the whole-Broker state)

**Preemption**:
Interrupting a running, lower-priority request so a higher-priority claimant gets the GPU. Two triggers: gaming/Plex contention preempts all inference; an interactive request preempts a running batch request. A preempted request returns 503 and its caller retries (batch via PENDING). Priority order: gaming/Plex > interactive > batch.
_Avoid_: Kill, Cancel

**Job**:
A durable unit of long-running batch inference work submitted to the Broker, identified by an id, with a lifecycle (queued → running → succeeded | failed | canceled) and observable status — Position while queued, live progress (tokens, elapsed) while running. Survives Broker restart. Carries a `source` (submitting consumer) and optional `owner` (e.g. a profile id) so status views can be scoped. Distinct from a Synchronous request, which streams through the Fronting Proxy and is not persisted.
_Avoid_: Task, Request

**Queue**:
The durable, ordered line of Jobs awaiting the GPU. Drained in priority order when the Broker is not yielding; paused as a whole while the Broker yields to gaming. A preempted Job returns to the front.
_Avoid_: Backlog, Buffer

**Position**:
A Job's 1-based place among the batch Jobs that will run before it — deterministic within the batch line. Reported while a Job is queued; pairs with a clearly-soft ETA, never a hard wait guarantee (interactive bursts and gaming move the line unpredictably).
_Avoid_: Rank, Slot, Place

**Fronting Proxy**:
The Broker's synchronous HTTP entry point: it speaks Ollama's own API, applies yield/priority, then forwards to real Ollama, streaming the response live. Serves every interactive request and short/cheap batch calls (e.g. embeddings) — callers repoint their Ollama host and need no other change. Long-running batch work uses the Job path instead. The superseded Bash CLI wrapper in `legacy/` is reference/history only — it shares no state with the Broker and must not be run alongside it.
_Avoid_: Gateway, Reverse Proxy (generic), Shim

**Synchronous request**:
Inference handled live through the Fronting Proxy — admitted (or 503'd) immediately, streamed back, never persisted. Used for all interactive work and short batch calls. The counterpart to a Job; the choice of mode is independent of priority Class.
_Avoid_: Sync call, Passthrough

**Consumer**:
Any service that sends inference requests through the Broker — fashion-monitor pipeline, LightRAG (RAG + embeddings), estate-scraper vision, ad-hoc CLI jobs.
_Avoid_: Client, Caller, User
