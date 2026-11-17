package yield

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// panicUnloader.Unload always panics with panicVal — used to prove doUnload's
// recover() catches a panic (e.g. from a typed-nil Unloader implementation,
// see docs/openai-compatible-upstream-backend/plan.md's "Typed-nil safety"
// note) instead of crashing the process.
type panicUnloader struct {
	panicVal any
}

func (p *panicUnloader) Unload(context.Context) error {
	panic(p.panicVal)
}

// Reload also panics with panicVal, symmetric to Unload, so panicUnloader
// can drive TestDoReloadRecoversPanic (doReload's counterpart to
// TestDoUnloadRecoversPanic) the same way.
func (p *panicUnloader) Reload(context.Context) error {
	panic(p.panicVal)
}

// panicLogHandler is a minimal slog.Handler that signals doneCh the first
// time it observes a record with the given message, capturing its "recover"
// attribute. Used instead of a plain bytes.Buffer capture (as in
// internal/queue/gate_test.go) because doUnload runs in its own goroutine:
// a channel gives a race-free signal that the panic was actually logged
// (and, since the log call is the last thing doUnload's recover does before
// returning, that the goroutine is done) rather than requiring a sleep or a
// racy buffer read from the test goroutine.
type panicLogHandler struct {
	mu      sync.Mutex
	want    string
	got     bool
	recover any
	trigger string
	doneCh  chan struct{}
}

func newPanicLogHandler(wantMsg string) *panicLogHandler {
	return &panicLogHandler{want: wantMsg, doneCh: make(chan struct{})}
}

func (h *panicLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *panicLogHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != h.want {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.got {
		return nil
	}
	h.got = true
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "recover" {
			h.recover = a.Value.Any()
		}
		if a.Key == "trigger" {
			h.trigger = a.Value.String()
		}
		return true
	})
	close(h.doneCh)
	return nil
}

func (h *panicLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *panicLogHandler) WithGroup(string) slog.Handler { return h }

func (h *panicLogHandler) recoverValue() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recover
}

func (h *panicLogHandler) triggerValue() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trigger
}

// TestDoUnloadRecoversPanic proves the doUnload() hardening: if Unloader.Unload
// panics inside doUnload's unrecovered goroutine (applyLocked spawns it via
// `go c.doUnload()`), the panic must be recovered and logged rather than
// crashing the whole broker process. This guards specifically against a
// typed-nil Unloader defeating the `c.unloader != nil` interface check in
// applyLocked (see docs/openai-compatible-upstream-backend/plan.md).
func TestDoUnloadRecoversPanic(t *testing.T) {
	h := newPanicLogHandler("panic in vram unload")
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	d := &fakeDet{}
	panicVal := "boom: nil vram handle"
	u := &panicUnloader{panicVal: panicVal}
	c := New(d, u, time.Hour)

	// Transition into yielding spawns `go c.doUnload()`, which panics inside
	// Unload and must recover instead of taking down the test process.
	d.set("gaming-steam", true)
	c.refresh(context.Background())

	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("panic in doUnload was not recovered/logged within timeout — goroutine may have crashed or hung")
	}

	if got := h.recoverValue(); got != panicVal {
		t.Fatalf("logged recover value = %v, want %v", got, panicVal)
	}

	// The controller itself must remain fully functional after the recovered
	// panic — not wedged by the goroutine that panicked.
	if y, r := c.Yielding(); !y || r != "gaming-steam" {
		t.Fatalf("post-panic state: (%v,%q), want (true,\"gaming-steam\")", y, r)
	}
	d.set("", false)
	c.refresh(context.Background())
	if y, _ := c.Yielding(); y {
		t.Fatal("controller did not clear contention after a recovered doUnload panic")
	}
}

// TestDoReloadRecoversPanic mirrors TestDoUnloadRecoversPanic for doReload's
// counterpart recover(): if Unloader.Reload panics inside doReload's
// unrecovered goroutine (applyLocked spawns it via `go c.doReload()` on the
// clear transition), the panic must be recovered and logged rather than
// crashing the whole broker process.
func TestDoReloadRecoversPanic(t *testing.T) {
	h := newPanicLogHandler("panic in vram reload")
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	d := &fakeDet{}
	panicVal := "boom: nil vram handle"
	u := &panicUnloader{panicVal: panicVal}
	c := New(d, u, time.Hour)

	// Transition into yielding spawns `go c.doUnload()`, which also panics
	// (panicUnloader.Unload panics too) but that is doUnload's own hardening,
	// covered by TestDoUnloadRecoversPanic — irrelevant here beyond letting
	// the controller reach the yielding state so the clear transition below
	// can spawn doReload.
	d.set("gaming-steam", true)
	c.refresh(context.Background())
	if y, _ := c.Yielding(); !y {
		t.Fatal("setup: expected yielding before driving the clear transition")
	}

	// Transition back to clear spawns `go c.doReload()`, which panics inside
	// Reload and must recover instead of taking down the test process.
	d.set("", false)
	c.refresh(context.Background())

	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("panic in doReload was not recovered/logged within timeout — goroutine may have crashed or hung")
	}

	if got := h.recoverValue(); got != panicVal {
		t.Fatalf("logged recover value = %v, want %v", got, panicVal)
	}

	// The controller itself must remain fully functional after the recovered
	// panic — not wedged by the goroutine that panicked.
	if y, _ := c.Yielding(); y {
		t.Fatal("controller did not clear contention after a recovered doReload panic")
	}
}

// TestDoUnloadLogsErrorButDoesNotPanic drives doUnload's error-returned
// branch (as opposed to fakeUnloader's always-nil success, and
// panicUnloader's panic path) — no existing test exercised Unload returning
// a real error.
type erroringUnloader struct {
	mu     sync.Mutex
	called int
	err    error
}

func (e *erroringUnloader) Unload(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.called++
	return e.err
}

// Reload also returns e.err, symmetric to Unload, so erroringUnloader can
// drive TestDoReloadLogsErrorButDoesNotPanic (doReload's counterpart to
// TestDoUnloadLogsErrorButDoesNotPanic) the same way.
func (e *erroringUnloader) Reload(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.called++
	return e.err
}

func (e *erroringUnloader) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.called
}

func TestDoUnloadLogsErrorButDoesNotPanic(t *testing.T) {
	h := newPanicLogHandler("vram unload failed")
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	d := &fakeDet{}
	u := &erroringUnloader{err: errors.New("vram driver busy")}
	c := New(d, u, time.Hour)

	d.set("gaming-steam", true)
	c.refresh(context.Background())

	// The "vram unload failed" WARN log (not "vram unload requested" INFO) is
	// the only observable proof doUnload took the `err != nil` branch —
	// killing a `!= nil` -> `== nil` negation mutation that a mere call-count
	// assertion can't distinguish (both branches call Unload exactly once).
	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("doUnload never logged \"vram unload failed\" despite Unload returning an error")
	}
	if !eventually(t, func() bool { return u.calls() > 0 }) {
		t.Fatal("doUnload never invoked Unload despite entering yield")
	}
}

// TestDoUnloadLogsYieldTrigger proves a yield-sourced unload (the only
// trigger applyLocked ever passes as of this task) logs "vram unload
// requested" with trigger=yield, so an operator can distinguish a
// contention-driven unload from a future idle-timeout-driven one in the same
// log stream.
func TestDoUnloadLogsYieldTrigger(t *testing.T) {
	h := newPanicLogHandler("vram unload requested")
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	d := &fakeDet{}
	u := &fakeUnloader{}
	c := New(d, u, time.Hour)

	d.set("gaming-steam", true)
	c.refresh(context.Background())

	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("doUnload never logged \"vram unload requested\"")
	}
	if got := h.triggerValue(); got != string(triggerYield) {
		t.Fatalf("trigger = %q, want %q", got, triggerYield)
	}
}

// TestDoReloadLogsErrorButDoesNotPanic mirrors
// TestDoUnloadLogsErrorButDoesNotPanic for doReload's error-returned branch
// — no existing test exercised Reload returning a real error.
func TestDoReloadLogsErrorButDoesNotPanic(t *testing.T) {
	h := newPanicLogHandler("vram reload failed")
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prevLogger)

	d := &fakeDet{}
	u := &erroringUnloader{err: errors.New("vram driver busy")}
	c := New(d, u, time.Hour)

	// Reach yielding first (doUnload also calls Unload/gets e.err, but that's
	// not what this test observes), then clear to spawn doReload.
	d.set("gaming-steam", true)
	c.refresh(context.Background())
	d.set("", false)
	c.refresh(context.Background())

	// The "vram reload failed" WARN log (not "vram reload requested" INFO) is
	// the only observable proof doReload took the `err != nil` branch —
	// killing a `!= nil` -> `== nil` negation mutation that a mere call-count
	// assertion can't distinguish (both branches call Reload exactly once).
	select {
	case <-h.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("doReload never logged \"vram reload failed\" despite Reload returning an error")
	}
	if !eventually(t, func() bool { return u.calls() > 0 }) {
		t.Fatal("doReload never invoked Reload despite clearing yield")
	}
}

// TestDoUnloadUsesTenSecondTimeout pins the unloadReloadTimeout deadline
// doUnload gives Unload — a fake Unloader captures the context's deadline
// and asserts it is close to time.Now()+unloadReloadTimeout, killing an
// ARITHMETIC_BASE mutation on that constant that no other test's black-box
// behavior distinguishes from (say) a 1s or 100s timeout.
type deadlineCapturingUnloader struct {
	mu             sync.Mutex
	deadline       time.Time
	ok             bool
	reloadDeadline time.Time
	reloadOK       bool
}

func (d *deadlineCapturingUnloader) Unload(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline, d.ok = ctx.Deadline()
	return nil
}

// Reload captures its own deadline separately from Unload's, so a single
// deadlineCapturingUnloader can pin both doUnload's and doReload's timeout
// across a full yield-then-clear cycle.
func (d *deadlineCapturingUnloader) Reload(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reloadDeadline, d.reloadOK = ctx.Deadline()
	return nil
}

func (d *deadlineCapturingUnloader) get() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deadline, d.ok
}

func (d *deadlineCapturingUnloader) getReload() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reloadDeadline, d.reloadOK
}

func TestDoUnloadUsesUnloadReloadTimeout(t *testing.T) {
	det := &fakeDet{}
	u := &deadlineCapturingUnloader{}
	c := New(det, u, time.Hour)

	before := time.Now()
	det.set("gaming-steam", true)
	c.refresh(context.Background())

	if !eventually(t, func() bool { _, ok := u.get(); return ok }) {
		t.Fatal("doUnload never called Unload")
	}
	deadline, _ := u.get()
	wantMin := before.Add(unloadReloadTimeout - time.Second)
	wantMax := before.Add(unloadReloadTimeout + time.Second)
	if deadline.Before(wantMin) || deadline.After(wantMax) {
		t.Errorf("Unload deadline = %v, want ~%v from %v (between %v and %v)", deadline, unloadReloadTimeout, before, wantMin, wantMax)
	}
}

// TestDoReloadUsesUnloadReloadTimeout is doReload's counterpart to
// TestDoUnloadUsesUnloadReloadTimeout, pinning the same unloadReloadTimeout
// constant on the Reload call's context deadline.
func TestDoReloadUsesUnloadReloadTimeout(t *testing.T) {
	det := &fakeDet{}
	u := &deadlineCapturingUnloader{}
	c := New(det, u, time.Hour)

	// Reach yielding first so the clear transition below spawns doReload.
	det.set("gaming-steam", true)
	c.refresh(context.Background())
	if !eventually(t, func() bool { _, ok := u.get(); return ok }) {
		t.Fatal("doUnload never called Unload")
	}

	before := time.Now()
	det.set("", false)
	c.refresh(context.Background())

	if !eventually(t, func() bool { _, ok := u.getReload(); return ok }) {
		t.Fatal("doReload never called Reload")
	}
	deadline, _ := u.getReload()
	wantMin := before.Add(unloadReloadTimeout - time.Second)
	wantMax := before.Add(unloadReloadTimeout + time.Second)
	if deadline.Before(wantMin) || deadline.After(wantMax) {
		t.Errorf("Reload deadline = %v, want ~%v from %v (between %v and %v)", deadline, unloadReloadTimeout, before, wantMin, wantMax)
	}
}

// orderedUnloader records each Unload/Reload call's start, and lets Unload
// block on release until the test signals it — used to reproduce a fast
// yield-then-clear flap where the clear transition happens while the first
// Unload call is still in flight, and prove Reload never actually starts
// running until Unload finishes. systemdUnitController's own mutex (or any
// Unloader's internal locking) only prevents two calls from running
// concurrently; it says nothing about which one runs first if both are
// spawned as independent goroutines racing for that lock. actionDone (see
// yield.go) is what pins the order to match the transitions.
type orderedUnloader struct {
	mu      sync.Mutex
	order   []string
	release chan struct{}
}

func (o *orderedUnloader) Unload(context.Context) error {
	o.mu.Lock()
	o.order = append(o.order, "unload")
	o.mu.Unlock()
	<-o.release
	return nil
}

func (o *orderedUnloader) Reload(context.Context) error {
	o.mu.Lock()
	o.order = append(o.order, "reload")
	o.mu.Unlock()
	return nil
}

func (o *orderedUnloader) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.order...)
}

// TestActionsPreserveTransitionOrderAcrossFlap drives the exact flap
// scenario actionDone exists for: yield-start spawns doUnload, which blocks
// inside Unload; before it returns, yield-clear spawns doReload. Without
// actionDone chaining doReload to wait for doUnload's completion, nothing
// stops doReload's goroutine from calling Reload immediately — this test
// fails on that old behavior (Reload would be recorded well before Unload's
// release) and passes once ordering is enforced.
func TestActionsPreserveTransitionOrderAcrossFlap(t *testing.T) {
	d := &fakeDet{}
	u := &orderedUnloader{release: make(chan struct{})}
	c := New(d, u, time.Hour)

	d.set("gaming-steam", true)
	c.refresh(context.Background())
	if !eventually(t, func() bool { return len(u.snapshot()) == 1 }) {
		t.Fatal("doUnload never called Unload")
	}

	// Clear immediately, while Unload is still blocked on release: this
	// spawns doReload before doUnload has finished.
	d.set("", false)
	c.refresh(context.Background())

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

// fakeGPUManager records Pause/Resume calls and the deadline each call's
// context carried, so tests can pin both pauseGPUMgr's and resumeGPUMgr's
// 10s timeout literals (yield.go:270, :276) — no existing test drove
// SetGPUManager/pauseGPUMgr/resumeGPUMgr at all.
type fakeGPUManager struct {
	mu               sync.Mutex
	pauseCalls       int
	resumeCalls      int
	pauseDeadline    time.Time
	pauseDeadlineOK  bool
	resumeDeadline   time.Time
	resumeDeadlineOK bool
}

func (f *fakeGPUManager) PauseGPU(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCalls++
	f.pauseDeadline, f.pauseDeadlineOK = ctx.Deadline()
	return nil
}

func (f *fakeGPUManager) ResumeGPU(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	f.resumeDeadline, f.resumeDeadlineOK = ctx.Deadline()
	return nil
}

func (f *fakeGPUManager) snapshot() (pauseCalls, resumeCalls int, pauseOK, resumeOK bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauseCalls, f.resumeCalls, f.pauseDeadlineOK, f.resumeDeadlineOK
}

func TestGPUManagerPausedOnYieldAndResumedOnClear(t *testing.T) {
	d := &fakeDet{}
	c := New(d, nil, time.Hour)
	mgr := &fakeGPUManager{}
	c.SetGPUManager(mgr)

	beforePause := time.Now()
	d.set("gaming-steam", true)
	c.refresh(context.Background())
	if !eventually(t, func() bool { p, _, _, _ := mgr.snapshot(); return p > 0 }) {
		t.Fatal("PauseGPU was never called on yield transition")
	}
	_, _, pauseOK, _ := mgr.snapshot()
	if !pauseOK {
		t.Fatal("PauseGPU's context had no deadline, want ~10s timeout")
	}
	// Pins the 10s literal itself (yield.go:270), not just that some deadline
	// was set — an ARITHMETIC_BASE mutation of the constant wouldn't trip a
	// bare "has a deadline" check.
	pMin, pMax := beforePause.Add(9*time.Second), beforePause.Add(11*time.Second)
	if mgr.pauseDeadline.Before(pMin) || mgr.pauseDeadline.After(pMax) {
		t.Errorf("PauseGPU deadline = %v, want ~10s from %v (between %v and %v)", mgr.pauseDeadline, beforePause, pMin, pMax)
	}

	beforeResume := time.Now()
	d.set("", false)
	c.refresh(context.Background())
	if !eventually(t, func() bool { _, r, _, _ := mgr.snapshot(); return r > 0 }) {
		t.Fatal("ResumeGPU was never called on clear transition")
	}
	_, _, _, resumeOK := mgr.snapshot()
	if !resumeOK {
		t.Fatal("ResumeGPU's context had no deadline, want ~10s timeout")
	}
	rMin, rMax := beforeResume.Add(9*time.Second), beforeResume.Add(11*time.Second)
	if mgr.resumeDeadline.Before(rMin) || mgr.resumeDeadline.After(rMax) {
		t.Errorf("ResumeGPU deadline = %v, want ~10s from %v (between %v and %v)", mgr.resumeDeadline, beforeResume, rMin, rMax)
	}
}
