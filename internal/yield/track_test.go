package yield

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTrackActivityUpdatesInFlightAndLastDispatch proves TrackActivity's
// plain bookkeeping: while the wrapped handler is running, inFlight[post]
// must read 1 and lastDispatch[post] must have moved to ~now; once it
// returns, inFlight[post] must be back to 0 and lastDispatch[post] must have
// moved again (the defer's second Store).
func TestTrackActivityUpdatesInFlightAndLastDispatch(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)

	release := make(chan struct{})
	entered := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})

	beforeStart := time.Now()
	h := c.TrackActivity(0, next)

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
		close(done)
	}()

	<-entered
	if got := c.inFlight[0].Load(); got != 1 {
		t.Fatalf("inFlight[0] during request = %d, want 1", got)
	}
	mid := time.Unix(0, c.lastDispatch[0].Load())
	if mid.Before(beforeStart.Add(-time.Second)) || mid.After(time.Now().Add(time.Second)) {
		t.Fatalf("lastDispatch[0] during request = %v, want ~now", mid)
	}

	beforeRelease := time.Now()
	close(release)
	<-done

	if got := c.inFlight[0].Load(); got != 0 {
		t.Fatalf("inFlight[0] after request = %d, want 0", got)
	}
	end := time.Unix(0, c.lastDispatch[0].Load())
	if end.Before(beforeRelease.Add(-time.Second)) || end.After(time.Now().Add(time.Second)) {
		t.Fatalf("lastDispatch[0] after request = %v, want ~now (updated again by the defer)", end)
	}
}

// TestTrackActivityDecrementsInFlightOnPanic proves the defer registered
// immediately after the increment (not a plain statement after
// next.ServeHTTP) still runs when next.ServeHTTP panics — the direct proof
// the decrement is actually guarded by a defer and not skipped on the panic
// path.
func TestTrackActivityDecrementsInFlightOnPanic(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := c.TrackActivity(0, next)

	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
	}()

	if got := c.inFlight[0].Load(); got != 0 {
		t.Fatalf("inFlight[0] after panicking request = %d, want 0 (defer must still decrement)", got)
	}
}

// TestTrackActivityPanicsOnNilFilteredOrigIndex proves TrackActivity panics
// at wrap time (not per-request) when asked to track an orig-index whose
// unloader was nil-filtered — mirroring ConfigureIdle's symmetric
// construction-time invariant check.
// TestTrackActivityWakesIdleUnloadedInstance proves TrackActivity's wake-on-
// request branch: pre-setting idleUnloaded[post] true (simulating a prior
// idle-unload) and then dispatching a request through the wrapped handler
// must flip idleUnloaded back to false via the CAS and fire doReload with
// triggerIdle — the request itself proceeds normally regardless.
func TestTrackActivityWakesIdleUnloadedInstance(t *testing.T) {
	h := newPanicLogHandler("vram reload requested")
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.idleUnloaded[0].Store(true)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	wrapped := c.TrackActivity(0, next)
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))

	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was not flipped false by the wake CAS")
	}

	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("TrackActivity's wake branch never fired doReload (\"vram reload requested\" never logged)")
	}
	if got := h.triggerValue(); got != string(triggerIdle) {
		t.Fatalf("trigger = %q, want %q", got, triggerIdle)
	}
	if !eventually(t, func() bool { return u.reloadCalls() == 1 }) {
		t.Fatalf("Reload called %d times, want 1", u.reloadCalls())
	}
}

// TestTrackActivityWakeWaitsForPriorUnload proves the wake branch's doReload
// is chained onto the same instance's still-in-flight predecessor doUnload
// via actionDone, exactly like TestActionsPreserveTransitionOrderAcrossFlap
// proves for Contention's own applyLocked flap: a prior idle-triggered
// doUnload that hasn't finished yet (blocked on orderedUnloader.release) must
// complete before the wake's doReload actually calls Reload, even though both
// goroutines are spawned essentially back-to-back.
func TestTrackActivityWakeWaitsForPriorUnload(t *testing.T) {
	d := &fakeDet{}
	u := &orderedUnloader{release: make(chan struct{})}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.ConfigureIdle([]time.Duration{5 * time.Millisecond})

	// Drive a real idle-unload via checkIdleLocked, the same way existing
	// idle tests do, so idleUnloaded[0] is true via the CAS path and
	// actionDone[0] is populated with the in-flight doUnload's done channel.
	c.lastDispatch[0].Store(time.Now().Add(-time.Hour).UnixNano())
	c.checkIdleLocked(context.Background())
	if !eventually(t, func() bool { return len(u.snapshot()) == 1 }) {
		t.Fatal("setup: doUnload never called Unload")
	}

	// Now dispatch a request while that Unload is still blocked on release:
	// this should win the wake CAS and spawn doReload, chained behind the
	// still-pending Unload.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	wrapped := c.TrackActivity(0, next)
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))

	// Give the scheduler every chance to run doReload first if nothing were
	// ordering it against doUnload's still-pending completion.
	time.Sleep(50 * time.Millisecond)
	if got := u.snapshot(); len(got) != 1 || got[0] != "unload" {
		t.Fatalf("Reload ran before Unload finished: order = %v, want [unload]", got)
	}

	close(u.release)

	if !eventually(t, func() bool {
		got := u.snapshot()
		return len(got) == 2 && got[0] == "unload" && got[1] == "reload"
	}) {
		t.Fatalf("final order = %v, want [unload reload]", u.snapshot())
	}
}

// TestTrackActivityWakeConcurrentRequestsFireOnce proves the wake CAS is the
// sole arbiter when two requests race against the same freshly-idle-unloaded
// instance: only one of them may win CompareAndSwap(true, false) and spawn
// doReload, even though both requests still proceed through next.ServeHTTP
// normally. Two goroutines released simultaneously via a shared start
// channel maximizes the chance of an actual race, rather than relying on
// scheduling luck from calling ServeHTTP twice in sequence.
func TestTrackActivityWakeConcurrentRequestsFireOnce(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	c.idleUnloaded[0].Store(true)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	wrapped := c.TrackActivity(0, next)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), "GET", "/", nil))
		}()
	}
	close(start)
	wg.Wait()

	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was not flipped false by the winning CAS")
	}
	if !eventually(t, func() bool { return u.reloadCalls() == 1 }) {
		t.Fatalf("Reload called %d times, want exactly 1 (only the CAS winner should wake)", u.reloadCalls())
	}
	// Give any erroneous second wake a chance to fire before declaring victory.
	time.Sleep(50 * time.Millisecond)
	if got := u.reloadCalls(); got != 1 {
		t.Fatalf("Reload called %d times after settling, want exactly 1 (CAS loser must not also wake)", got)
	}
}

func TestTrackActivityPanicsOnNilFilteredOrigIndex(t *testing.T) {
	d := &fakeDet{}
	u0 := &fakeUnloader{}
	c := NewWithConfirmMulti(d, []Unloader{u0, nil}, nil, Polling{Interval: time.Hour, ConfirmPolls: 1})

	defer func() {
		if recover() == nil {
			t.Fatal("TrackActivity did not panic for an orig index with no Unloader")
		}
	}()
	c.TrackActivity(1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
}

// TestIdleSummaryWithOneConfiguredInstance proves IdleSummary returns a
// single-entry slice when one instance has idle-unload configured, with
// correct label, idle_timeout, idle_unloaded, and since_last_dispatch values.
func TestIdleSummaryWithOneConfiguredInstance(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, []string{"test-instance"}, time.Hour)
	c.ConfigureIdle([]time.Duration{5 * time.Minute})

	// Set lastDispatch to a known point in the past for predictable since calculation
	pastTime := time.Now().Add(-10 * time.Second)
	c.lastDispatch[0].Store(pastTime.UnixNano())
	c.idleUnloaded[0].Store(true)

	result := c.IdleSummary()
	if result == nil {
		t.Fatal("IdleSummary returned nil, want a non-nil slice")
	}

	entries, ok := result.([]IdleSummaryEntry)
	if !ok {
		t.Fatalf("IdleSummary returned type %T, want []IdleSummaryEntry", result)
	}

	if len(entries) != 1 {
		t.Fatalf("IdleSummary returned %d entries, want 1", len(entries))
	}

	entry := entries[0]
	if entry.Label != "test-instance" {
		t.Errorf("label = %q, want %q", entry.Label, "test-instance")
	}
	if entry.IdleTimeout != (5 * time.Minute).String() {
		t.Errorf("idle_timeout = %q, want %q", entry.IdleTimeout, (5 * time.Minute).String())
	}
	if !entry.IdleUnloaded {
		t.Error("idle_unloaded = false, want true")
	}

	// since_last_dispatch should be close to 10 seconds (within 1 second tolerance)
	elapsed, err := time.ParseDuration(entry.SinceLastDispatch)
	if err != nil {
		t.Fatalf("failed to parse since_last_dispatch %q: %v", entry.SinceLastDispatch, err)
	}
	expectedMin := 9 * time.Second
	expectedMax := 11 * time.Second
	if elapsed < expectedMin || elapsed > expectedMax {
		t.Errorf("since_last_dispatch = %v, want ~10s (between %v and %v)", elapsed, expectedMin, expectedMax)
	}
}

// TestIdleSummaryWithZeroConfiguredInstances proves IdleSummary returns nil
// when no instance has idle-unload configured (all idleTimeouts are 0).
func TestIdleSummaryWithZeroConfiguredInstances(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	// Don't call ConfigureIdle, so idleTimeouts[0] remains 0

	result := c.IdleSummary()
	if result != nil {
		t.Fatalf("IdleSummary returned %v, want nil", result)
	}
}

// TestIdleSummaryJSONMarshaling proves the returned slice marshals to valid JSON
// with the expected snake_case field keys.
func TestIdleSummaryJSONMarshaling(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, []string{"my-label"}, time.Hour)
	c.ConfigureIdle([]time.Duration{time.Hour})

	c.lastDispatch[0].Store(time.Now().UnixNano())
	c.idleUnloaded[0].Store(false)

	result := c.IdleSummary()
	if result == nil {
		t.Fatal("IdleSummary returned nil, want a non-nil slice for JSON marshaling test")
	}

	// Marshal to JSON
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	jsonStr := string(data)

	// Verify snake_case keys are present
	expectedKeys := []string{"label", "idle_timeout", "idle_unloaded", "since_last_dispatch"}
	for _, key := range expectedKeys {
		if !contains(jsonStr, `"`+key+`"`) {
			t.Errorf("JSON output missing key %q: %s", key, jsonStr)
		}
	}

	// Verify the label value is correct
	if !contains(jsonStr, "my-label") {
		t.Errorf("JSON output missing label value \"my-label\": %s", jsonStr)
	}
}

// contains is a helper to check if a substring exists in a string.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
