# Resource Broker

The Resource Broker (called "the Broker" throughout this repo) controls who gets the GPU on a home PC that gaming, Plex video transcoding, and Ollama inference all share. Gaming and Plex always win the GPU; the Broker queues, pauses, and resumes inference work around them.

This file is the glossary for this repo. Every term below has exactly one meaning here — using a different word for the same concept, or the same word for a different concept, causes real confusion in code, docs, and conversation. Each term's *Avoid* list names close-sounding words people might reach for instead; don't use those instead, because they either mean something more general or blur two ideas this repo needs kept separate.

## Terms

**Broker**:
The single program that decides which inference request gets the GPU, and when. For each request it decides whether to run it now, put it in line, or preempt it (interrupt it for something higher-priority) — based on whether gaming or Plex currently needs the GPU (see Contention, below). It is the one place all GPU-access decisions get made.
_Avoid_: Manager, Orchestrator, Gateway

**Contention**:
A high-priority claim on the GPU: gaming, or Plex video transcoding. The Broker detects it by matching the process name. While Contention is present, the Broker yields the GPU completely — no inference runs.
_Avoid_: Load, Pressure, Busy

**Yield**:
The state the whole Broker enters in response to Contention: it stops admitting any inference work, so gaming or Plex gets 100% of the GPU. The opposite of Yield is serving the queue — running inference normally.
_Avoid_: Throttle, Pause, Preempt (Preempt is the act on one request; Yield is the whole-Broker state)

**Idle**:
The condition of one configured backend instance (the default backend, or one `BROKER_ROUTE_<N>` instance) having received no Synchronous request dispatched to it for at least that instance's own configured idle duration. Idle is per-instance traffic history, not a Broker-wide state — it says nothing about any other instance and nothing about whether gaming/Plex Contention is present. Don't confuse it with Yield: Yield is the whole-Broker response to Contention; Idle is one instance quietly going unused.
_Avoid_: Yield (Yield is whole-Broker and Contention-driven; Idle is per-instance and traffic-driven), Sleep, Suspend

**Idle-unload**:
The act of freeing an Idle instance's VRAM — and the symmetric act of bringing it back on its next request — driven through the exact same `systemctl stop`/`start` `yield.Unloader` mechanism Contention-triggered Yield already uses for that instance. Idle-unload is a second, independent event source feeding the same per-instance `yield.Controller` action chain Contention drives; it is not a new state, a new controller, or a new unload mechanism, and it never overrides or delays Contention's own response.
_Avoid_: Yield-unload (there is no such state — Yield and Idle are separate triggers into one mechanism), Unload (alone; too generic — Unload is the general `systemctl stop` mechanism itself, Idle-unload is one specific trigger for it, Contention is the other)

**Preemption**:
Interrupting a request that is already running so a higher-priority one can get the GPU instead. Two things trigger it: gaming/Plex Contention preempts all inference; an interactive request preempts a running batch request. A preempted request gets a `503` response, and its caller must retry it (a batch request goes to PENDING to be retried later). Priority order, highest first: gaming/Plex, then interactive, then batch.
_Avoid_: Kill, Cancel

**Job**:
A unit of long-running batch inference work that a Consumer submits to the Broker. Each Job has an id and moves through a lifecycle: queued, then running, then succeeded, failed, or canceled. While queued, its status shows its Position in line; while running, it shows live progress (tokens generated, time elapsed). A Job survives a Broker restart. It carries a `source` field (which Consumer submitted it) and an optional `owner` field (for example, a profile id), so status views can be scoped to one Consumer or owner. A Job differs from a Synchronous request, which streams through the Fronting Proxy and is never saved.
_Avoid_: Task, Request

**Queue**:
The ordered, durable line of Jobs waiting for the GPU. The Broker drains it in priority order whenever it is not yielding to gaming; the whole Queue pauses while the Broker yields. A preempted Job goes back to the front of the Queue.
_Avoid_: Backlog, Buffer

**Park** / **Parked request**:
A short-lived, in-memory holding state for Batch Synchronous requests that arrive while the Broker is yielding. A parked request waits until Yield ends, then is released in the order it arrived (first in, first out) to take its turn for the GPU. Parking differs from the Queue: a parked request is not a Job — it is not saved to disk — and parking is off by default (`BROKER_PARK_MAX_QUEUE=0` turns it off). Three hard limits apply: a maximum number of parked requests at once, a maximum time any one request may stay parked, and immediate failure for parked requests if the Broker shuts down.
_Avoid_: Hold, Buffer, Suspend, Queue

**Drain burst**:
How the Broker releases parked requests when it switches from yielding back to serving: it releases `BROKER_PARK_DRAIN_BURST` parked requests every polling tick (once per second). A large backlog of parked requests is released gradually, tick by tick, instead of all at once — this avoids a sudden spike in GPU load and spreads requests out.
_Avoid_: Flush, Replay-all, Burst (alone)

**Position**:
A Job's place in line among the batch Jobs that will run before it, counted from 1. It is reported only while the Job is queued. Position comes with a soft estimated wait time, never a guaranteed one — interactive traffic and gaming can move the line unpredictably.
_Avoid_: Rank, Slot, Place

**Fronting Proxy**:
The Broker's entry point for live (Synchronous) requests. It speaks Ollama's own HTTP API, applies Yield and priority rules, then forwards the request to the real Ollama and streams the response back live. It serves every interactive request plus short, cheap batch calls such as embeddings — a Consumer needs no code change beyond pointing at the Broker instead of Ollama. Long-running batch work uses the Job path instead. The Bash command-line tool this replaced lives in `legacy/` for reference only; it shares no state with the Broker and must never run alongside it.
_Avoid_: Gateway, Reverse Proxy (generic), Shim

**Synchronous request**:
Inference handled live through the Fronting Proxy: it is admitted immediately or rejected with `503`, its response streams back, and it is never saved. Used for all interactive work and short batch calls. It is the counterpart to a Job — whether a request is Synchronous or a Job is independent of its priority Class (interactive vs. batch).
_Avoid_: Sync call, Passthrough

**Embed lane**:
An optional second upstream server (see ADR-0008): an Infinity SigLIP image-embedding server, fronted on its own port (`:11438`), built for the internal-scraper-service image corpus (internal-scraper-service's collection of listing photos). It runs on the CPU, so it has its own scheduler and does not compete for the GPU slot — but it still shares the Broker's Yield controller, so it backs off during Contention like everything else. It presents an OpenAI-style `/embeddings` endpoint and rewrites each request to Infinity's own `/embeddings_image` endpoint. It only starts if the `INFINITY_URL` environment variable is set.
_Avoid_: Embedding proxy, CPU broker

**Consumer**:
Any service that sends inference requests through the Broker. Examples: the internal-monitor-app pipeline, LightRAG (for retrieval-augmented generation and embeddings), internal-scraper-service's vision step, and one-off command-line jobs.
_Avoid_: Client, Caller, User
