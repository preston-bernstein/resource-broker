# Plan: Embed-lane request parking during GPU yield

Status: draft, no user Q&A. Companion to `requirements.md` (the contract — every FR/NFR/AC/C cited below is cross-checked against it). Target implementation branch: `v2-go`. The `.gitignore`/`internal/queue` tracking gap described in earlier drafts of this plan is **already fixed upstream** (commit `e565edc`, "Commit internal/queue package; narrow gitignore's queue/ pattern") — this worktree is rebased onto it. See "Prerequisite" below for the one-line verification, not a fix.

This revision replaces the original `yield.Controller.YieldEnd()` design (channel-close broadcast tied to a new field pair on `Controller`) with a plain polling drain loop, per accepted review findings — see "Core redesign" below. `internal/yield/yield.go` is untouched by this feature.

## Prerequisite (resolved — verify only)

`internal/queue` is tracked as of `e565edc`. Before writing any park code, confirm the fix is actually present in this worktree:

```
git ls-files internal/queue | wc -l   # expect ≥ 9
```

If that count is lower, stop and re-sync with `v2-go` — do not re-diagnose the `.gitignore` collision from scratch; it's already root-caused and fixed (see commit `e565edc`).

## Concurrent WIP note

The main checkout (`v2-go`) may carry uncommitted work-in-progress touching `internal/config/config.go` and `cmd/broker/main.go` (unrelated Plex-session detection work). Before this redesign, that WIP would also have collided with `internal/yield/yield.go` — it no longer does, since this plan makes no changes to `yield.go` at all. Remaining overlap is limited to `config.go` (new fields appended, not existing ones changed) and `main.go` (new wiring lines appended near existing `Set*` calls). Final merge rebases onto whatever has landed on `v2-go` by then; landed work wins — this plan's `config.go`/`main.go` additions are written as append-only diffs specifically so that rebase is mechanical, not a redesign.

## Core redesign: plain ticker poll, not a yield-controller signal

**Rejected: `yield.Controller` gains a `YieldEnd()` channel** (the original design in earlier drafts of this plan). Review found this breaks on inspection: `yieldEndCtx` is a context that is *alive* for the duration of a yield period and *cancelled* the instant it ends. A parked goroutine doing `select { case <-adm.YieldEnd(): ... }` would fire the instant it entered `select` on **every occasion the broker is not actively transitioning out of yield** — i.e., during all normal (non-yielding) uptime, `yieldEndCtx` sits already-cancelled (or, depending on exact lifecycle, needs to be "a live no-op that never fires" before any yield has happened — a level/edge distinction that is easy to get backwards and, if gotten backwards even once across the yield/no-yield/mid-yield state space, produces a persistent goroutine spinning a `select` in a tight loop, burning a CPU core for the entire time the broker is *not* yielding). That bug class doesn't exist if `yield.go` is never touched.

**Accepted instead: the park drain runs on a plain 1s ticker**, external to `yield.Controller` entirely:

- `internal/queue/park.go` gains `RunParkDrain(ctx context.Context, yielding func() bool)` — a **method on `Scheduler`**, not a package-level function, and it takes a **plain closure**, not the `Admission` interface. This is deliberate: `scheduler.go`'s package doc comment says the package "carries no HTTP or yield logic — those layer on top," and `Admission` is `gate.go`'s type (HTTP+yield meeting point). Taking `func() bool` instead of `Admission` keeps that boundary intact for every `Scheduler` method except this one closure parameter, which only ever needs a single yes/no probe, not the whole interface.
- Each tick: if `yielding()` is `false` **and** the park queue is non-empty, release one burst (`parkDrainBurst` entries, FIFO). If `yielding()` is `true`, or the queue is empty, do nothing this tick.
- Wired from `main.go` as `go sched.RunParkDrain(ctx, yieldingFn)`, where `yieldingFn` is a one-line adapter closure (`ctrl.Yielding()` returns `(bool, string)`, not `bool`, so a wrapper is required — see Integration points #6).
- **Consequence: `internal/yield/yield.go` is not modified in any way.** No `yieldEndCtx`/`yieldEndCancel` fields, no `YieldEnd()` method, no changes to `applyLocked`, `New()`, `computeLocked`, `doUnload`, or `serveCtx`/`serveCancel`. It remains exactly the seam `broker-graded-yield-frontier` work is expected to extend later.
- **Consequence: `Admission` (in `gate.go`) is not widened.** It stays `Yielding() (bool, string)` + `ServeContext() context.Context`, exactly as today.
- **Consequence: no test-fake changes.** `alwaysServe`, `alwaysYield`, `manualAdm` (in `gate_yield_test.go`/`gate_test.go`) need no new method to keep compiling — they already satisfy the unchanged `Admission` interface.
- **Trade-off, accepted explicitly:** release latency after a yield period ends is bounded by the ticker period, not instantaneous — up to ~1s before a parked request re-enters admission, versus the near-zero latency of a broadcast-on-transition design. Given `BROKER_PARK_HOLD` defaults to 600s, a ≤1s scheduling slop on the *release* side is immaterial to any of FR-1 through FR-9's bounds; it is not immaterial to nothing, but nothing in the requirements doc cares about sub-second release latency, so this is a real trade with no real cost here.

## Approach

Parking is implemented as a same-package extension of `internal/queue` (a new `park.go` file), not a new `internal/park` package, and not a change to the yield-transition logic in `internal/yield`. `Gate`'s existing two `adm.Yielding()` checks (today: immediate `deferRequest`) become the two entry points into a bounded, FIFO, per-`Scheduler` park queue, drained by a plain polling goroutine (see Core redesign above) — not by anything `yield.Controller` broadcasts. Everything else — Acquire/Release, the wait-budget semantics, the trailer-based outcome signal, the Interactive-class fast-fail path — is untouched.

The design reuses three things the repo already has, rather than inventing new machinery:
1. **`Scheduler`'s existing `Set*` config pattern** (`SetMaxWaiters`, `SetMaxInflight`) — copied for `SetParkConfig`/`SetShutdownContext`.
2. **`Scheduler`'s existing FIFO slice queues** (`iq`/`bq`) — the same append/pop-front/splice shape (including `removeWaiter`'s exact linear-scan-under-lock idiom), for the park queue and its ghost-cleanup path.
3. **The existing `deferRequest`/`record` helpers** — widened by one parameter each, not replaced.

## Architecture

### Where parking lives: `internal/queue` (extend Gate), not a new package

**Decision: `internal/queue/park.go`, same package as `gate.go`/`scheduler.go`.** Rejected: a new top-level `internal/park` package.

Justification, weighed against this repo's actual layering (per `broker-architecture-contract` invariant #12 and the `queue` package doc comment — "carries no HTTP or yield logic — those layer on top" — a statement about `scheduler.go` specifically; `gate.go` is explicitly the layer where HTTP + yield + scheduler already meet):

- **FR-5 requires the park ceiling to be independent per `Scheduler` instance** ("`:11436` and `:11438` each get their own ceiling, matching the existing per-`Scheduler` `MaxWaiters` pattern"). `MaxWaiters` is a field on `Scheduler`, configured once via `SetMaxWaiters`, read under `s.mu`. A park ceiling that must follow the identical per-instance lifecycle belongs on `Scheduler` too — a separate `internal/park` package would need `Scheduler` to export new fields or accept a park-holder object through `New()`/`Gate()`, more surface for no benefit.
- **Release-then-reacquire (FR-2, FR-7)** needs `Scheduler.Release()` and `Scheduler.Acquire()` called from inside the park logic itself (see Gate integration below). Both are already exported for exactly this kind of internal reuse.
- A cross-package `internal/park` would still need to import `queue` for `Recorder`, `Class`, and `*Scheduler` — i.e., it would be a satellite of `queue` in practice, adding an import edge with no independent reuse. One package, one file, matches the existing per-concern-file convention (`gate.go`, `scheduler.go`, now `park.go`).

### How parked goroutines wait: `context.WithTimeout` + channel-close, scoped entirely to `park.go`

**Decision: a plain per-request `context.WithTimeout` around the caller's own request context, plus a `release chan struct{}` closed by the drain loop.** No `sync.Cond` (doesn't compose with `select`), and — per the Core redesign — no new field on `yield.Controller` either. This is entirely local to `park.go`; nothing outside it participates in the wait.

```go
// park.go — new types, same package as gate.go/scheduler.go

type parkResult int

const (
	parkReleased parkResult = iota
	parkExpired
	parkRejected
	parkCanceled
	parkShutdown
)

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
	q          []*parkedReq
	maxQueue   int           // 0 is MEANINGFUL: parking disabled (see SetParkConfig)
	hold       time.Duration
	drainBurst int
}

const parkDrainInterval = time.Second // internal constant, not a config axis
```

`parkFor` is the whole per-request wait, called from `Gate`'s loop only when `class == Batch` and parking is configured (see Gate integration). It does not take `Admission` — the caller (`Gate`) has already made the `Yielding`/class decision before calling in.

```go
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
```

### Replay ordering + drain-burst cap: FIFO slice, released by `RunParkDrain`'s 1s poll (see Core redesign)

**Decision: reuse the existing FIFO-slice-of-pointers shape from `Scheduler.iq`/`bq`, not a heap.** Arrival order is already park-entry order; Interactive never parks (FR-10), so there is only ever one "class" of thing in `p.q`.

**Decision: interval-batched release via a ticker in `RunParkDrain`, not a token bucket** — FR-7's own reasoning ("release at most N parked requests per short interval") describes a leaky-bucket-by-time-slice, not a per-request-cost token bucket. Per the Core redesign, the ticker lives in `RunParkDrain` (a `Scheduler` method taking a plain closure), not tied to any signal from `yield.Controller`.

```go
// RunParkDrain releases this Scheduler's parked Batch requests, FIFO, in
// bursts of drainBurst every parkDrainInterval, whenever yielding() reports
// false and the queue is non-empty. yielding is a plain probe (not
// Admission) so this method stays outside the "Admission reaches queue"
// boundary gate.go alone is supposed to own — see Core redesign. Runs until
// ctx is done. Wired from main.go: go sched.RunParkDrain(ctx, yieldingFn).
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
	batch := p.q[:n]
	for i := range batch {
		p.q[i] = nil // let GC reclaim, mirroring dequeue's nil-out-before-reslice
	}
	p.q = p.q[n:]
	p.mu.Unlock()
	for _, r := range batch {
		close(r.release)
	}
}
```

If yielding flaps (ends, some requests released, begins again before the queue is empty), `RunParkDrain`'s next tick simply observes `yielding() == true` and does nothing that tick — no special-casing. Requests already released proceed into `Gate`'s reacquire path; if they observe `Yielding() == true` there, they re-park via the same FR-2 path.

### Config surface: `BROKER_PARK_HOLD`, `BROKER_PARK_MAX_QUEUE`, `BROKER_PARK_DRAIN_BURST`; `0` on `BROKER_PARK_MAX_QUEUE` is the kill-switch

| Var | Default | Reasoning |
|---|---|---|
| `BROKER_PARK_HOLD` | `600s` | NFR-1: must stay under LightRAG's `EMBEDDING_TIMEOUT` (1200s, wraps the whole call) with margin. **Headroom wording (corrected):** `600s` (park) + `300s` (`BROKER_BATCH_WAIT`, slot-contention wait) = `900s` of the 1200s budget is consumed by *waiting alone*, before any embed call begins. The remaining ~300s is the serve-plus-retry-transport budget — **not slack**. An operator raising either wait default eats directly into that serve/retry headroom; it is not a buffer with room to spare, and ADR-0009 must say so explicitly (see ADR outline below). |
| `BROKER_PARK_MAX_QUEUE` | `32` | Per FR-5's own reasoning: LightRAG's `EMBEDDING_FUNC_MAX_ASYNC=10` concurrent callers, ~3x headroom for retry-transport overlap and other Batch traffic. **`0` is not "tiny," it is the documented kill-switch** — see SetParkConfig below and ADR-0009 point 4. |
| `BROKER_PARK_DRAIN_BURST` | `8` | A quarter of the 32-deep ceiling per tick, so a *full* queue drains in ~4 seconds of wall time (4 × `parkDrainInterval`) — gentle pacing for the newly-reopened single GPU slot (`MaxInflight` defaults to 1). Far under Ollama's own `OLLAMA_MAX_QUEUE=512`. |

**`BROKER_PARK_MAX_QUEUE=0` IS the enable/disable toggle — deliberate, not a missing feature.** Earlier drafts of this plan debated whether a separate `BROKER_PARK_ENABLED` was needed and concluded no, because `BROKER_PARK_MAX_QUEUE` set low "already provides an effective-disable knob." This revision sharpens that into an explicit contract rather than an incidental one:

```go
// SetParkConfig configures the bounded park queue.
//   - maxQueue == 0 is MEANINGFUL: parking is disabled outright (the
//     kill-switch — see ADR-0009 point 4 and the soak runbook's rollback
//     step). A Batch request arriving during yield then takes the exact same
//     immediate-deferRequest path Interactive always has — see Gate
//     integration's parkEnabled() check. This is also the zero-value default
//     for a Scheduler that never calls SetParkConfig at all, which is what
//     pins TestGateRefusesWhenYielding (Batch case) as the never-configured
//     fail-closed default — see Test plan.
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
```

Config loading consequence: `internal/config.getint` today rejects any value `< 1` (`"%s must be >= 1, got %d"`), which is correct for every existing var but wrong for `BROKER_PARK_MAX_QUEUE`, where `0` must load successfully as the kill-switch. `config.go` needs a second int-loading helper (e.g. `getintMin(key string, def, min int)`, or a dedicated `getParkMaxQueue` that allows `0` and rejects `< 0`) — call this out explicitly in the `config.go` diff so it isn't silently reused from `getint` and made to reject `0`.

Per constraint C-2, all three vars touch the same five places in the same change as the Tdarr precedent requires: `internal/config/config.go` (new `ParkHold time.Duration`, `ParkMaxQueue int`, `ParkDrainBurst int` fields + loader calls in `Load()`, including the `0`-permitting loader above for `ParkMaxQueue`), `internal/config/config_test.go` (default-resolution + override + invalid-value tests for all three, **plus a dedicated `0`-is-valid test for `ParkMaxQueue`** distinct from the invalid/negative case), the README `### Configuration (env)` table, `deploy/broker.service` (`Environment=` lines with a comment block in the `ad07905` style, explicitly mentioning `BROKER_PARK_MAX_QUEUE=0` as the documented rollback), and `.claude/skills/broker-config-and-flags/SKILL.md`'s env-var table.

### Gate integration: the rewritten admission loop (Admission interface unchanged)

`Gate`'s current two-`if`-statement structure becomes a `for` loop so a park→release→re-yielding sequence (FR-2) can retry admission without recursion. **`Admission` is untouched** — `parkFor` takes no `Admission` argument; `Gate` decides whether to call it at all, using a new `parkEnabled()` helper:

```go
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
```

```go
func (s *Scheduler) Gate(class Class, wait time.Duration, adm Admission, rec Recorder, next http.Handler) http.Handler {
	cls := class.String()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var parkWait time.Duration
		wasParked := false

		for {
			if yielding, reason := adm.Yielding(); yielding {
				if class != Batch || !s.parkEnabled() { // FR-10 + kill-switch/never-configured
					deferRequest(w, rec, cls, "deferred", "deferred", "yielding GPU: "+reason, wait, time.Since(start))
					return
				}
				t0 := time.Now()
				switch res := s.parkFor(r.Context()); res {
				case parkReleased:
					wasParked = true
					parkWait += time.Since(t0)
					continue // FR-6/FR-7: re-enter admission from the top
				case parkExpired:
					deferRequest(w, rec, cls, "deferred", "expired", "park hold exceeded", wait, time.Since(start))
					return
				case parkRejected:
					deferRequest(w, rec, cls, "deferred", "park_rejected", "park queue full", wait, 0)
					return
				case parkCanceled:
					record(rec, cls, "canceled", time.Since(start)) // FR-8: no response write, client is gone
					return
				case parkShutdown:
					deferRequest(w, rec, cls, "crash_failed", "crash_failed", "broker shutting down", wait, time.Since(start))
					return
				}
			}

			actx, acancel := context.WithTimeout(r.Context(), wait)
			err := s.Acquire(actx, class)
			acancel()
			waited := time.Since(start)
			if err != nil {
				deferRequest(w, rec, cls, "deferred", "deferred", "GPU busy: wait budget exceeded", wait, waited)
				return
			}

			if yielding, _ := adm.Yielding(); yielding {
				s.Release() // never hold the slot through a park (NFR-3's spirit)
				if class != Batch || !s.parkEnabled() {
					deferRequest(w, rec, cls, "deferred", "deferred", "yielding GPU", wait, waited)
					return
				}
				continue // FR-2: park instead of the old immediate deferRequest
			}
			defer s.Release()

			// ...unchanged from here: ctx/cancel, serveDone select, headers,
			// next.ServeHTTP, outcome-from-serveDone, trailer, record(). One
			// addition: if wasParked && outcome == "served", also call
			// rec.RecordPark(parkWait) — see Data model, metrics.
			break
		}
		...
	})
}
```

Note that `Gate`'s existing `serveDone := adm.ServeContext().Done()` select for in-flight preemption is completely unaffected — that mechanism belongs to the already-served part of the request, downstream of the loop above, and nothing here touches it.

## Data model

No persistent state (NFR-4) — everything below is in-memory, scoped to one `Scheduler` instance's lifetime.

**`Scheduler` gains:**
```go
type Scheduler struct {
	// ...existing fields unchanged...
	park        parker
	shutdownCtx context.Context // set via SetShutdownContext; defaults to
	                             // context.Background() (never fires) so
	                             // existing tests that never call the
	                             // setter are unaffected
}
```
```go
// SetShutdownContext wires the broker's top-level shutdown signal (main.go's
// cancel()) into the park path (FR-9). Call before serving traffic.
func (s *Scheduler) SetShutdownContext(ctx context.Context)
```
(`SetParkConfig` is defined in the Config surface section above.)

**`parkedReq`**, **`parker`**, **`parkResult`** — see Architecture above; one heap-allocated `parkedReq` per currently-parked request, removed from `p.q` by exactly one of `drainOneBurst` or `parker.remove` (never both, never neither — see the ghost-cleanup argument in `parkFor`'s doc comment).

**`Registry` (metrics package) gains:**
```go
type Registry struct {
	// ...existing fields...
	parkWaitSumMs float64
	parkWaitCount int64
}

// RecordPark tallies time spent parked, for a request that was eventually
// served (mirrors Record's existing waitSumMs/waitCount pattern exactly).
func (r *Registry) RecordPark(wait time.Duration)
```
The `Recorder` interface in `gate.go` widens by one method:
```go
type Recorder interface {
	Record(class, outcome string, wait time.Duration)
	RecordPark(wait time.Duration) // NEW
}
```
Blast radius: `*metrics.Registry` (the only production implementer) gets the new method; any test fake implementing `Recorder` needs the one-line addition (grep `internal/queue/*_test.go`; `internal/job/worker_test.go`'s rig passes `nil`, which satisfies the interface fine — no change needed there). This is the one interface-widening change in the whole feature (the `Admission` interface is explicitly NOT touched — see Core redesign); everywhere else extends by adding new outcome *strings* through the existing `Record(class, outcome, wait)` shape, per FR-11.

**`Scheduler.Stats()` gains one field:**
```go
type Stats struct {
	// ...existing fields...
	Parked int // live park-queue depth, this Scheduler instance
}
```
```go
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
```

## API contract

No new HTTP endpoints. Wire-visible changes only:

- **`X-Broker-Status` response header** gains one new value: `crash_failed` (FR-9). `deferred` continues to cover the immediate-yield-reject, wait-budget-exceeded, expired, *and* park_rejected cases. `canceled` writes no header at all (client already disconnected — FR-8).
- **`Retry-After`** header: set on every 503 path except `canceled` — unchanged mechanism, now also covering `expired`/`park_rejected`/`crash_failed`.
- **HTTP trailer** (`http.TrailerPrefix+"X-Broker-Status"`): unaffected for the streamed-and-served-after-park case — parking is invisible to it once the request is actually being served.
- **`broker_requests_total{class="batch",outcome=...}`**: four new outcome label values (`expired`, `park_rejected`, `canceled`, `crash_failed`) on the existing counter — no new metric family (FR-11).
- **`broker_parked_depth`** (new gauge). **No `class` label** — deliberate, not an oversight: parking is a single-class feature (Interactive never parks, FR-10), so a `class="interactive"` series would exist only to permanently read `0`, and a `class="batch"` series would carry the label with no cardinality it ever varies over. The `HELP` text states this explicitly ("batch-class park depth; Interactive never parks") so a Grafana dashboard author doesn't go looking for a missing interactive series. `/status` and `Stats().Parked` follow the same no-label convention.
- **`broker_park_wait_seconds_sum` / `broker_park_wait_seconds_count`** (new counter pair, mirrors `broker_wait_seconds_sum`/`_count` — that existing pair also has no class label, for the same reason).
- **`GET /status`** (admin, `internal/admin/admin.go`): the `"queue"` object gains a `"parked"` key, `Stats().Parked` — see Integration points #8.
- **`internal/config.Config`**: three new exported fields (`ParkHold`, `ParkMaxQueue`, `ParkDrainBurst`).

## Integration points

1. **`internal/yield/yield.go`** — **NOT MODIFIED.** Explicitly listed here (rather than omitted) so a future diff review can confirm zero lines changed in this file as part of this feature — see Core redesign.
2. **`internal/queue/gate.go`** — `Admission` interface **unchanged**; widen `Recorder` (add `RecordPark`); rewrite the admission section of `Gate` into the loop above (adds `parkEnabled()` check + `parkFor` call, no `adm` passed into `parkFor`); widen `deferRequest`'s signature to take a separate `status` (X-Broker-Status value) and `outcome` (metrics tag) pair instead of one string.
3. **`internal/queue/park.go`** (new file) — `parker`, `parkedReq`, `parkResult`, `parkFor`, `parker.remove`, `drainOneBurst`, `RunParkDrain`, `SetParkConfig`, `SetShutdownContext`, `parkEnabled`, `shutdownDone`.
4. **`internal/queue/scheduler.go`** — add `park parker` and `shutdownCtx context.Context` fields to `Scheduler`; `Stats()` gains `Parked` (lock-order comment, see Data model); `New()` initializes `shutdownCtx: context.Background()`.
5. **`internal/metrics/metrics.go`** — `RecordPark`, `parkWaitSumMs`/`parkWaitCount` fields, new `write()` lines (`broker_parked_depth` gauge with no class label + `broker_park_wait_seconds_{sum,count}` counter pair); `Gauges` struct gains `Parked int`.
6. **`cmd/broker/main.go`** — `sched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)` and `sched.SetShutdownContext(ctx)` alongside the existing `sched.SetMaxWaiters(...)`/`SetMaxInflight(...)` calls; a one-line adapter closure `yieldingFn := func() bool { y, _ := ctrl.Yielding(); return y }` defined once near `ctrl := yield.New(...)`; `go sched.RunParkDrain(ctx, yieldingFn)` alongside `go ctrl.Run(ctx)`; identical `SetParkConfig`/`SetShutdownContext`/`go embedSched.RunParkDrain(ctx, yieldingFn)` calls inside the `if cfg.InfinityURL != nil` block (reusing the same `yieldingFn`, since both schedulers watch the same shared `ctrl`); `metricsHandler`'s `Gauges{...}` literal gains `Parked: st.Parked` (from `sched.Stats()` — **not** `embedSched.Stats()`, an existing, documented scope limit — see Risk Areas).
7. **`internal/config/config.go`** + **`config_test.go`** — three new fields/vars; `ParkMaxQueue` needs a `0`-permitting loader distinct from the existing `getint` (see Config surface); three-shape tests each (default/override/invalid), plus a dedicated `0`-is-valid case for `ParkMaxQueue`.
8. **`internal/admin/admin.go`** — `/status` handler's `"queue"` map literal gains `"parked": st.Parked` (the `st := stats.Stats()` call already happens at the top of the handler — this is a one-line addition to the existing map literal, no new interface method needed since `StatsProvider.Stats()` already returns the widened `queue.Stats`).
9. **`internal/queue/gate_yield_test.go`, `internal/queue/gate_test.go`, `internal/queue/gate_wait_test.go`, `internal/queue/scheduler_test.go`** (existing) — **no fake changes** (Core redesign: `Admission` untouched). `TestGateRefusesWhenYielding` is re-pointed as the Batch-class never-configured fail-closed pin (a comment added, assertions unchanged) — see Test plan. `scheduler_test.go`'s existing `Stats()` test gains a check that `Parked` initializes to `0`.
10. **`README.md`** — `### Configuration (env)` table, three new rows.
11. **`deploy/broker.service`** — three new `Environment=` lines with a rationale comment block (`ad07905`-style), explicitly naming `BROKER_PARK_MAX_QUEUE=0` as the first-line rollback.
12. **`.claude/skills/broker-config-and-flags/SKILL.md`** — new rows in the env var table, new entries in the "how to add a config axis" precedent list if it references specific vars.
13. **`.claude/skills/broker-architecture-contract/SKILL.md`** — new invariant row (parking never lets a Batch request hold a slot while parked; parked requests are always released before the 10s shutdown window closes; `RunParkDrain` is the one `Scheduler` method that takes a plain closure instead of respecting the package's "no HTTP or yield logic" boundary via `Admission`, and that's intentional — see Core redesign). Recommended, not blocking.
14. **`CONTEXT.md`** — new **Park** / **Parked request** and **Drain burst** glossary entries (C-5), each with an *Avoid* list, distinct from **Queue**.
15. **`docs/adr/0009-embed-lane-parking.md`** (new) and **`docs/adr/0002-stateless-http-bounded-wait.md`** (status line amended in place) — see ADR-0009 outline below.
16. **`docs/runbooks/embed-parking-soak.md`** (new, or extension to an existing skill) — chaos/soak validation procedure (AC-14): force yield via the existing control endpoint, not a fake gaming process — see ADR outline point 4 / Test plan.

## Technology choices

- **Language/stdlib only**: `context`, `sync`, `time` — no new dependency. Matches the repo's zero-new-dependency posture.
- **`context.WithTimeout`** for the per-request park wait, composing the same way `Gate`'s existing `serveDone`-derived select already does — no new synchronization primitive. Per the Core redesign, this is entirely local to `park.go`; `yield.Controller` contributes nothing to it.
- **`time.Ticker`** for `RunParkDrain`'s 1s poll — the same primitive `yield.Controller.Run` already uses for its own interval loop, so this is consistent with the codebase, not novel — but it is a *new, independent* ticker in `park.go`, not a reuse of `Controller.Run`'s ticker or anything driven by `yield.Controller` at all.
- **Slice-based FIFO** over `container/list` or a channel-as-queue — matches `Scheduler.iq`/`bq` exactly, including the `nil`-out-before-reslice idiom, reused verbatim for `parker.q`.

## Risk areas

1. **`Gate`'s admission section becomes a loop with five new exit branches** where it was two straight-line `if`s. This is the highest-traffic function in the binary (every Batch and Interactive request passes through it) and the change with the most surface for a subtle bug (e.g., forgetting `s.Release()` before parking on the FR-2 path would silently violate NFR-3 by holding a slot for up to `BROKER_PARK_HOLD`). Mitigated by AC-1 through AC-9 covering every branch explicitly, but this is worth an extra pass of manual review beyond the usual bar, and a full-repo `-race` run before merge (see Test plan).
2. **`deferRequest`'s signature change (adding a `status` param separate from `outcome`) touches every existing call site** (2 in `gate.go` today, both must pass `"deferred","deferred"` to stay behaviorally identical) — low functional risk (compiler catches missed sites), but worth an explicit diff check that no existing 503 response's `X-Broker-Status` header value silently changed.
3. **NFR-5 (no added Interactive-path latency/contention) depends on `parker.mu` never being the same mutex as `Scheduler.mu`** (true by construction — `Stats()` is the one documented exception, and it takes them in a fixed nested order, never concurrently from two different call sites in opposite order), **and on Interactive requests never calling into `park.go` at all** (true by construction — `class != Batch` short-circuits before `parkEnabled()`/`parkFor` in both places `Gate` checks yielding). Worth a one-line comment at the `parker.mu` declaration calling this out, and the architecture-contract invariant row (Integration point #13).
4. **The park-then-reacquire loop (FR-2/FR-7) means a request can pay the full `BROKER_PARK_HOLD` *and then* the full `BROKER_BATCH_WAIT` more than once** if yield flaps (ends, a request is released, yield starts again before it re-acquires, it re-parks). This holds regardless of the drain mechanism (ticker-poll here vs. the earlier signal-based design) — NFR-2's "900s" figure is a *single-park* bound, not an enforced cap; under pathological flapping the actual bound is unbounded in theory. In practice `BROKER_YIELD_CONFIRM_POLLS` debounces entry into yield (not exit), and real gaming sessions don't flap sub-minute, so this is a theoretical edge the ADR names explicitly (see ADR outline point 3) rather than a practical concern.
5. **`internal/admin` test suite is already broken at HEAD** (tracked, pre-existing, per `broker-architecture-contract` weak point #4). This change now touches `internal/admin/admin.go` directly (Integration point #8, the `/status` `"parked"` key) in addition to `internal/metrics.Gauges` and `internal/queue.Stats` — a strictly larger blast radius against that already-broken suite than earlier drafts of this plan assumed. C-6 requires diffing the failure set before/after, not just eyeballing "tests still fail"; this is now more load-bearing than before, not less.
6. **`RunParkDrain`'s ≤1s release latency is a deliberate, accepted trade** (see Core redesign) — flagged here as a risk-area entry only so a future reader doesn't mistake it for an overlooked bug: the ticker interval is a design choice bounded by `parkDrainInterval`, not an incidental scheduling artifact.

## ADR-0009 outline

**File:** `docs/adr/0009-embed-lane-parking.md`. **Status line:** `accepted; supersedes ADR-0002 for the "arrives during yield" behavior of Batch-class Synchronous requests specifically — ADR-0002's stateless-bounded-wait model remains in force for everything else (interactive class, and the non-yield wait-budget path for batch).`

Structure (house style: one page, decision + rationale + rejected alternatives, per `broker-change-control` non-negotiable #4):

1. **Decision.** Batch-class requests arriving (or caught mid-slot-wait) during yield are parked, bounded by `BROKER_PARK_HOLD` (600s) and `BROKER_PARK_MAX_QUEUE` (32 per Scheduler instance, `0` = disabled/kill-switch), released FIFO in bursts of `BROKER_PARK_DRAIN_BURST` (8) at most once per second on a plain poll of yield state, instead of immediate 503. Interactive-class semantics are unchanged. **`park release ordering` clarification (new, must be stated explicitly):** FIFO governs the order requests *leave the park queue*, not the order they are ultimately *served* — a released request still re-enters `Scheduler.Acquire`'s own priority-and-FIFO-within-class queueing (Interactive can still jump ahead of a just-released Batch request at that stage, exactly as it does for any other Batch request today). Park-FIFO and service-order are two distinct, sequential guarantees; conflating them would overclaim what parking itself controls.
2. **Why now.** The embed-cascade problem: LightRAG has no retry at the embedding layer (discussion #1591) and an embed failure aborts the whole ingest batch (#2387/#2300/#2257/#2495), disproving ADR-0002's founding premise ("every consumer already tolerates a deferred LLM") for this specific consumer/path. Cite the 2026-07-22 research sweep.
3. **Answering ADR-0002's three original objections, point by point** (C-3, mandatory):
   - *fd exhaustion* → the hard per-instance park ceiling (FR-5/NFR-3): one goroutine + one open connection per parked request, capped at 32, nowhere near ulimit defaults on a single-purpose home broker process.
   - *client timeouts fire anyway* → `BROKER_PARK_HOLD` (600s) is chosen with explicit margin under LightRAG's `EMBEDDING_TIMEOUT` (1200s) — NFR-1/NFR-2. State the corrected 600s+300s=900s **wait-budget** sum explicitly (not "the second half of the budget for wait plus serve" — that undercounts what's actually spoken for): the remaining ~300s is serve-plus-retry headroom, not slack an operator can freely reallocate. State the flapping caveat (Risk Area #4) honestly — not automatically enforced, operator-must-recheck-on-default-change.
   - *reboot-safety* → no cross-restart state (NFR-4, in-memory only); fail-fast-visibly within the existing 10s shutdown window on a *graceful* stop (FR-9, `crash_failed`); and the honest limitation that a *hard* crash (SIGKILL/OOM/power loss) is unobservable by definition — recovery is entirely external (LightRAG's rescan of un-embedded chunks + its LLM-response cache, extraction never re-billed). State this plainly, not glossed over.
4. **Rejected alternatives**, named with reasons: unbounded park (fd exhaustion); no ceiling / reject-never (same); immediate full-burst replay on yield-end (thundering herd against a 1-slot GPU scheduler, no benefit over paced release); a separate `BROKER_PARK_ENABLED` toggle (redundant — `BROKER_PARK_MAX_QUEUE=0` already IS the documented kill-switch, a second knob for the same effect is gold-plating); `sync.Cond` (doesn't compose with `select`/context cancellation); a new `internal/park` package (see Architecture); a token-bucket cost-metering release mechanism (no per-request cost variance to meter); **`yield.Controller.YieldEnd()` — a channel-close broadcast field pair added to `Controller`, the design in earlier drafts of this plan** (rejected on review: `yieldEndCtx` is alive for the duration of a yield period and cancelled the instant it ends, so a persistent `select` waiting on it fires immediately during all *normal, non-yielding* uptime unless its idle-state polarity is threaded correctly through every one of `New()`/`applyLocked`'s two branches/the pre-first-yield case — a level-vs-edge distinction proven, on review, to be exactly the kind of thing that's easy to get backwards once across that state space, at the cost of a goroutine spinning a tight loop and burning a CPU core for the entire time the broker is not yielding. The plain 1s-ticker poll in `RunParkDrain` (Core redesign) has no such failure mode and needs no change to `yield.go` at all).
5. **`StartLimitBurst`/`StartLimitIntervalSec` decision (NFR-7, AC-15 — must not be silent).** Recommended: **unchanged** (`RestartSec=5`, `StartLimitIntervalSec=60`, `StartLimitBurst=5`). Reasoning to state explicitly, judged adequate on its own terms rather than merely asserted: parking adds a new class of long-lived blocked handler, but every parked goroutine is bounded by `BROKER_PARK_HOLD` (600s) and unconditionally resolved within the 10s shutdown window on any *graceful* stop (FR-9) — for a park-path bug to threaten the restart budget, it would have to **panic the process**, not merely block a request; nothing in the park design (bounded queue, bounded hold, mutex-protected slice splice, no recursion, no unbounded goroutine growth) introduces a new panic surface beyond what a bug in any other request-handling code could already do. `newServer()` in `cmd/broker/main.go` sets `ReadHeaderTimeout` but explicitly **no `WriteTimeout`/`ReadTimeout`** ("inference streams can run for minutes" — existing comment); verified this is still safe with parking: a parked connection consumes no server-side read/write loop time while waiting in `p.q` (it's blocked in `parkFor`'s `select`, not in `next.ServeHTTP`), so a 600s park hold is not a new exposure against those absent timeouts today. **Forward warning (record in the ADR, not just here):** if a future change adds `WriteTimeout`/`ReadTimeout` to `newServer()`, it MUST account for park holds explicitly (a naive fixed timeout shorter than `BROKER_PARK_HOLD` would sever a parked connection's underlying TCP socket out from under `parkFor`'s otherwise-correct in-memory wait) — this is exactly the kind of interaction a future "harden the HTTP server" pass could break without realizing parking depends on the current no-timeout posture. If a future soak (AC-14) finds park-path panics under real load, revisit the restart budget — but do not preemptively widen it against a failure mode this design doesn't newly introduce.
6. **Scope note**, restating C-1/C-7 verbatim: yield transition itself, the Job/durable path, Interactive semantics, and Ollama itself are unchanged. **Restated once more, explicitly, since it is the headline of this revision:** `internal/yield/yield.go` has zero lines changed by this feature — see Core redesign.

`docs/adr/0002-stateless-http-bounded-wait.md` status-line amendment (C-4, in place, not deleted):
> **Status: still in force for Synchronous requests (interactive + short batch, and the non-yield wait-budget path for batch); superseded for long batch by ADR-0006/0007; superseded for the "arrives during yield" behavior of Batch-class Synchronous requests specifically by ADR-0009 (parking).**

## CONTEXT.md additions (C-5)

Two new entries, same format as existing (bold term, definition, `_Avoid_` list), inserted after **Queue** so the distinction reads adjacently:

> **Park** (or **Parked request**):
> A Batch-class request held in memory, bounded by `BROKER_PARK_HOLD`, while the Broker is yielding — waiting for yield to end rather than being refused immediately. Distinct from the Queue (which is the per-class *scheduler* waiter list for GPU-slot contention, unrelated to yield): a request can be parked, then queued, sequentially, as two separate bounded waits. Interactive-class requests are never parked. `BROKER_PARK_MAX_QUEUE=0` disables parking entirely (kill-switch) — a Batch request during yield then behaves exactly like an Interactive one.
> _Avoid_: Hold, Buffer, Suspend, Queue (do not conflate with the scheduler waiter list)

> **Drain burst**:
> The bounded batch of parked requests released together, at most once per second, when a poll finds yielding has ended and the park queue is non-empty — paced by `BROKER_PARK_DRAIN_BURST` so a full park queue draining does not present Ollama's own queue with a larger burst than a single caller would produce.
> _Avoid_: Flush, Replay-all, Burst (alone, without "drain")

## Test plan (names the functions; see also AC cross-check below)

New tests, `internal/queue` unless noted:

- **`TestGateParksDuringYield`** (AC-1): a Batch-class request arriving while `Yielding()` is true, with parking configured, does not return until yielding clears (within the hold bound), then is served normally.
- **`TestGateParkExpires`** (AC-2): parked longer than `BROKER_PARK_HOLD` → 503, `outcome=expired`, upstream never hit.
- **`TestGateParkQueueCeiling`** (AC-3): at `BROKER_PARK_MAX_QUEUE`, the next arrival is `park_rejected` immediately, no blocking.
- **`TestGateRefusesWhenYielding`** (existing, AC-4) — **re-pointed with an added comment, assertions unmodified**: this test never calls `SetParkConfig`, so it now pins the Batch-class **never-configured fail-closed default** — `park.maxQueue`'s zero value means parking is off, and Batch during yield takes the identical immediate-`deferRequest` path Interactive always has. The comment should say this explicitly so a future reader doesn't mistake "test still passes" for "parking works," when what it actually proves is "parking being off doesn't break anything."
- **`TestGateInteractiveNeverParksWhenConfigured`** (new, FR-10, distinct from the above): with parking explicitly configured (`SetParkConfig` called, `maxQueue > 0`), an Interactive-class request during yield still returns immediately via `deferRequest`, never entering `parkFor` — proves the `class != Batch` gate itself, not just "parking was never turned on."
- **`TestGateParkDrainBurst`** (AC-5): N parked requests exceeding `BROKER_PARK_DRAIN_BURST`; once `yielding()` (the test's own closure, not a real `Controller`) flips false, released requests reach slot-acquire at no more than the configured burst rate, in FIFO park-entry order.
- **`TestGateParkClientDisconnect`** (AC-6): cancelling a parked request's context returns promptly, `outcome=canceled`, no leaked goroutine (`-race` + a goroutine-count check).
- **`TestGateParkShutdown`** (AC-7): active parked requests resolve to `crash_failed` within the existing 10s `shutCtx`, never hanging.
- **`TestParkGhostCleanup`** (new — mandatory fix #1 regression guard): drive several `parkFor` calls through expiry/cancel/shutdown exit paths (not release), then assert `len(park.q) == 0` and `Stats().Parked == 0` afterward — proves every non-released exit actually spliced itself out, not just that the response was correct.
- **`TestParkConcurrentCeilingRace`** (new, run with `-race`): many goroutines call `parkFor` concurrently against a small `maxQueue`; assert the queue never exceeds `maxQueue` at any point and exactly `maxQueue` are accepted (`parkRejected` for the rest) — proves the ceiling-check-and-append atomicity fix, not just the single-goroutine happy path.
- **`TestRunParkDrainPacing`** (new): with N parked > `drainBurst`, assert successive release batches are separated by roughly `parkDrainInterval` (not released all at once, not released early) — a direct per-tick pacing assertion, not just an eventual-consistency check.
- **`TestRunParkDrainBoundedUnderFlap`** (new — mandatory fix / busy-loop regression guard): drive the `yielding func() bool` closure to flip rapidly (e.g. every 10ms) over a fixed wall-clock window (e.g. 3s), and assert the number of times `drainOneBurst` actually executes is bounded near `window / parkDrainInterval` (e.g. ≤ 5), not in the thousands. This is the test that would have caught the rejected `YieldEnd()` design's busy-loop bug had it shipped — keep it even though that design was rejected, since it also guards `RunParkDrain`'s own ticker logic against a future regression.
- **AC-8/AC-9 metrics tests** (`internal/metrics`): `broker_parked_depth` reflects live count and returns to `0` after drain (no class label — assert the metric line itself has no `class=` in it); `broker_requests_total`/`broker_park_wait_seconds_{sum,count}` increment correctly for each of served-after-park, expired, park_rejected, canceled, crash_failed.
- **`TestMainShutdownFailsParkedRequests`** (AC-7, `cmd/broker` or an integration-style test) — full shutdown path, not just the `Scheduler`-level unit test.
- **Full-repo `-race` gate before merge**: `go test -race ./...` clean, zero new failures beyond the pre-existing, tracked `internal/admin` failure (C-6) — required given Risk Area #5's larger `internal/admin` blast radius in this revision (the `/status` `"parked"` key).

## Acceptance-criteria cross-check

Every AC in `requirements.md` maps onto a named piece of this plan: AC-1/AC-2/AC-3/AC-6/AC-7 → the `parkFor`/`Gate` loop, its five-way switch, and the Test plan's named tests above; AC-4 → `TestGateRefusesWhenYielding`, re-pointed as the never-configured fail-closed pin (see Test plan); AC-5 → `drainOneBurst` + `TestGateParkDrainBurst`/`TestRunParkDrainPacing`; AC-8/AC-9 → `Stats.Parked` + `RecordPark`/new `broker_requests_total` outcomes + their metrics tests; AC-10 → Prerequisite verification (`git ls-files internal/queue | wc -l` ≥ 9) makes C-6's diff-the-failure-set meaningful; AC-11 → Config surface section + Integration points #7/#10/#11/#12; AC-12/AC-13 → ADR-0009 outline + CONTEXT.md additions; AC-14 → the soak runbook (Integration point #16): force yield via `POST /control {"mode":"yield"}` then `{"mode":"auto"}` (ADR-0005's existing control endpoint) mid-embed-burst, confirm zero LightRAG ingest failures across the window, then flip back — **no fake gaming process needed**, the existing manual-override mechanism already does exactly this; AC-15 → ADR-0009 outline point 5.
