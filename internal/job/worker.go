package job

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
)

// Gate is the scheduler surface the worker needs: a batch slot, plus the signal
// and depth that drive interactive preemption (ADR-0004).
type Gate interface {
	Acquire(ctx context.Context, class queue.Class) error
	Release()
	InteractiveWaiting() <-chan struct{}
	Stats() queue.Stats
}

// Yielder reports whether inference may run and exposes the serve context that
// is cancelled the instant the broker starts yielding to gaming/Plex.
type Yielder interface {
	Yielding() (bool, string)
	ServeContext() context.Context
}

// Generator runs one inference, streaming token-count progress via onTokens.
type Generator interface {
	Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error)
}

// Worker drains the durable Job queue one Job at a time through the shared
// single-GPU gate. A Job is the batch class: gaming/Plex preempts it instantly;
// an interactive request preempts it once it has run past the min-run quantum.
// A preempted Job requeues at the front (resume-first), a failed one retries up
// to maxAttempts, and a cancelled one stops.
type Worker struct {
	svc     *Service
	gate    Gate
	yld     Yielder
	gen     Generator
	quantum time.Duration
	poll    time.Duration
}

// NewWorker builds a Worker. quantum is the batch min-run window before
// interactive may preempt (ADR-0004); poll is how often the loop re-checks for
// work while idle or yielding.
func NewWorker(svc *Service, gate Gate, yld Yielder, gen Generator, quantum, poll time.Duration) *Worker {
	if quantum <= 0 {
		quantum = 10 * time.Second
	}
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	return &Worker{svc: svc, gate: gate, yld: yld, gen: gen, quantum: quantum, poll: poll}
}

// Run drains the queue until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		// Hold the line while yielding to gaming/Plex — don't claim work the GPU
		// can't run.
		if yielding, _ := w.yld.Yielding(); yielding {
			if !w.sleep(ctx) {
				return
			}
			continue
		}
		// Cheap peek so we don't take a GPU slot just to find the queue empty.
		has, err := w.svc.store.HasQueued(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("job peek", "err", err)
		}
		if err != nil || !has {
			if !w.sleep(ctx) {
				return
			}
			continue
		}
		// Acquire the slot first, then claim — so a Job only becomes RUNNING once
		// it actually holds the GPU, never while merely waiting in line.
		if !w.runNext(ctx) {
			if !w.sleep(ctx) {
				return
			}
		}
	}
}

func (w *Worker) sleep(ctx context.Context) bool {
	t := time.NewTimer(w.poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runNext acquires a GPU slot and runs one Job. It returns false when there was
// nothing to do (empty queue or yielding) so the caller can back off; true once
// a Job was attempted.
func (w *Worker) runNext(ctx context.Context) bool {
	// Acquire the batch slot before claiming. On failure (shutdown) there's
	// nothing claimed to requeue.
	if err := w.gate.Acquire(ctx, queue.Batch); err != nil {
		return false
	}
	defer w.gate.Release()

	// Yielding may have begun while we waited for the slot; don't touch the GPU.
	if yielding, _ := w.yld.Yielding(); yielding {
		return false
	}

	j, err := w.svc.store.ClaimNext(ctx)
	if err == ErrNotFound {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("job claim", "err", err)
		}
		return false
	}
	w.runJob(ctx, j)
	return true
}

func (w *Worker) runJob(ctx context.Context, j *Job) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	rs := w.svc.startRun(j.ID, cancel)
	defer w.svc.endRun(j.ID)

	var preempted atomic.Bool
	serveDone := w.yld.ServeContext().Done()
	go w.monitor(jobCtx, serveDone, w.svc.now(), cancel, &preempted)

	opts := decodeOptions(j.ParamsJSON)
	text, genErr := w.gen.Generate(jobCtx, j.Model, j.Prompt, opts, func(n int) {
		w.svc.reportTokens(j.ID, rs, n)
	})

	// Terminal writes must survive worker-loop shutdown, so use a fresh context.
	bg, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bgCancel()

	run := w.svc.now().Sub(rs.startedAt)

	if genErr == nil {
		if err := w.svc.store.Succeed(bg, j.ID, text); err != nil {
			slog.Warn("job succeed", "id", j.ID, "err", err)
			return
		}
		w.svc.publishTerminal(j.ID, StateSucceeded)
		w.svc.recordJob("succeeded", run)
		slog.Info("job done", "id", j.ID, "state", StateSucceeded)
		return
	}

	// The generation aborted. Classify why, preferring "requeue" causes over
	// "fail" so transient interruptions never burn an attempt.
	switch {
	case w.svc.isCanceled(bg, j.ID):
		// Explicit Cancel already set CANCELED and published the terminal event.
		slog.Info("job canceled", "id", j.ID)
	case ctx.Err() != nil:
		w.requeue(j.ID) // broker shutting down
	case isClosed(serveDone):
		w.requeue(j.ID) // gaming/Plex took the GPU
		w.svc.recordJob("preempted", run)
		slog.Info("job preempted", "id", j.ID, "by", "gaming")
	case preempted.Load():
		w.requeue(j.ID) // interactive request preempted past quantum
		w.svc.recordJob("preempted", run)
		slog.Info("job preempted", "id", j.ID, "by", "interactive")
	default:
		failed, err := w.svc.store.FailOrRetry(bg, j.ID, genErr.Error(), w.svc.maxAttempts)
		if err != nil {
			slog.Warn("job fail", "id", j.ID, "err", err)
			return
		}
		if failed {
			w.svc.publishTerminal(j.ID, StateFailed)
			w.svc.recordJob("failed", run)
			slog.Info("job failed", "id", j.ID, "err", genErr)
		} else {
			w.svc.bus.publish(j.ID, Event{Type: EventState, State: StateQueued})
			w.svc.recordJob("retried", run)
			slog.Info("job retry", "id", j.ID, "err", genErr)
		}
	}
}

// monitor cancels the job context when the GPU must be surrendered: immediately
// on gaming/Plex (serveDone), and — only after the min-run quantum — when an
// interactive request is waiting. The ticker re-checks so a long-parked
// interactive request is noticed even without a fresh wake-up.
func (w *Worker) monitor(jobCtx context.Context, serveDone <-chan struct{}, start time.Time, cancel context.CancelFunc, preempted *atomic.Bool) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-jobCtx.Done():
			return
		case <-serveDone:
			cancel()
			return
		case <-w.gate.InteractiveWaiting():
			if w.shouldPreempt(start) {
				preempted.Store(true)
				cancel()
				return
			}
		case <-ticker.C:
			if w.shouldPreempt(start) {
				preempted.Store(true)
				cancel()
				return
			}
		}
	}
}

func (w *Worker) shouldPreempt(start time.Time) bool {
	return w.svc.now().Sub(start) >= w.quantum && w.gate.Stats().Interactive > 0
}

func (w *Worker) requeue(id string) {
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.svc.store.Preempt(bg, id); err != nil {
		slog.Warn("job requeue", "id", id, "err", err)
		return
	}
	w.svc.bus.publish(id, Event{Type: EventState, State: StateQueued})
}

func decodeOptions(paramsJSON string) map[string]any {
	if paramsJSON == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(paramsJSON), &m); err != nil {
		return nil
	}
	return m
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
