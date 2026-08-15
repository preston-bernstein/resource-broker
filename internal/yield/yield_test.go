package yield

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeDet struct {
	mu     sync.Mutex
	reason string
	cont   bool
}

func (f *fakeDet) set(reason string, cont bool) {
	f.mu.Lock()
	f.reason, f.cont = reason, cont
	f.mu.Unlock()
}

func (f *fakeDet) Detect() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason, f.cont
}

func TestModeOverrides(t *testing.T) {
	d := &fakeDet{}
	c := New(d, nil, time.Hour)

	// Auto follows detection.
	d.set("gaming-steam", true)
	c.refresh()
	if y, r := c.Yielding(); !y || r != "gaming-steam" {
		t.Fatalf("auto+contended: (%v,%q)", y, r)
	}
	d.set("", false)
	c.refresh()
	if y, _ := c.Yielding(); y {
		t.Fatal("auto+clear should not yield")
	}

	// Force yield wins even when detection is clear.
	c.SetMode(ModeForceYield)
	if y, r := c.Yielding(); !y || r != "manual" {
		t.Fatalf("force-yield: (%v,%q)", y, r)
	}

	// Force serve wins even when detection is contended.
	d.set("plex", true)
	c.refresh()
	c.SetMode(ModeForceServe)
	if y, _ := c.Yielding(); y {
		t.Fatal("force-serve should not yield despite contention")
	}
}

func TestRunPolls(t *testing.T) {
	d := &fakeDet{}
	c := New(d, nil, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	d.set("gaming-wine", true)
	if !eventually(t, func() bool { y, _ := c.Yielding(); return y }) {
		t.Fatal("controller did not pick up contention from poll loop")
	}
	d.set("", false)
	if !eventually(t, func() bool { y, _ := c.Yielding(); return !y }) {
		t.Fatal("controller did not clear contention from poll loop")
	}
}

type fakeUnloader struct {
	mu     sync.Mutex
	called int
}

func (f *fakeUnloader) Unload(context.Context) error {
	f.mu.Lock()
	f.called++
	f.mu.Unlock()
	return nil
}

func (f *fakeUnloader) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called
}

func TestYieldTransitionCancelsServeAndUnloads(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := New(d, u, time.Hour)

	serve := c.ServeContext()
	select {
	case <-serve.Done():
		t.Fatal("serve context cancelled before yielding")
	default:
	}

	// Transition into yielding.
	d.set("gaming-steam", true)
	c.refresh()

	select {
	case <-serve.Done():
	case <-time.After(time.Second):
		t.Fatal("serve context not cancelled on yield start")
	}
	if !eventually(t, func() bool { return u.calls() == 1 }) {
		t.Fatalf("unloader called %d times, want 1", u.calls())
	}

	// Resuming gives a fresh, live serve context.
	d.set("", false)
	c.refresh()
	fresh := c.ServeContext()
	select {
	case <-fresh.Done():
		t.Fatal("fresh serve context already cancelled")
	default:
	}
}

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
		return true
	})
	close(h.doneCh)
	return nil
}

func (h *panicLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *panicLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *panicLogHandler) recoverValue() any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recover
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
	c.refresh()

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
	c.refresh()
	if y, _ := c.Yielding(); y {
		t.Fatal("controller did not clear contention after a recovered doUnload panic")
	}
}

// TestPollAge proves the /healthz staleness signal: before Run/refresh ever
// happens PollAge is "infinite" (never polled — a wedged or never-started
// detection loop), and after a refresh it drops to a small, real duration.
func TestPollAge(t *testing.T) {
	d := &fakeDet{}
	c := New(d, nil, time.Hour)

	if age := c.PollAge(); age < 24*time.Hour {
		t.Fatalf("PollAge before any refresh = %v, want a very large sentinel", age)
	}

	c.refresh()
	if age := c.PollAge(); age < 0 || age > time.Second {
		t.Fatalf("PollAge just after refresh = %v, want ~0", age)
	}
}

func TestConfirmPollsDebouncesTransientMatch(t *testing.T) {
	d := &fakeDet{}
	c := NewWithConfirm(d, nil, time.Hour, 3)

	// A single-poll blip (launcher background housekeeping) must not yield.
	d.set("gaming-heroic", true)
	c.refresh()
	if y, _ := c.Yielding(); y {
		t.Fatal("single detection should not yield with confirmPolls=3")
	}
	d.set("", false)
	c.refresh()
	if y, _ := c.Yielding(); y {
		t.Fatal("clearing after one blip should not yield")
	}

	// Sustained detection across confirmPolls consecutive refreshes must yield.
	d.set("gaming-heroic", true)
	c.refresh()
	c.refresh()
	if y, _ := c.Yielding(); y {
		t.Fatal("should not yield before confirmPolls consecutive detections")
	}
	c.refresh()
	if y, r := c.Yielding(); !y || r != "gaming-heroic" {
		t.Fatalf("should yield on the confirmPolls-th consecutive detection: (%v,%q)", y, r)
	}
}

func TestConfirmPollsResetsOnReasonChange(t *testing.T) {
	d := &fakeDet{}
	c := NewWithConfirm(d, nil, time.Hour, 2)

	d.set("gaming-heroic", true)
	c.refresh()
	d.set("gaming-wine", true) // different reason: must not carry over the count
	c.refresh()
	if y, _ := c.Yielding(); y {
		t.Fatal("a reason change should reset confirmation, not accumulate")
	}
	c.refresh() // second consecutive "gaming-wine"
	if y, r := c.Yielding(); !y || r != "gaming-wine" {
		t.Fatalf("should yield after confirmPolls consecutive detections of the new reason: (%v,%q)", y, r)
	}
}

func TestConfirmPollsClearIsInstant(t *testing.T) {
	d := &fakeDet{}
	c := NewWithConfirm(d, nil, time.Hour, 3)

	d.set("plex", true)
	c.refresh()
	c.refresh()
	c.refresh()
	if y, _ := c.Yielding(); !y {
		t.Fatal("setup: expected yielding after confirmPolls detections")
	}

	d.set("", false)
	c.refresh() // recovery must not be debounced
	if y, _ := c.Yielding(); y {
		t.Fatal("clearing contention should take effect on the very next poll")
	}
}

// TestNewWithConfirmClampsBelowOne proves confirmPolls < 1 is clamped to 1
// (per NewWithConfirm's doc comment) at the exact boundary — confirmPolls=1
// must behave identically to confirmPolls=0, and confirmPolls=1 passed
// through unchanged must yield on the very first detection (no debounce at
// all). No existing test drove NewWithConfirm with 0 or 1.
func TestNewWithConfirmClampsBelowOne(t *testing.T) {
	d0 := &fakeDet{}
	c0 := NewWithConfirm(d0, nil, time.Hour, 0)
	d0.set("gaming-steam", true)
	c0.refresh()
	if y, r := c0.Yielding(); !y || r != "gaming-steam" {
		t.Fatalf("confirmPolls=0 (clamped to 1): (%v,%q), want immediate yield", y, r)
	}

	d1 := &fakeDet{}
	c1 := NewWithConfirm(d1, nil, time.Hour, 1)
	d1.set("gaming-steam", true)
	c1.refresh()
	if y, r := c1.Yielding(); !y || r != "gaming-steam" {
		t.Fatalf("confirmPolls=1: (%v,%q), want immediate yield", y, r)
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
	c.refresh()

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

// TestDoUnloadUsesTenSecondTimeout pins the 10s deadline doUnload gives
// Unload (yield.go:260) — a fake Unloader captures the context's deadline
// and asserts it is close to time.Now()+10s, killing an ARITHMETIC_BASE
// mutation on that literal that no other test's black-box behavior
// distinguishes from (say) a 1s or 100s timeout.
type deadlineCapturingUnloader struct {
	mu       sync.Mutex
	deadline time.Time
	ok       bool
}

func (d *deadlineCapturingUnloader) Unload(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline, d.ok = ctx.Deadline()
	return nil
}

func (d *deadlineCapturingUnloader) get() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deadline, d.ok
}

func TestDoUnloadUsesTenSecondTimeout(t *testing.T) {
	det := &fakeDet{}
	u := &deadlineCapturingUnloader{}
	c := New(det, u, time.Hour)

	before := time.Now()
	det.set("gaming-steam", true)
	c.refresh()

	if !eventually(t, func() bool { _, ok := u.get(); return ok }) {
		t.Fatal("doUnload never called Unload")
	}
	deadline, _ := u.get()
	wantMin := before.Add(9 * time.Second)
	wantMax := before.Add(11 * time.Second)
	if deadline.Before(wantMin) || deadline.After(wantMax) {
		t.Errorf("Unload deadline = %v, want ~10s from %v (between %v and %v)", deadline, before, wantMin, wantMax)
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
	c.refresh()
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
	c.refresh()
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

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"auto": ModeAuto, "yield": ModeForceYield, "serve": ModeForceServe} {
		if m, ok := ParseMode(in); !ok || m != want {
			t.Errorf("ParseMode(%q) = (%v,%v)", in, m, ok)
		}
	}
	if _, ok := ParseMode("bogus"); ok {
		t.Error("ParseMode(bogus) should fail")
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
