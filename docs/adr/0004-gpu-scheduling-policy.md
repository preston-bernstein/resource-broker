# GPU scheduling: configurable concurrency, tiered preemption, batch quantum

**Status: accepted (audit 2026-06-16) — supersedes the implicit "concurrency-1, gaming-only preemption" model. Code change pending.**

The broker's load-bearing invariant is **yield the GPU to gaming/Plex**, not "one inference at a time." The original design conflated the two: it hard-serialized inference to a single in-flight request always, which underutilizes an idle GPU (Ollama does its own parallelism/multi-model loading), thrashes models, and — worst — let a long batch call block a latency-sensitive interactive request, since interactive only queue-jumped and never preempted. This ADR sets the policy:

1. **Max in-flight is configurable** (`BROKER_MAX_INFLIGHT`, default 1 to preserve current behavior). Concurrency-1 is a conservative default, not a law; the only law is yielding to gaming.
2. **Tiered preemption**, priority `gaming/Plex > interactive > batch`: contention preempts all inference; an interactive request preempts a running batch request; batch preempts nothing. Preempted requests return 503 and callers retry (batch via internal-monitor-app PENDING). This reuses the existing serve-context cancellation.
3. **Batch min-run quantum** (`BROKER_BATCH_QUANTUM`, default ~10s): interactive may preempt a batch request only within its first quantum; past that the batch runs to completion. This bounds interactive added-latency to ≈quantum, guarantees batch *progress* (no starvation under steady interactive load), and prevents reload thrash from interactive bursts.

Alternatives rejected: pure always-on concurrency-1 (the costs above); dropping the broker's concurrency control entirely and trusting Ollama's FIFO (loses cross-consumer priority); strict fairness/aging (more machinery than a single-GPU home box needs).
