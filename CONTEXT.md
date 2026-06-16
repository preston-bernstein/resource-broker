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
Interrupting an in-flight inference job when contention appears, returning it to the front of the queue to resume once contention clears.
_Avoid_: Kill, Cancel

**Queue**:
Ordered set of pending inference requests. Preempted (interrupted) work outranks newly arrived work. Drained in priority order when the Broker is not yielding.
_Avoid_: Backlog, Buffer

**Fronting Proxy**:
The Broker's HTTP entry point: it speaks Ollama's own API on its own port, applies queue/yield/priority, then forwards to real Ollama. Callers (internal-monitor-app, LightRAG, internal-scraper-service) repoint their Ollama host at the Broker and need no other change. Distinct from the legacy CLI batch-job wrapper, which only governs commands explicitly run through it.
_Avoid_: Gateway, Reverse Proxy (generic), Shim

**Consumer**:
Any service that sends inference requests through the Broker — internal-monitor-app pipeline, LightRAG (RAG + embeddings), internal-scraper-service vision, ad-hoc CLI jobs.
_Avoid_: Client, Caller, User
