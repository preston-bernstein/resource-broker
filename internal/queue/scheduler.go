// Package queue serializes access to the single GPU. Exactly one inference
// request reaches Ollama at a time; waiters are granted in priority order
// (interactive before batch, FIFO within a class). It carries no HTTP or
// yield logic — those layer on top.
package queue

import (
	"context"
	"errors"
	"sync"
)

// MaxWaitersPerClass is the default per-class waiter cap. Beyond the cap,
// Acquire fails fast so a burst can't pile up unbounded goroutines/memory;
// the gate maps the failure to a 503. Override via Scheduler.SetMaxWaiters.
const MaxWaitersPerClass = 256

// ErrQueueFull is returned by Acquire when a class queue is at capacity.
var ErrQueueFull = errors.New("queue full")

// Class is a priority tier. Higher value = higher priority.
type Class int8

const (
	// Batch is background, latency-tolerant work (pipelines, embeddings).
	Batch Class = iota
	// Interactive is human-waiting work (chat). It jumps ahead of Batch.
	Interactive
)

func (c Class) String() string {
	switch c {
	case Interactive:
		return "interactive"
	case Batch:
		return "batch"
	default:
		return "unknown"
	}
}

// Scheduler is a priority-aware gate over the GPU. It admits up to maxInflight
// concurrent requests (default 1 — see ADR-0004); waiters are granted in
// priority order. Preemption of a running batch request by interactive work is
// not done here — the scheduler only *signals* that interactive work is waiting
// (InteractiveWaiting); the batch holder (the Job worker) decides whether to
// yield its slot, honouring the batch min-run quantum.
type Scheduler struct {
	mu          sync.Mutex
	inflight    int
	maxInflight int
	iq          []chan struct{} // interactive waiters, FIFO
	bq          []chan struct{} // batch waiters, FIFO
	maxWaiters  int

	// interactiveWaiting is pinged (non-blocking, coalesced) whenever an
	// interactive request parks because no slot is free. The Job worker selects
	// on it to decide whether to preempt a running batch Job past its quantum.
	interactiveWaiting chan struct{}

	// park is this Scheduler's bounded FIFO park queue for Batch requests
	// caught during a GPU yield (see park.go). Zero-value (maxQueue == 0)
	// means parking is disabled — fail-closed for a Scheduler that never
	// calls SetParkConfig.
	park parker
	// shutdownCtx is the broker's top-level shutdown signal, wired via
	// SetShutdownContext. Defaults to context.Background() (never fires) so
	// existing tests that never call the setter are unaffected.
	shutdownCtx context.Context
}

// New returns an idle Scheduler: concurrency 1, default per-class waiter cap.
func New() *Scheduler {
	return &Scheduler{
		maxInflight:        1,
		maxWaiters:         MaxWaitersPerClass,
		interactiveWaiting: make(chan struct{}, 1),
		shutdownCtx:        context.Background(),
	}
}

// SetMaxWaiters overrides the per-class waiter cap (values < 1 are ignored).
// Call before serving traffic.
func (s *Scheduler) SetMaxWaiters(n int) {
	if n < 1 {
		return
	}
	s.mu.Lock()
	s.maxWaiters = n
	s.mu.Unlock()
}

// SetMaxInflight overrides how many requests may reach Ollama concurrently
// (ADR-0004; values < 1 are ignored). Call before serving traffic.
func (s *Scheduler) SetMaxInflight(n int) {
	if n < 1 {
		return
	}
	s.mu.Lock()
	s.maxInflight = n
	s.mu.Unlock()
}

// InteractiveWaiting returns a channel pinged when an interactive request parks
// for a busy GPU. It is coalesced (buffered 1): a receiver learns only that at
// least one interactive request is queued, not how many.
func (s *Scheduler) InteractiveWaiting() <-chan struct{} { return s.interactiveWaiting }

// Acquire blocks until the caller holds a slot, or until ctx is done. On
// success it returns nil and the caller MUST call Release exactly once. On ctx
// cancellation it returns ctx.Err() and the caller must NOT call Release.
func (s *Scheduler) Acquire(ctx context.Context, class Class) error {
	s.mu.Lock()
	if s.inflight < s.maxInflight {
		s.inflight++
		s.mu.Unlock()
		return nil
	}
	if s.queueLen(class) >= s.maxWaiters {
		s.mu.Unlock()
		return ErrQueueFull
	}
	w := make(chan struct{})
	s.enqueue(class, w)
	if class == Interactive {
		s.pingInteractiveLocked()
	}
	s.mu.Unlock()

	select {
	case <-w:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		if s.removeWaiter(class, w) {
			// Still queued: we were never granted the slot.
			s.mu.Unlock()
			return ctx.Err()
		}
		// Not in queue: Release already granted (and closed w) under lock.
		// We own the slot; hand it on so it is not leaked.
		s.mu.Unlock()
		s.Release()
		return ctx.Err()
	}
}

// Release frees a slot, handing it to the highest-priority waiter if any.
func (s *Scheduler) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.dequeue(); w != nil {
		close(w) // slot ownership transfers to the woken waiter; inflight unchanged
		return
	}
	s.inflight--
}

// Stats is a point-in-time snapshot for observability.
type Stats struct {
	// Busy reports whether at least one slot is occupied (kept for back-compat).
	Busy        bool
	Inflight    int
	MaxInflight int
	Interactive int
	Batch       int
	Parked      int // live park-queue depth, this Scheduler instance
}

// Stats returns current occupancy and per-class queue depth, plus live park
// depth. Lock order: s.mu, then park.mu — Stats is the ONLY place both are
// held at once (nested, s.mu outer). Every other path in this file takes at
// most one of the two. If a future change needs to hold both somewhere else,
// it MUST follow this same order or risk deadlock against Stats().
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Busy:        s.inflight > 0,
		Inflight:    s.inflight,
		MaxInflight: s.maxInflight,
		Interactive: len(s.iq),
		Batch:       len(s.bq),
	}
	s.park.mu.Lock()
	st.Parked = len(s.park.q)
	s.park.mu.Unlock()
	return st
}

// --- helpers (caller holds s.mu) ---

// pingInteractiveLocked coalesces a non-blocking notification that interactive
// work is queued. Caller holds s.mu.
func (s *Scheduler) pingInteractiveLocked() {
	select {
	case s.interactiveWaiting <- struct{}{}:
	default:
	}
}

func (s *Scheduler) queueLen(class Class) int {
	if class == Interactive {
		return len(s.iq)
	}
	return len(s.bq)
}

func (s *Scheduler) enqueue(class Class, w chan struct{}) {
	if class == Interactive {
		s.iq = append(s.iq, w)
	} else {
		s.bq = append(s.bq, w)
	}
}

func (s *Scheduler) dequeue() chan struct{} {
	if len(s.iq) > 0 {
		w := s.iq[0]
		s.iq[0] = nil // let GC reclaim even though the backing array slot lingers
		s.iq = s.iq[1:]
		return w
	}
	if len(s.bq) > 0 {
		w := s.bq[0]
		s.bq[0] = nil
		s.bq = s.bq[1:]
		return w
	}
	return nil
}

func (s *Scheduler) removeWaiter(class Class, w chan struct{}) bool {
	q := &s.bq
	if class == Interactive {
		q = &s.iq
	}
	old := *q
	for i, c := range old {
		if c == w {
			copy(old[i:], old[i+1:])
			old[len(old)-1] = nil // clear freed tail slot so it isn't retained
			*q = old[:len(old)-1]
			return true
		}
	}
	return false
}
