package job

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/inference"
	"github.com/preston-bernstein/resource-broker/internal/queue"
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

// Generator runs one inference, streaming token-count progress via req.OnTokens.
type Generator interface {
	Generate(ctx context.Context, req inference.Request) (string, error)
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

// WorkerConfig is everything NewWorker needs. Quantum is the batch min-run
// window before interactive may preempt (ADR-0004); Poll is how often the loop
// re-checks for work while idle or yielding. A zero Quantum or Poll takes its
// default.
type WorkerConfig struct {
	Service   *Service
	Gate      Gate
	Yielder   Yielder
	Generator Generator
	Quantum   time.Duration
	Poll      time.Duration
}

// NewWorker builds a Worker from cfg.
func NewWorker(cfg WorkerConfig) *Worker {
	if cfg.Quantum <= 0 {
		cfg.Quantum = 10 * time.Second
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 500 * time.Millisecond
	}
	return &Worker{svc: cfg.Service, gate: cfg.Gate, yld: cfg.Yielder, gen: cfg.Generator, quantum: cfg.Quantum, poll: cfg.Poll}
}

// Run drains the queue until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if w.tryRunNext(ctx) {
			continue
		}
		if !w.sleep(ctx) {
			return
		}
	}
}

// tryRunNext runs one Job if the GPU is free and one is queued. It returns
// false when the caller should back off.
func (w *Worker) tryRunNext(ctx context.Context) bool {
	// Hold the line while yielding to gaming/Plex — don't claim work the GPU
	// can't run.
	if yielding, _ := w.yld.Yielding(); yielding {
		return false
	}
	// Cheap peek so we don't take a GPU slot just to find the queue empty.
	has, err := w.svc.store.HasQueued(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("job peek", "err", err)
		}
		return false
	}
	// Acquire the slot first, then claim — so a Job only becomes RUNNING once
	// it actually holds the GPU, never while merely waiting in line.
	return has && w.runNext(ctx)
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
	if errors.Is(err, ErrNotFound) {
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

// attempt is one try at running a Job and how it ended.
type attempt struct {
	job       *Job
	genErr    error
	run       time.Duration
	serveDone <-chan struct{}
	preempted *atomic.Bool
}

func (w *Worker) runJob(ctx context.Context, j *Job) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	rs := w.svc.startRun(j.ID, cancel)
	defer w.svc.endRun(j.ID)

	a := &attempt{job: j, serveDone: w.yld.ServeContext().Done(), preempted: &atomic.Bool{}}
	go w.monitor(jobCtx, a.serveDone, w.svc.now(), cancel, a.preempted)

	opts := decodeOptions(j.ParamsJSON)
	text, genErr := w.gen.Generate(jobCtx, inference.Request{Model: j.Model, Prompt: j.Prompt, Options: opts, OnTokens: func(n int) {
		w.svc.reportTokens(j.ID, rs, n)
	}})
	a.genErr = genErr
	a.run = w.svc.now().Sub(rs.startedAt)

	// Terminal writes must survive worker-loop shutdown, so they get a context
	// that keeps ctx's values but not its cancellation.
	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer bgCancel()
	if genErr == nil {
		w.succeed(bg, a, text)
		return
	}
	w.classifyAbort(ctx, bg, a)
}

func (w *Worker) succeed(bg context.Context, a *attempt, text string) {
	id := a.job.ID
	if err := w.svc.store.Succeed(bg, id, text); err != nil {
		slog.Warn("job succeed", "id", id, "err", err)
		return
	}
	w.svc.publishTerminal(id, StateSucceeded)
	w.svc.recordJob("succeeded", a.run)
	slog.Info("job done", "id", id, "state", StateSucceeded)
}

// classifyAbort handles a generation that aborted. It classifies why,
// preferring "requeue" causes over "fail" so transient interruptions never
// burn an attempt.
func (w *Worker) classifyAbort(ctx, bg context.Context, a *attempt) {
	id := a.job.ID
	switch {
	case w.svc.isCanceled(bg, id):
		// Explicit Cancel already set CANCELED and published the terminal event.
		slog.Info("job canceled", "id", id)
	case ctx.Err() != nil:
		w.requeue(ctx, id) // broker shutting down
	case isClosed(a.serveDone):
		w.requeue(ctx, id) // gaming/Plex took the GPU
		w.svc.recordJob("preempted", a.run)
		slog.Info("job preempted", "id", id, "by", "gaming")
	case a.preempted.Load():
		w.requeue(ctx, id) // interactive request preempted past quantum
		w.svc.recordJob("preempted", a.run)
		slog.Info("job preempted", "id", id, "by", "interactive")
	default:
		w.failOrRetry(bg, a)
	}
}

func (w *Worker) failOrRetry(bg context.Context, a *attempt) {
	id := a.job.ID
	failed, err := w.svc.store.FailOrRetry(bg, id, a.genErr.Error(), w.svc.maxAttempts)
	if err != nil {
		slog.Warn("job fail", "id", id, "err", err)
		return
	}
	if failed {
		w.svc.publishTerminal(id, StateFailed)
		w.svc.recordJob("failed", a.run)
		slog.Info("job failed", "id", id, "err", a.genErr)
		return
	}
	w.svc.bus.publish(id, Event{Type: EventState, State: StateQueued})
	w.svc.recordJob("retried", a.run)
	slog.Info("job retry", "id", id, "err", a.genErr)
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

func (w *Worker) requeue(ctx context.Context, id string) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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
