package yield

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	mu           sync.Mutex
	called       int
	reloadCalled int
}

func (f *fakeUnloader) Unload(context.Context) error {
	f.mu.Lock()
	f.called++
	f.mu.Unlock()
	return nil
}

func (f *fakeUnloader) Reload(context.Context) error {
	f.mu.Lock()
	f.reloadCalled++
	f.mu.Unlock()
	return nil
}

func (f *fakeUnloader) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called
}

func (f *fakeUnloader) reloadCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloadCalled
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
	if !eventually(t, func() bool { return u.reloadCalls() == 1 }) {
		t.Fatalf("reloader called %d times, want 1", u.reloadCalls())
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
func (h *panicLogHandler) WithGroup(string) slog.Handler      { return h }

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
	c.refresh()
	if y, _ := c.Yielding(); !y {
		t.Fatal("setup: expected yielding before driving the clear transition")
	}

	// Transition back to clear spawns `go c.doReload()`, which panics inside
	// Reload and must recover instead of taking down the test process.
	d.set("", false)
	c.refresh()

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
	c.refresh()

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
	c.refresh()
	d.set("", false)
	c.refresh()

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
	c.refresh()

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
	c.refresh()
	if !eventually(t, func() bool { _, ok := u.get(); return ok }) {
		t.Fatal("doUnload never called Unload")
	}

	before := time.Now()
	det.set("", false)
	c.refresh()

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
	c.refresh()
	if !eventually(t, func() bool { return len(u.snapshot()) == 1 }) {
		t.Fatal("doUnload never called Unload")
	}

	// Clear immediately, while Unload is still blocked on release: this
	// spawns doReload before doUnload has finished.
	d.set("", false)
	c.refresh()

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
	c := NewWithConfirmMulti(d, []Unloader{u0, nil, u2}, nil, time.Hour, 1)

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
	c := NewWithConfirmMulti(d, []Unloader{u}, nil, time.Hour, 1)
	after := time.Now()

	got := time.Unix(0, c.lastDispatch[0].Load())
	if got.Before(before) || got.After(after) {
		t.Fatalf("lastDispatch[0] = %v, want between %v and %v (construction time, not the Unix epoch)", got, before, after)
	}

	// End-to-end: a short idle timeout must NOT fire on the very first
	// refresh() immediately after construction, with zero elapsed real time.
	c.ConfigureIdle([]time.Duration{time.Hour})
	c.refresh()
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
	c := NewWithConfirmMulti(d, []Unloader{u0, nil, u2}, nil, time.Hour, 1)

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
	c := NewWithConfirmMulti(d, []Unloader{u0, nil, u2}, nil, time.Hour, 1)

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
	c := NewWithConfirmMulti(d, []Unloader{u0}, nil, time.Hour, 1)

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

	c.refresh()

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
	c.refresh()
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

	c.refresh()
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

	c.refresh()

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

	c.checkIdleLocked()
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

	c.checkIdleLocked()
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

	c.checkIdleLocked()
	time.Sleep(20 * time.Millisecond)

	if got := u.calls(); got != 0 {
		t.Fatalf("Unload called %d times, want 0 (instance already idle-unloaded)", got)
	}
}

// TestYieldClearResetsIdleState proves applyLocked's eff==false (yield-clear)
// branch resets every configured instance's idle bookkeeping: an instance
// that was idle-unloaded before a Contention-triggered Yield started must
// come out of the yield-clear with lastDispatch reset to ~now and
// idleUnloaded reset to false, and must NOT be immediately re-idle-unloaded
// on the very next refresh (its fresh lastDispatch means elapsed time is
// ~0, well under the configured idle duration).
func TestYieldClearResetsIdleState(t *testing.T) {
	d := &fakeDet{}
	u := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u}, nil, time.Hour)
	// A long idle duration, paired with a far-backdated lastDispatch for the
	// setup idle-unload below: this makes the post-reset "must not
	// immediately re-fire" assertion robust against scheduling jitter
	// (elapsed since a ~now reset will be milliseconds, nowhere near 1 hour),
	// instead of racing a short duration against test execution time.
	c.ConfigureIdle([]time.Duration{time.Hour})

	// Idle-unload the instance directly via checkIdleLocked, the same way
	// the existing idle tests do.
	c.lastDispatch[0].Store(time.Now().Add(-2 * time.Hour).UnixNano())
	c.checkIdleLocked()
	if !eventually(t, func() bool { return u.calls() == 1 }) {
		t.Fatalf("setup: Unload called %d times, want 1 (idle-unload)", u.calls())
	}
	if !c.idleUnloaded[0].Load() {
		t.Fatal("setup: idleUnloaded[0] was not set true by checkIdleLocked")
	}

	// Trigger a Yield-start, then a Yield-clear. Yield-start unconditionally
	// unloads every configured instance (applyLocked's eff==true branch,
	// regardless of idleUnloaded), so u.calls() goes to 2 here — that is
	// expected setup, not what this test is proving.
	d.set("gaming-steam", true)
	c.refresh()
	if y, _ := c.Yielding(); !y {
		t.Fatal("setup: expected yielding after yield-start")
	}
	if !eventually(t, func() bool { return u.calls() == 2 }) {
		t.Fatalf("setup: Unload called %d times after yield-start, want 2 (idle-unload + yield-start unload)", u.calls())
	}
	callsAfterYieldStart := u.calls()

	before := time.Now()
	d.set("", false)
	c.refresh()
	if y, _ := c.Yielding(); y {
		t.Fatal("setup: expected cleared after yield-clear")
	}

	// lastDispatch[0] must be reset to ~now, and idleUnloaded[0] to false.
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was not reset to false on yield-clear")
	}
	got := time.Unix(0, c.lastDispatch[0].Load())
	if got.Before(before.Add(-time.Second)) || got.After(before.Add(time.Second)) {
		t.Fatalf("lastDispatch[0] = %v, want ~%v (reset to now on yield-clear)", got, before)
	}

	// The very next tick must not immediately re-idle-unload the instance:
	// its fresh lastDispatch means elapsed time is ~0, nowhere near the
	// configured 1-hour idle duration. u.calls() must stay exactly where it
	// was right after yield-start (no additional idle-triggered Unload).
	c.refresh()
	if got := u.calls(); got != callsAfterYieldStart {
		t.Fatalf("Unload called %d times after yield-clear + immediate refresh, want still %d (not immediately re-idle-unloaded)", got, callsAfterYieldStart)
	}
	if c.idleUnloaded[0].Load() {
		t.Fatal("idleUnloaded[0] was set true again immediately after yield-clear")
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

// TestMultiInstanceUnloadReloadBothFire proves NewMulti/NewWithConfirmMulti
// actually drive every configured instance, not just unloaders[0]: on a
// yield-start transition both instances' Unload must be called, and on the
// following clear transition both instances' Reload must be called.
func TestMultiInstanceUnloadReloadBothFire(t *testing.T) {
	d := &fakeDet{}
	u1 := &fakeUnloader{}
	u2 := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u1, u2}, nil, time.Hour)

	d.set("gaming-steam", true)
	c.refresh()
	if !eventually(t, func() bool { return u1.calls() == 1 }) {
		t.Fatalf("instance 1 Unload called %d times, want 1", u1.calls())
	}
	if !eventually(t, func() bool { return u2.calls() == 1 }) {
		t.Fatalf("instance 2 Unload called %d times, want 1", u2.calls())
	}

	d.set("", false)
	c.refresh()
	if !eventually(t, func() bool { return u1.reloadCalls() == 1 }) {
		t.Fatalf("instance 1 Reload called %d times, want 1", u1.reloadCalls())
	}
	if !eventually(t, func() bool { return u2.reloadCalls() == 1 }) {
		t.Fatalf("instance 2 Reload called %d times, want 1", u2.reloadCalls())
	}
}

// TestMultiInstanceFlapPreservesPerInstanceOrder is the multi-instance
// counterpart to TestActionsPreserveTransitionOrderAcrossFlap: it drives the
// exact same fast yield-then-clear-while-still-unloading race, but against
// two simultaneously configured instances, each with its own independent
// orderedUnloader (own release channel, own recorded order). It proves
// actionDone's per-index chaining (see yield.go) is truly independent per
// instance — a shared/global chain would still happen to pass a
// single-instance test, but would show up here as one instance's Reload
// leaking onto the wrong chain or waiting on the other instance's release.
func TestMultiInstanceFlapPreservesPerInstanceOrder(t *testing.T) {
	d := &fakeDet{}
	u1 := &orderedUnloader{release: make(chan struct{})}
	u2 := &orderedUnloader{release: make(chan struct{})}
	c := NewMulti(d, []Unloader{u1, u2}, nil, time.Hour)

	d.set("gaming-steam", true)
	c.refresh()
	if !eventually(t, func() bool { return len(u1.snapshot()) == 1 }) {
		t.Fatal("instance 1: doUnload never called Unload")
	}
	if !eventually(t, func() bool { return len(u2.snapshot()) == 1 }) {
		t.Fatal("instance 2: doUnload never called Unload")
	}

	// Clear immediately, while both instances' Unload are still blocked on
	// release: this spawns doReload for both instances before either
	// doUnload has finished.
	d.set("", false)
	c.refresh()

	// Give the scheduler every chance to run either doReload first if
	// nothing were ordering it against that same instance's still-pending
	// doUnload completion.
	time.Sleep(50 * time.Millisecond)
	if got := u1.snapshot(); len(got) != 1 || got[0] != "unload" {
		t.Fatalf("instance 1: Reload ran before Unload finished: order = %v, want [unload]", got)
	}
	if got := u2.snapshot(); len(got) != 1 || got[0] != "unload" {
		t.Fatalf("instance 2: Reload ran before Unload finished: order = %v, want [unload]", got)
	}

	close(u1.release)
	close(u2.release)

	if !eventually(t, func() bool {
		got := u1.snapshot()
		return len(got) == 2 && got[0] == "unload" && got[1] == "reload"
	}) {
		t.Fatalf("instance 1: final order = %v, want [unload reload]", u1.snapshot())
	}
	if !eventually(t, func() bool {
		got := u2.snapshot()
		return len(got) == 2 && got[0] == "unload" && got[1] == "reload"
	}) {
		t.Fatalf("instance 2: final order = %v, want [unload reload]", u2.snapshot())
	}
}

// slowUnloader's Unload records that it started, sleeps a short fixed delay,
// then records completion and returns nil — used as
// TestMultiInstanceOneInstanceErrorsDoesNotBlockOther's "instance B", so the
// test can distinguish "B's Unload eventually got called" (weak) from "B's
// Unload actually ran to completion promptly" (the real FR-13 isolation
// claim) instead of only observing a call count.
type slowUnloader struct {
	mu     sync.Mutex
	called int
	done   bool
	delay  time.Duration
}

func (s *slowUnloader) Unload(context.Context) error {
	s.mu.Lock()
	s.called++
	s.mu.Unlock()
	time.Sleep(s.delay)
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	return nil
}

func (s *slowUnloader) Reload(context.Context) error { return nil }

func (s *slowUnloader) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called
}

func (s *slowUnloader) isDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// TestMultiInstanceOneInstanceErrorsDoesNotBlockOther proves FR-13's
// independence guarantee: instance A permanently errors on Unload (returning
// immediately, simulating a jammed backend that will never succeed), while
// instance B's Unload takes a short but nonzero amount of time to complete.
// Because each configured instance gets its own actionDone chain and its own
// `go c.doUnload()` goroutine (see applyLocked/startAction in yield.go), A's
// error must never delay or block B: B's Unload must still be called and
// must still run to completion within its own delay, not stall waiting on A.
func TestMultiInstanceOneInstanceErrorsDoesNotBlockOther(t *testing.T) {
	d := &fakeDet{}
	a := &erroringUnloader{err: errors.New("instance A permanently jammed")}
	b := &slowUnloader{delay: 50 * time.Millisecond}
	c := NewMulti(d, []Unloader{a, b}, nil, time.Hour)

	start := time.Now()
	d.set("gaming-steam", true)
	c.refresh()

	if !eventually(t, func() bool { return a.calls() > 0 }) {
		t.Fatal("instance A (jammed): Unload never called")
	}
	if !eventually(t, func() bool { return b.calls() > 0 }) {
		t.Fatal("instance B: Unload never called — A's error should not block B from even starting")
	}
	if !eventually(t, func() bool { return b.isDone() }) {
		t.Fatal("instance B: Unload never completed — appears blocked by A's error")
	}

	// B's Unload only sleeps 50ms; give a generous bound (well under
	// eventually's 1s poll ceiling) to prove it wasn't stalled waiting on A.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("instance B completed after %v, want ~%v (not blocked by instance A)", elapsed, b.delay)
	}
}

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
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
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
		defer func() { recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
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
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

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
	c.checkIdleLocked()
	if !eventually(t, func() bool { return len(u.snapshot()) == 1 }) {
		t.Fatal("setup: doUnload never called Unload")
	}

	// Now dispatch a request while that Unload is still blocked on release:
	// this should win the wake CAS and spawn doReload, chained behind the
	// still-pending Unload.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	wrapped := c.TrackActivity(0, next)
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

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
			wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
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
	c := NewWithConfirmMulti(d, []Unloader{u0, nil}, nil, time.Hour, 1)

	defer func() {
		if recover() == nil {
			t.Fatal("TrackActivity did not panic for an orig index with no Unloader")
		}
	}()
	c.TrackActivity(1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
}
