package yield

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ConfigureIdle records per-instance idle-unload timeouts, keyed by ORIG
// (pre-nil-filter) index — the same index space as the unloaders slice
// originally passed into NewWithConfirmMulti (or NewMulti), before nil
// filtering. idleTimeouts must be the same length as that original slice
// (equivalently, len(c.origToFiltered)); a zero duration at index i means no
// idle timeout is configured for that instance.
//
// This only populates c.idleTimeouts (index-aligned with the post-filter
// c.unloaders/c.labels); it does not itself start or affect any idle
// detection or unload behavior — that is checkIdleLocked/TrackActivity/
// IdleSummary, added in later tasks.
//
// Both failure modes here are construction-time invariant violations that
// config validation (added in a separate task) should already prevent, so
// this panics rather than silently ignoring or corrupting state:
//   - a length mismatch against origToFiltered (would otherwise silently
//     drop entries or index out of bounds with Go's generic panic message);
//   - a nonzero idle timeout at an orig index whose unloader was nil-filtered
//     (no Unloader means there is nothing to idle-unload).
func (c *Controller) ConfigureIdle(idleTimeouts []time.Duration) {
	if len(idleTimeouts) != len(c.origToFiltered) {
		panic(fmt.Sprintf("yield: ConfigureIdle: got %d idle timeouts, want %d (len(origToFiltered))", len(idleTimeouts), len(c.origToFiltered)))
	}
	for i, d := range idleTimeouts {
		post := c.origToFiltered[i]
		if post == -1 {
			if d != 0 {
				panic(fmt.Sprintf("yield: ConfigureIdle: orig index %d has an idle timeout but no Unloader (no _UNIT_NAME) — this should have been caught at config.Load()", i))
			}
			continue
		}
		c.idleTimeouts[post] = d
	}
}

// TrackActivity wraps next with per-instance idle bookkeeping for the
// configured instance at ORIG (pre-nil-filter) index origIdx — the same
// index space ConfigureIdle takes. It is a construction-time decorator:
// called once per instance, by a caller in internal/backend, at wrap time —
// not per request — so the origIdx -> post-filter lookup below happens once
// here rather than being repeated on every request the returned handler
// serves.
//
// origIdx must identify an instance with a configured Unloader (i.e.
// c.origToFiltered[origIdx] != -1); requesting idle-tracking for an
// orig-index with no Unloader is a construction-time invariant violation
// config validation should already prevent (mirrors ConfigureIdle's own
// panic for the symmetric case), so this panics rather than silently
// no-op'ing or indexing a bookkeeping slot that doesn't exist.
//
// The returned handler updates c.inFlight[post] and c.lastDispatch[post]
// around every call to next.ServeHTTP so checkIdleLocked can see accurate
// in-flight/last-activity state. The decrement + final timestamp update is
// registered as a defer immediately after the increment — before
// next.ServeHTTP is called — so it still runs if next.ServeHTTP panics;
// deferring it only after a plain post-ServeHTTP statement would skip it on
// that path and leave inFlight permanently elevated.
func (c *Controller) TrackActivity(origIdx int, next http.Handler) http.Handler {
	post := c.origToFiltered[origIdx]
	if post == -1 {
		panic(fmt.Sprintf("yield: TrackActivity: orig index %d has no Unloader (no _UNIT_NAME) — idle-tracking should never have been requested for it — this should have been caught at config.Load()", origIdx))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.inFlight[post].Add(1)
		c.lastDispatch[post].Store(time.Now().UnixNano())
		defer func() {
			c.inFlight[post].Add(-1)
			c.lastDispatch[post].Store(time.Now().UnixNano())
		}()
		// Wake-on-request: if this instance was idle-unloaded, the request
		// that wins the CompareAndSwap (idleUnloaded[post] true -> false) is
		// responsible for kicking off the reload. This is fire-and-forget —
		// the current request proceeds to next.ServeHTTP immediately,
		// regardless of the reload's progress, rather than blocking on it.
		// That is a deliberate, accepted cost: the waking request may hit a
		// connection error against the still-cold-starting upstream,
		// mirroring ADR-0014's existing accepted cost for Contention-
		// triggered wakes. A CAS loss (either not idle-unloaded, or another
		// concurrent request already won the wake) does nothing extra here.
		//
		// startAction requires c.mu (see its doc comment), but this hot
		// request path must not contend on the lock for the common case
		// where the CAS fails — so the lock is only acquired in the rare
		// CAS-won branch, held just long enough for startAction, then
		// released before spawning the goroutine, mirroring applyLocked/
		// checkIdleLocked's own startAction-then-spawn sequencing.
		if c.idleUnloaded[post].CompareAndSwap(true, false) {
			c.mu.Lock()
			// Units this instance displaces stop first; the reload waits
			// for them so the two never hold VRAM at once (ADR-0020).
			unitWaits := c.stopDisplacedLocked(r.Context(), post)
			a := c.startAction(post)
			c.mu.Unlock()
			if len(unitWaits) > 0 {
				a.wait = afterAll(append(unitWaits, a.wait)...)
			}
			go c.doReload(r.Context(), a, triggerIdle)
		}
		next.ServeHTTP(w, r)
	})
}

// checkIdleLocked fires a per-instance idle-unload for every post-filter
// instance i whose configured idleTimeouts[i] has elapsed since
// lastDispatch[i]. Called by refresh() immediately after applyLocked, still
// under c.mu, so idle checks always run strictly after — and observe the
// state resulting from — that tick's contention transition.
//
// Before the elapsed-time check, each instance is guarded by three early
// skips, in order, so an already-guarded-out instance does zero unnecessary
// work (no time.Since computation, no CAS attempt):
//   - c.effective (whole-Broker Yield is already active): an Idle-unload
//     must never fire while Contention-triggered Yield is in effect — that
//     path (applyLocked) already handles unloading every instance.
//   - c.inFlight[i] > 0: a request is currently in flight against this
//     instance, so it must not be unloaded out from under it.
//   - c.idleUnloaded[i] already true: dedup — this instance was already
//     idle-unloaded by an earlier tick, no need to re-evaluate or re-fire.
//
// idleTimeouts[i] == 0 (disabled) is skipped in O(1) with no atomic writes;
// otherwise idleUnloaded[i] is CompareAndSwap'd false->true, and only the
// goroutine that wins the CAS spawns doUnload — so even without the explicit
// already-fired guard above, this method cannot double-fire a single
// instance from one call. Caller holds c.mu.
func (c *Controller) checkIdleLocked(ctx context.Context) {
	for i := range c.idleTimeouts {
		if c.idleTimeouts[i] == 0 {
			continue
		}
		if c.effective {
			continue
		}
		if c.inFlight[i].Load() > 0 {
			continue
		}
		if c.idleUnloaded[i].Load() {
			continue
		}
		elapsed := time.Since(time.Unix(0, c.lastDispatch[i].Load()))
		if elapsed >= c.idleTimeouts[i] {
			if c.idleUnloaded[i].CompareAndSwap(false, true) {
				go c.doUnload(ctx, c.startAction(i), triggerIdle)
			}
		}
	}
}

// IdleSummaryEntry is a single instance's idle-unload status in IdleSummary.
type IdleSummaryEntry struct {
	Label             string `json:"label"`
	IdleTimeout       string `json:"idle_timeout"`
	IdleUnloaded      bool   `json:"idle_unloaded"`
	SinceLastDispatch string `json:"since_last_dispatch"`
}

// IdleSummary returns idle-unload status for instances that have it configured,
// or nil if no instance has idle-unload enabled. Each enabled instance gets one
// entry in the returned slice.
func (c *Controller) IdleSummary() any {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check if any instance has idle-unload configured
	hasAnyConfigured := false
	for _, timeout := range c.idleTimeouts {
		if timeout != 0 {
			hasAnyConfigured = true
			break
		}
	}

	// Return nil if no instance has idle-unload configured
	if !hasAnyConfigured {
		return nil
	}

	// Build a slice with entries for instances that have idle-unload enabled
	var result []IdleSummaryEntry
	for i, timeout := range c.idleTimeouts {
		if timeout == 0 {
			continue
		}
		entry := IdleSummaryEntry{
			Label:             c.labels[i],
			IdleTimeout:       timeout.String(),
			IdleUnloaded:      c.idleUnloaded[i].Load(),
			SinceLastDispatch: time.Since(time.Unix(0, c.lastDispatch[i].Load())).String(),
		}
		result = append(result, entry)
	}

	// Never return a non-nil empty slice (per task spec)
	if len(result) == 0 {
		return nil
	}

	return result
}
