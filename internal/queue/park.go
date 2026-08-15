// Package queue: park.go — bounded FIFO parking for Batch-class requests
// caught during a GPU yield. Extends Scheduler (see scheduler.go) and is
// consumed by Gate (see gate.go); it carries no HTTP logic of its own.
//
// Parking is entirely local to this file: a per-request context.WithTimeout
// around the caller's own request context, plus a release channel closed by
// the drain loop. Nothing in internal/yield participates (see ADR-0009's
// Core redesign: the drain loop is a plain 1s ticker poll of a yielding
// closure, not a broadcast signal from yield.Controller).
package queue

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// parkDrainInterval is the internal poll period for RunParkDrain. Not a
// config axis — see ADR-0009.
const parkDrainInterval = time.Second

// parkResult is the outcome of a parkFor call.
type parkResult int

const (
	parkReleased parkResult = iota
	parkExpired
	parkRejected
	parkCanceled
	parkShutdown
)

// parkedReq is one waiter in the park queue.
type parkedReq struct {
	enqueuedAt time.Time
	release    chan struct{} // closed by drainOneBurst to admit this waiter (FR-6/FR-7)
}

// parker is one bounded FIFO park queue, owned by exactly one Scheduler
// instance (per FR-5: :11436 and :11438 each get their own).
type parker struct {
	mu sync.Mutex // NEVER held at the same time as Scheduler.mu, except inside
	// Stats() — see the lock-order comment there. Every other
	// parker method takes only this mutex.
	q        []*parkedReq
	maxQueue int // 0 is MEANINGFUL: parking disabled (see SetParkConfig). Also
	// the zero-value default for a Scheduler that never calls
	// SetParkConfig at all — fail-closed by construction.
	hold       time.Duration
	drainBurst int
}

// parkFor enqueues the caller as a parked waiter and blocks until release
// (yield ended and this waiter's turn came up), the configured hold bound
// elapses, the caller's own context is cancelled (client disconnect), or the
// broker is shutting down. It is the sole entry and exit point for a
// parkedReq's lifetime in the queue: every code path below either transfers
// ownership of removal to drainOneBurst (parkReleased) or removes pr itself
// exactly once (every other outcome) — a ghost entry (pr left in p.q after
// its owning goroutine has stopped waiting on it) cannot happen, because both
// the enqueue and every dequeue/remove happen under p.mu, and there is no
// path that returns without having done one or the other. There is likewise
// no separate "depth" counter to fall out of sync: Stats().Parked and the
// broker_parked_depth gauge both read len(p.q) live under p.mu, so every
// remove() IS the decrement.
func (s *Scheduler) parkFor(ctx context.Context) parkResult {
	p := &s.park

	// Fix: ceiling check + append happen atomically under p.mu — no window
	// where two goroutines both observe "one slot free" and both enqueue,
	// blowing the ceiling.
	p.mu.Lock()
	if p.maxQueue == 0 || len(p.q) >= p.maxQueue {
		p.mu.Unlock()
		return parkRejected
	}
	hold := p.hold
	pr := &parkedReq{enqueuedAt: time.Now(), release: make(chan struct{})}
	p.q = append(p.q, pr)
	p.mu.Unlock()

	hctx, hcancel := context.WithTimeout(ctx, hold)
	defer hcancel()

	select {
	case <-pr.release:
		return parkReleased // drainOneBurst already spliced pr out of p.q

	case <-hctx.Done():
		// Fix: hctx.Done() fires for BOTH "hold elapsed" and "ctx (client)
		// cancelled" (WithTimeout wraps ctx). Before classifying either way,
		// do a non-blocking check of pr.release first — release winning the
		// race against the timeout must take priority.
		select {
		case <-pr.release:
			return parkReleased
		default:
		}
		if !p.remove(pr) {
			// Already spliced out by a concurrent drainOneBurst between
			// hctx.Done() firing and this check: release actually won, even
			// though its channel-close hasn't been observed above yet.
			return parkReleased
		}
		if ctx.Err() != nil {
			return parkCanceled // the outer (client) ctx is what fired, not the hold timer
		}
		return parkExpired

	case <-s.shutdownDone():
		if !p.remove(pr) {
			return parkReleased
		}
		return parkShutdown
	}
}

// remove splices pr out of the park queue — linear scan under p.mu, the same
// idiom as Scheduler.removeWaiter. Returns false if pr was not found (it was
// already removed, i.e. released, by a concurrent drainOneBurst) — the
// signal parkFor uses to detect "release won the race."
func (p *parker) remove(pr *parkedReq) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.q {
		if c == pr {
			copy(p.q[i:], p.q[i+1:])
			p.q[len(p.q)-1] = nil
			p.q = p.q[:len(p.q)-1]
			return true
		}
	}
	return false
}

// shutdownDone reads s.shutdownCtx under s.mu (never park.mu — see the
// lock-order note on Stats()) so the race detector has nothing to complain
// about even though SetShutdownContext is, by contract, only ever called
// once before serving traffic.
func (s *Scheduler) shutdownDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdownCtx.Done()
}

// RunParkDrain releases this Scheduler's parked Batch requests, FIFO, in
// bursts of drainBurst every parkDrainInterval, whenever yielding() reports
// false and the queue is non-empty. yielding is a plain probe (not
// Admission) so this method stays outside the "Admission reaches queue"
// boundary gate.go alone is supposed to own — see ADR-0009's Core redesign.
// Runs until ctx is done. Wired from main.go: go sched.RunParkDrain(ctx, yieldingFn).
func (s *Scheduler) RunParkDrain(ctx context.Context, yielding func() bool) {
	t := time.NewTicker(parkDrainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if yielding() {
				continue
			}
			s.park.mu.Lock()
			empty := len(s.park.q) == 0
			burst := s.park.drainBurst
			s.park.mu.Unlock()
			if empty {
				continue
			}
			s.park.drainOneBurst(burst)
		}
	}
}

// drainOneBurst releases up to burst parked requests, FIFO (batch[0] is the
// longest-waiting). Splicing p.q and closing release happen as two separate
// steps — the splice (removal) is done under p.mu; the closes happen after
// unlocking, so a slow receiver doesn't hold the lock. This is the ONLY
// place a parkedReq is removed from p.q on the "released" path — parkFor's
// own remove() is never called for a request drainOneBurst has already
// picked up (see parkFor's non-blocking pr.release check).
func (p *parker) drainOneBurst(burst int) {
	p.mu.Lock()
	if len(p.q) == 0 {
		p.mu.Unlock()
		return
	}
	n := burst
	if n > len(p.q) {
		n = len(p.q)
	}
	// Copy the batch out before nilling the queue slots: batch must not alias
	// p.q's backing array, or the nil-out below would nil the very pointers we
	// are about to close (the release channels), panicking on every drain.
	batch := make([]*parkedReq, n)
	copy(batch, p.q[:n])
	for i := 0; i < n; i++ {
		p.q[i] = nil // let GC reclaim, mirroring dequeue's nil-out-before-reslice
	}
	p.q = p.q[n:]
	p.mu.Unlock()
	for _, r := range batch {
		close(r.release)
	}
}

// SetParkConfig configures the bounded park queue.
//   - maxQueue == 0 is MEANINGFUL: parking is disabled outright (the
//     kill-switch — see ADR-0009 point 4 and the soak runbook's rollback
//     step). A Batch request arriving during yield then takes the exact same
//     immediate-deferRequest path Interactive always has — see Gate
//     integration's parkEnabled() check. This is also the zero-value default
//     for a Scheduler that never calls SetParkConfig at all, which is what
//     pins TestGateRefusesWhenYielding (Batch case) as the never-configured
//     fail-closed default.
//   - maxQueue < 0 is a config error, not a disable request: ignored (kept
//     at its previous/default value), logged as a warning. Silently treating
//     a negative value as "disabled" would hide a typo behind behavior that
//     looks identical to the deliberate kill-switch.
//   - drainBurst < 1 is ignored (kept), same "values < 1 ignored" contract
//     SetMaxWaiters already uses — no meaningful zero case for a burst size.
//   - hold <= 0 is NEVER applied silently: it would make every park expire
//     instantly, indistinguishable from a live bug. Ignored, kept at its
//     previous/default value, logged as a warning — never a silent
//     instant-expire.
//
// Call before serving traffic.
func (s *Scheduler) SetParkConfig(hold time.Duration, maxQueue, drainBurst int) {
	s.park.mu.Lock()
	defer s.park.mu.Unlock()
	switch {
	case maxQueue == 0:
		s.park.maxQueue = 0
	case maxQueue < 0:
		slog.Warn("park: BROKER_PARK_MAX_QUEUE negative, ignoring", "value", maxQueue)
	default:
		s.park.maxQueue = maxQueue
	}
	if drainBurst >= 1 {
		s.park.drainBurst = drainBurst
	}
	if hold > 0 {
		s.park.hold = hold
	} else {
		slog.Warn("park: BROKER_PARK_HOLD <= 0, ignoring, keeping previous value", "value", hold)
	}
}

// SetShutdownContext wires the broker's top-level shutdown signal (main.go's
// cancel()) into the park path (FR-9). Call before serving traffic.
func (s *Scheduler) SetShutdownContext(ctx context.Context) {
	s.mu.Lock()
	s.shutdownCtx = ctx
	s.mu.Unlock()
}

// parkEnabled reports whether parking is configured for this Scheduler
// (BROKER_PARK_MAX_QUEUE != 0). Gate uses this so a never-configured or
// explicitly kill-switched Batch request takes the SAME immediate-
// deferRequest path Interactive always has (outcome "deferred"), rather than
// entering parkFor and coming back out tagged "park_rejected" — a different,
// misleading signal for "parking was never turned on here."
func (s *Scheduler) parkEnabled() bool {
	s.park.mu.Lock()
	defer s.park.mu.Unlock()
	return s.park.maxQueue != 0
}
