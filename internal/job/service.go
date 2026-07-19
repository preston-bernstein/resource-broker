package job

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Service is the durable Job API surface used by both the HTTP layer and the
// worker. It owns the Store, the live event bus, and the in-memory run state
// (live token counts and per-Job cancel handles) for RUNNING Jobs.
// Recorder tallies terminal Job outcomes for metrics. May be nil.
type Recorder interface {
	RecordJob(outcome string, run time.Duration)
}

type Service struct {
	store       Store
	bus         *bus
	maxAttempts int
	now         func() time.Time
	rec         Recorder

	mu      sync.Mutex
	running map[string]*runState
}

// SetRecorder attaches a metrics recorder. Call before serving traffic.
func (s *Service) SetRecorder(r Recorder) { s.rec = r }

func (s *Service) recordJob(outcome string, run time.Duration) {
	if s.rec != nil {
		s.rec.RecordJob(outcome, run)
	}
}

type runState struct {
	startedAt time.Time
	cancel    context.CancelFunc
	tokens    atomic.Int64
}

// NewService wraps a Store. maxAttempts <= 0 uses DefaultMaxAttempts.
func NewService(store Store, maxAttempts int) *Service {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	return &Service{
		store:       store,
		bus:         newBus(),
		maxAttempts: maxAttempts,
		now:         time.Now,
		running:     make(map[string]*runState),
	}
}

// SubmitRequest is a new Job submission.
type SubmitRequest struct {
	IdempotencyKey string
	Source         string
	Owner          string
	Model          string
	Prompt         string
	Options        map[string]any
}

// Submit persists a new Job (or returns the existing one for a repeated
// Idempotency-Key) and returns it. The id is returned only after the durable
// write commits (ADR-0007: no ack is ever lost).
func (s *Service) Submit(ctx context.Context, req SubmitRequest) (*Job, bool, error) {
	var paramsJSON string
	if len(req.Options) > 0 {
		b, err := json.Marshal(req.Options)
		if err != nil {
			return nil, false, err
		}
		paramsJSON = string(b)
	}
	j := &Job{
		ID:             uuid.NewString(),
		IdempotencyKey: req.IdempotencyKey,
		Source:         req.Source,
		Owner:          req.Owner,
		Model:          req.Model,
		Prompt:         req.Prompt,
		ParamsJSON:     paramsJSON,
		CreatedAt:      s.now(),
	}
	stored, created, err := s.store.Submit(ctx, j)
	if err != nil {
		return nil, false, err
	}
	if created {
		s.bus.publish(stored.ID, Event{Type: EventState, State: StateQueued})
	}
	return stored, created, nil
}

// Get returns the canonical small status, including live position and progress.
func (s *Service) Get(ctx context.Context, id string) (*Status, error) {
	j, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	st := &Status{
		ID:        j.ID,
		State:     j.State,
		Source:    j.Source,
		Owner:     j.Owner,
		Attempts:  j.Attempts,
		Error:     j.Error,
		CreatedAt: j.CreatedAt,
		FetchedAt: j.FetchedAt,
	}
	if j.State == StateQueued {
		if pos, err := s.store.Position(ctx, id); err == nil {
			st.Position = pos
		}
	}
	if j.State == StateRunning {
		st.Progress = s.progress(id)
	}
	return st, nil
}

// Result returns a SUCCEEDED Job's output and stamps the first-fetch time
// (retain-until-fetched). ErrNotReady if the Job has not succeeded.
func (s *Service) Result(ctx context.Context, id string) (string, error) {
	j, err := s.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if j.State != StateSucceeded {
		return "", ErrNotReady
	}
	if err := s.store.StampFetched(ctx, id); err != nil {
		slog.Warn("job stamp fetched", "id", id, "err", err)
	}
	return j.Result, nil
}

// List returns Jobs matching the filter.
func (s *Service) List(ctx context.Context, f Filter) ([]*Job, error) {
	return s.store.List(ctx, f)
}

// Cancel marks a Job CANCELED and aborts its upstream call if it is running.
func (s *Service) Cancel(ctx context.Context, id string) (*Job, error) {
	j, err := s.store.Cancel(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.State == StateCanceled {
		// Abort the in-flight generation, if any; the worker sees the CANCELED
		// state and does not requeue.
		var run time.Duration
		s.mu.Lock()
		if rs := s.running[id]; rs != nil {
			run = s.now().Sub(rs.startedAt)
			rs.cancel()
		}
		s.mu.Unlock()
		s.publishTerminal(id, StateCanceled)
		s.recordJob("canceled", run)
	}
	return j, nil
}

// Counts returns Job counts by state.
func (s *Service) Counts(ctx context.Context) (Counts, error) { return s.store.Counts(ctx) }

// Recover runs the startup sweep: RUNNING Jobs (interrupted by a crash/restart)
// go back to QUEUED@front, capped by maxAttempts.
func (s *Service) Recover(ctx context.Context) error {
	n, err := s.store.RecoverRunning(ctx, s.maxAttempts)
	if err != nil {
		return err
	}
	if n > 0 {
		slog.Info("job recovery sweep", "running_reset", n)
	}
	return nil
}

// Subscribe returns a live event stream for a Job and an unsubscribe func.
func (s *Service) Subscribe(id string) (<-chan Event, func()) {
	sub, ch := s.bus.subscribe(id)
	return ch, func() { s.bus.unsubscribe(id, sub) }
}

// RunPrune sweeps terminal Jobs on a ticker until ctx is done (ADR-0007).
func (s *Service) RunPrune(ctx context.Context, every, fetchedGrace, hardCap time.Duration) {
	s.pruneOnce(ctx, fetchedGrace, hardCap)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pruneOnce(ctx, fetchedGrace, hardCap)
		}
	}
}

func (s *Service) pruneOnce(ctx context.Context, fetchedGrace, hardCap time.Duration) {
	n, err := s.store.Prune(ctx, fetchedGrace, hardCap, s.now())
	if err != nil {
		slog.Warn("job prune", "err", err)
		return
	}
	if n > 0 {
		slog.Info("job prune", "deleted", n)
	}
}

// --- run-state bookkeeping (used by the worker) ---

func (s *Service) startRun(id string, cancel context.CancelFunc) *runState {
	rs := &runState{startedAt: s.now(), cancel: cancel}
	s.mu.Lock()
	s.running[id] = rs
	s.mu.Unlock()
	s.bus.publish(id, Event{Type: EventState, State: StateRunning})
	return rs
}

func (s *Service) endRun(id string) {
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
}

func (s *Service) reportTokens(id string, rs *runState, tokens int) {
	rs.tokens.Store(int64(tokens))
	s.bus.publish(id, Event{Type: EventProgress, Progress: &Progress{
		Tokens:    tokens,
		ElapsedMs: s.now().Sub(rs.startedAt).Milliseconds(),
	}})
}

func (s *Service) progress(id string) *Progress {
	s.mu.Lock()
	rs := s.running[id]
	s.mu.Unlock()
	if rs == nil {
		return nil
	}
	return &Progress{
		Tokens:    int(rs.tokens.Load()),
		ElapsedMs: s.now().Sub(rs.startedAt).Milliseconds(),
	}
}

func (s *Service) publishTerminal(id string, state State) {
	s.bus.publish(id, Event{Type: EventState, State: state})
	s.bus.publish(id, Event{Type: EventDone, State: state})
}

// isCanceled reports whether the Job is in the CANCELED terminal state.
func (s *Service) isCanceled(ctx context.Context, id string) bool {
	j, err := s.store.Get(ctx, id)
	return err == nil && j.State == StateCanceled
}
