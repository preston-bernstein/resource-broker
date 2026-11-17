package yield

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// TestOrigToFilteredMapsAcrossNilGap proves origToFiltered (built inside
// NewWithConfirmMulti's existing nil-filter loop) maps each ORIG (pre-filter)
// index to its POST-FILTER position, with -1 as the sentinel for an
// orig-index whose unloader was nil and got filtered out. Three orig
// positions with position 1 nil reproduces cmd/broker/main.go's numbering
// (0=default backend, 1/2=routes) with one route unconfigured.
func TestOrigToFilteredMapsAcrossNilGap(t *testing.T) {
	d := &fakeDet{}
	u0 := &fakeUnloader{}
	u2 := &fakeUnloader{}
	c := NewWithConfirmMulti(d, []Unloader{u0, nil, u2}, nil, Polling{Interval: time.Hour, ConfirmPolls: 1})

	if len(c.origToFiltered) != 3 {
		t.Fatalf("len(origToFiltered) = %d, want 3", len(c.origToFiltered))
	}
	if c.origToFiltered[0] != 0 {
		t.Errorf("origToFiltered[0] = %d, want 0", c.origToFiltered[0])
	}
	if c.origToFiltered[1] != -1 {
		t.Errorf("origToFiltered[1] = %d, want -1 (nil-filtered sentinel)", c.origToFiltered[1])
	}
	if c.origToFiltered[2] != 1 {
		t.Errorf("origToFiltered[2] = %d, want 1", c.origToFiltered[2])
	}
	if len(c.unloaders) != 2 {
		t.Fatalf("len(c.unloaders) = %d, want 2 (nil filtered out)", len(c.unloaders))
	}
}

// TestNewControllerInitializesLastDispatchToNow is the regression test for a
// construction-time bug: lastDispatch[i] must start at time.Now(), never at
// its atomic.Int64 zero-value (the Unix epoch). Before this fix, a freshly
// constructed Controller with an idle timeout configured but no traffic yet
// would have checkIdleLocked compute a multi-decade "elapsed" on its very
// first tick (Run's immediate first refresh(), before the polling ticker
// ever fires) and idle-unload the instance instantly, regardless of the
// configured timeout — including on every broker restart, immediately
// stopping a systemd unit that had been serving real traffic seconds
// earlier. See docs/vllm-idle-unload/plan.md's Risk-areas section, which
// documents "initialized to time.Now() at Controller construction" as the
// intended (and only acceptable) behavior.
func TestNewControllerInitializesLastDispatchToNow(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	before := time.Now()
	c := NewWithConfirmMulti(d, []Unloader{u}, nil, Polling{Interval: time.Hour, ConfirmPolls: 1})
	after := time.Now()

	got := time.Unix(0, c.lastDispatch[0].Load())
	if got.Before(before) || got.After(after) {
		t.Fatalf("lastDispatch[0] = %v, want between %v and %v (construction time, not the Unix epoch)", got, before, after)
	}

	// End-to-end: a short idle timeout must NOT fire on the very first
	// refresh() immediately after construction, with zero elapsed real time.
	c.ConfigureIdle([]time.Duration{time.Hour})
	c.refresh(context.Background())
	time.Sleep(20 * time.Millisecond)
	if got := u.calls(); got != 0 {
		t.Fatalf("Unload called %d times on a freshly-constructed Controller's first refresh, want 0 (zero-value lastDispatch would have made this fire instantly)", got)
	}
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] = true after a freshly-constructed Controller's first refresh, want false")
	}
}

// TestConfigureIdlePopulatesPostFilterSlice proves ConfigureIdle takes an
// ORIG-index-ordered slice and stores each entry at its POST-FILTER position
// in c.idleTimeouts, skipping the nil-filtered orig index (whose entry here
// is 0, so it does not panic).
func TestConfigureIdlePopulatesPostFilterSlice(t *testing.T) {
	d := &fakeDet{}
	u0 := &fakeUnloader{}
	u2 := &fakeUnloader{}
	c := NewWithConfirmMulti(d, []Unloader{u0, nil, u2}, nil, Polling{Interval: time.Hour, ConfirmPolls: 1})

	c.ConfigureIdle([]time.Duration{5 * time.Minute, 0, 10 * time.Minute})

	if len(c.idleTimeouts) != 2 {
		t.Fatalf("len(c.idleTimeouts) = %d, want 2", len(c.idleTimeouts))
	}
	if c.idleTimeouts[0] != 5*time.Minute {
		t.Errorf("idleTimeouts[0] = %v, want 5m (orig index 0)", c.idleTimeouts[0])
	}
	if c.idleTimeouts[1] != 10*time.Minute {
		t.Errorf("idleTimeouts[1] = %v, want 10m (orig index 2)", c.idleTimeouts[1])
	}
}

// TestConfigureIdleEmptySliceOnEmptyController proves ConfigureIdle does not
// panic when called with an empty slice against a Controller with zero
// configured instances (e.g. New(det, nil, interval)).
func TestConfigureIdleEmptySliceOnEmptyController(t *testing.T) {
	d := &fakeDet{}
	c := New(d, nil, time.Hour)
	c.ConfigureIdle(nil)
	if len(c.idleTimeouts) != 0 {
		t.Errorf("len(c.idleTimeouts) = %d, want 0", len(c.idleTimeouts))
	}
}

// TestConfigureIdlePanicsOnTimeoutForNilFilteredInstance proves ConfigureIdle
// panics when given a nonzero idle timeout at an orig index whose
// origToFiltered entry is -1 (the unloader at that position was nil and has
// no _UNIT_NAME to idle-unload) — a construction-time invariant violation
// config validation should already have prevented.
func TestConfigureIdlePanicsOnTimeoutForNilFilteredInstance(t *testing.T) {
	d := &fakeDet{}
	u0 := &fakeUnloader{}
	u2 := &fakeUnloader{}
	c := NewWithConfirmMulti(d, []Unloader{u0, nil, u2}, nil, Polling{Interval: time.Hour, ConfirmPolls: 1})

	defer func() {
		if recover() == nil {
			t.Fatal("ConfigureIdle did not panic on a nonzero timeout for a nil-filtered orig index")
		}
	}()
	c.ConfigureIdle([]time.Duration{0, time.Minute, 0})
}

// TestConfigureIdlePanicsOnLengthMismatch proves ConfigureIdle panics with a
// clear message (rather than either silently ignoring extra entries or
// index-out-of-bounds panicking with Go's generic message) when the caller's
// slice length doesn't match len(origToFiltered).
func TestConfigureIdlePanicsOnLengthMismatch(t *testing.T) {
	d := &fakeDet{}
	u0 := &fakeUnloader{}
	c := NewWithConfirmMulti(d, []Unloader{u0}, nil, Polling{Interval: time.Hour, ConfirmPolls: 1})

	defer func() {
		if recover() == nil {
			t.Fatal("ConfigureIdle did not panic on a length mismatch against origToFiltered")
		}
	}()
	c.ConfigureIdle([]time.Duration{time.Minute, time.Minute})
}

// TestCheckIdleLockedFiresAfterTimeoutElapses proves checkIdleLocked's base
// timer-check: an instance whose lastDispatch is far enough in the past that
// its configured idleTimeouts[i] has elapsed gets doUnload fired exactly
// once, with trigger=idle (as opposed to applyLocked's triggerYield).
func TestCheckIdleLockedFiresAfterTimeoutElapses(t *testing.T) {
	h := newPanicLogHandler("vram unload requested")
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{5 * time.Millisecond})
	c.lastDispatch[0].Store(time.Now().Add(-time.Hour).UnixNano())

	c.refresh(context.Background())

	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("checkIdleLocked never fired doUnload for an elapsed idle timeout")
	}
	if got := h.triggerValue(); got != string(triggerIdle) {
		t.Fatalf("trigger = %q, want %q", got, triggerIdle)
	}
	if !eventually(t, func() bool { return u.calls() == 1 }) {
		t.Fatalf("Unload called %d times, want 1", u.calls())
	}
	if !c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was not set true after firing")
	}

	// A second refresh must not fire again (the CAS in checkIdleLocked is
	// the only guard this task implements, but it alone must still prevent a
	// double-fire on repeated ticks).
	c.refresh(context.Background())
	time.Sleep(20 * time.Millisecond)
	if got := u.calls(); got != 1 {
		t.Fatalf("Unload called %d times after a second refresh, want still 1 (CAS must prevent re-fire)", got)
	}
}

// TestCheckIdleLockedDoesNotFireWithinWindow proves an instance whose
// lastDispatch is recent (well within its configured idleTimeouts[i]) is left
// alone: no doUnload call, no idleUnloaded flip.
func TestCheckIdleLockedDoesNotFireWithinWindow(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{time.Hour})
	c.lastDispatch[0].Store(time.Now().UnixNano())

	c.refresh(context.Background())
	time.Sleep(20 * time.Millisecond)

	if got := u.calls(); got != 0 {
		t.Fatalf("Unload called %d times, want 0 (idle window has not elapsed)", got)
	}
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was set true despite the idle window not having elapsed")
	}
}

// TestCheckIdleLockedOnlyFiresElapsedInstance configures two instances with
// different idle durations and proves only the one whose duration has
// actually elapsed fires — the other instance's Unload is never called and
// its idleUnloaded stays false.
func TestCheckIdleLockedOnlyFiresElapsedInstance(t *testing.T) {
	d := &fakeDet{}
	fast := &fakeUnloader{} // short idle timeout, elapsed
	slow := &fakeUnloader{} // long idle timeout, not elapsed
	c := NewMulti(d, []Unloader{fast, slow}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{5 * time.Millisecond, time.Hour})
	c.lastDispatch[0].Store(time.Now().Add(-time.Hour).UnixNano())
	c.lastDispatch[1].Store(time.Now().UnixNano())

	c.refresh(context.Background())

	if !eventually(t, func() bool { return fast.calls() == 1 }) {
		t.Fatalf("fast instance Unload called %d times, want 1", fast.calls())
	}
	time.Sleep(20 * time.Millisecond)
	if got := slow.calls(); got != 0 {
		t.Fatalf("slow instance Unload called %d times, want 0 (its idle window has not elapsed)", got)
	}
	if !c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] (fast) was not set true")
	}
	if c.idleUnloaded[1].Load() {
		t.Fatal("idleUnloaded[1] (slow) was set true despite its window not elapsing")
	}
}

// TestCheckIdleLockedSkipsWhenYieldEffective proves checkIdleLocked's
// Yield-active guard: an instance whose idle duration has clearly elapsed
// must not fire doUnload while c.effective is true (whole-Broker
// Contention-triggered Yield is already handling unloading via applyLocked).
func TestCheckIdleLockedSkipsWhenYieldEffective(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{5 * time.Millisecond})
	c.lastDispatch[0].Store(time.Now().Add(-time.Hour).UnixNano())
	c.effective = true

	c.checkIdleLocked(context.Background())
	time.Sleep(20 * time.Millisecond)

	if got := u.calls(); got != 0 {
		t.Fatalf("Unload called %d times, want 0 (Yield is effective, idle-unload must not fire)", got)
	}
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was set true despite Yield being effective")
	}
}

// TestCheckIdleLockedSkipsWhenInFlight proves checkIdleLocked's in-flight
// guard: an instance whose idle duration has elapsed must not be unloaded
// while a request is currently in flight against it.
func TestCheckIdleLockedSkipsWhenInFlight(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{5 * time.Millisecond})
	c.lastDispatch[0].Store(time.Now().Add(-time.Hour).UnixNano())
	c.inFlight[0].Store(1)

	c.checkIdleLocked(context.Background())
	time.Sleep(20 * time.Millisecond)

	if got := u.calls(); got != 0 {
		t.Fatalf("Unload called %d times, want 0 (instance has an in-flight request)", got)
	}
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was set true despite an in-flight request")
	}
}

// TestCheckIdleLockedSkipsWhenAlreadyIdleUnloaded proves checkIdleLocked's
// dedup guard: pre-setting idleUnloaded[i] true (simulating an earlier tick
// having already idle-unloaded this instance) must prevent a duplicate
// doUnload call on a later tick, even though the elapsed time still exceeds
// the configured duration.
func TestCheckIdleLockedSkipsWhenAlreadyIdleUnloaded(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{5 * time.Millisecond})
	c.lastDispatch[0].Store(time.Now().Add(-time.Hour).UnixNano())
	c.idleUnloaded[0].Store(true)

	c.checkIdleLocked(context.Background())
	time.Sleep(20 * time.Millisecond)

	if got := u.calls(); got != 0 {
		t.Fatalf("Unload called %d times, want 0 (instance already idle-unloaded)", got)
	}
}

// yieldCycle runs one Contention-triggered Yield-start and Yield-clear and
// waits for the Yield-start unload to land, so a test can then check what
// the clear did.
func yieldCycle(t *testing.T, c *Controller, d *fakeDet, u *fakeUnloader) {
	t.Helper()
	before := u.calls()
	d.set("plex", true)
	c.refresh(context.Background())
	if y, _ := c.Yielding(); !y {
		t.Fatal("setup: expected yielding after yield-start")
	}
	if !eventually(t, func() bool { return u.calls() == before+1 }) {
		t.Fatalf("setup: Unload called %d times after yield-start, want %d", u.calls(), before+1)
	}
	d.set("", false)
	c.refresh(context.Background())
	if y, _ := c.Yielding(); y {
		t.Fatal("setup: expected cleared after yield-clear")
	}
}

// TestYieldClearKeepsIdleUnloadedInstanceUnloaded proves a yield-clear does
// not reload an instance that was idle-unloaded before the Yield started.
// It stays idle-unloaded, and the next request wakes it (TrackActivity).
// Found 2026-10-20: Plex yielded about every 35 minutes, each clear reloaded
// vLLM and reset its 60m idle clock, so vLLM never idle-unloaded and held
// 9.5 GB of VRAM that pushed Ollama's models onto the CPU.
func TestYieldClearKeepsIdleUnloadedInstanceUnloaded(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{time.Hour})

	c.lastDispatch[0].Store(time.Now().Add(-2 * time.Hour).UnixNano())
	c.checkIdleLocked(context.Background())
	if !eventually(t, func() bool { return u.calls() == 1 }) {
		t.Fatalf("setup: Unload called %d times, want 1 (idle-unload)", u.calls())
	}
	stamp := c.lastDispatch[0].Load()

	yieldCycle(t, c, d, u)
	time.Sleep(20 * time.Millisecond)

	if got := u.reloadCalls(); got != 0 {
		t.Fatalf("Reload called %d times on yield-clear, want 0 (instance was idle-unloaded)", got)
	}
	if !c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was reset to false on yield-clear, want still true")
	}
	if got := c.lastDispatch[0].Load(); got != stamp {
		t.Fatal("lastDispatch[0] changed on yield-clear, want it untouched")
	}
}

// TestYieldClearDoesNotReloadInstanceIdlePastTimeout proves a yield-clear
// does not reload an instance whose last dispatch is older than its idle
// timeout, even when no idle tick fired before the Yield started. It is
// marked idle-unloaded so the next request wakes it.
func TestYieldClearDoesNotReloadInstanceIdlePastTimeout(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{time.Hour})
	c.lastDispatch[0].Store(time.Now().Add(-2 * time.Hour).UnixNano())

	yieldCycle(t, c, d, u)
	time.Sleep(20 * time.Millisecond)

	if got := u.reloadCalls(); got != 0 {
		t.Fatalf("Reload called %d times on yield-clear, want 0 (idle past its timeout)", got)
	}
	if !c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] = false after yield-clear, want true so a request wakes it")
	}
}

// TestYieldClearReloadsRecentlyUsedInstanceWithoutResettingClock proves a
// yield-clear reloads an instance used within its idle timeout, and keeps
// its last-dispatch time. A Yield is not traffic, so it must not restart
// the idle window.
func TestYieldClearReloadsRecentlyUsedInstanceWithoutResettingClock(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{time.Hour})
	stamp := time.Now().Add(-10 * time.Minute).UnixNano()
	c.lastDispatch[0].Store(stamp)

	yieldCycle(t, c, d, u)

	if !eventually(t, func() bool { return u.reloadCalls() == 1 }) {
		t.Fatalf("Reload called %d times on yield-clear, want 1 (recently used)", u.reloadCalls())
	}
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] = true after yield-clear of a recently used instance")
	}
	if got := c.lastDispatch[0].Load(); got != stamp {
		t.Fatal("lastDispatch[0] reset on yield-clear, want the last real dispatch kept")
	}
}

// TestYieldClearReloadsInstanceWithRequestInFlight proves a yield-clear
// reloads an instance that has a request in flight, even when its last
// dispatch is older than its idle timeout. Leaving it unloaded would serve
// that request against a stopped upstream with no wake pending.
func TestYieldClearReloadsInstanceWithRequestInFlight(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{time.Hour})
	c.lastDispatch[0].Store(time.Now().Add(-2 * time.Hour).UnixNano())
	c.inFlight[0].Add(1)

	yieldCycle(t, c, d, u)

	if !eventually(t, func() bool { return u.reloadCalls() == 1 }) {
		t.Fatalf("Reload called %d times on yield-clear, want 1 (request in flight)", u.reloadCalls())
	}
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] = true after yield-clear with a request in flight")
	}
}

// TestYieldClearReloadsInstanceWithoutIdleTimeout proves an instance with no
// idle timeout still reloads on every yield-clear, as before.
func TestYieldClearReloadsInstanceWithoutIdleTimeout(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.lastDispatch[0].Store(time.Now().Add(-2 * time.Hour).UnixNano())

	yieldCycle(t, c, d, u)

	if !eventually(t, func() bool { return u.reloadCalls() == 1 }) {
		t.Fatalf("Reload called %d times on yield-clear, want 1 (no idle timeout)", u.reloadCalls())
	}
}

func eventually(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
