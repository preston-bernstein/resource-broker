// Package yield tracks whether the broker should yield the GPU to gaming/Plex.
// Effective state combines a manual override with automatic detection. On a
// transition into yielding it cancels in-flight inference (via a serve context)
// and forces the upstream to unload models from VRAM.
package yield

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

// Mode is the operator override.
type Mode int

const (
	// ModeAuto follows detection (default).
	ModeAuto Mode = iota
	// ModeForceYield always yields, regardless of detection.
	ModeForceYield
	// ModeForceServe never yields, regardless of detection.
	ModeForceServe
)

func (m Mode) String() string {
	switch m {
	case ModeForceYield:
		return "yield"
	case ModeForceServe:
		return "serve"
	default:
		return "auto"
	}
}

// ParseMode maps a string to a Mode.
func ParseMode(s string) (Mode, bool) {
	switch s {
	case "auto":
		return ModeAuto, true
	case "yield":
		return ModeForceYield, true
	case "serve":
		return ModeForceServe, true
	default:
		return ModeAuto, false
	}
}

// Detector reports automatic contention.
type Detector interface {
	Detect() (reason string, contended bool)
}

// Unloader frees GPU memory on the upstream and reloads it back. Optional
// (may be nil).
type Unloader interface {
	Unload(ctx context.Context) error
	Reload(ctx context.Context) error
}

// unloadReloadTimeout bounds how long doUnload/doReload will wait for the
// Unloader call before giving up client-side. It is a give-up bound only:
// cancelling exec.CommandContext when this timeout fires kills the local
// client process, but does not cancel a systemd job already queued via
// D-Bus on the upstream — that job runs to completion (or failure)
// independently of whether our client gave up waiting on it. Callers of
// Unloader should not treat this timeout as proof the underlying
// unload/reload actually stopped.
const unloadReloadTimeout = 30 * time.Second

// GPUManager is an optional external GPU consumer (e.g. Tdarr) that the broker
// pauses when gaming/Plex takes the GPU and resumes when contention clears.
// Priority: gaming/Plex > Ollama inference > GPUManager background work.
type GPUManager interface {
	PauseGPU(ctx context.Context) error
	ResumeGPU(ctx context.Context) error
}

// Controller holds the effective yield state, refreshed by a polling loop.
type Controller struct {
	det          Detector
	unloaders    []Unloader // non-nil only; filtered at construction, see newMulti
	labels       []string   // labels[i] identifies unloaders[i] in log lines; same length as unloaders
	gpuMgr       GPUManager // optional; nil = disabled
	interval     time.Duration
	confirmPolls int // consecutive same-reason detections required to enter yield

	mu            sync.Mutex
	mode          Mode
	autoContended bool
	autoReason    string
	effective     bool
	lastPoll      time.Time // set by refresh(); zero until Run's first tick
	confirmCount  int
	confirmReason string

	// serveCtx is alive while NOT yielding; cancelled the instant yielding
	// begins so in-flight upstream calls abort. A fresh one is made when
	// serving resumes.
	serveCtx    context.Context
	serveCancel context.CancelFunc

	// actionDone[i], when non-nil, is closed once the most recently spawned
	// doUnload/doReload goroutine for unloaders[i] has finished its
	// Unload/Reload call. Each new spawn (in applyLocked, via startAction)
	// captures the current value as its own predecessor-wait channel before
	// replacing it with a fresh one. This is read and written only while
	// holding mu (both here in startAction and by applyLocked's caller), so
	// there is no race on the slice or its elements.
	//
	// It exists to fix a real ordering gap: systemdUnitController's own
	// mutex only prevents Unload and Reload from *overlapping* on the same
	// instance — it does not guarantee they run in the order the yield/clear
	// transitions happened, because `go c.doUnload()` and a later
	// `go c.doReload()` are independent goroutine spawns with no ordering
	// relationship between when they actually start executing. Under a fast
	// contention flap (yield then clear before the first systemctl call has
	// even been scheduled), the Go scheduler could let the second goroutine
	// acquire systemdUnitController's mutex first, running `systemctl start`
	// before `systemctl stop` — leaving the unit stopped even though the
	// controller's effective state is "not yielding". Chaining each action
	// on its predecessor's completion (see startAction, doUnload, doReload)
	// removes that race without making applyLocked itself block.
	//
	// One chain per configured instance (index i) so a slow or permanently
	// failing instance never blocks or reorders another instance's chain —
	// each unloaders[i] gets its own fully independent ADR-0014 ordering
	// guarantee.
	actionDone []chan struct{}
}

// New returns a Controller in ModeAuto, not yielding, that acts on the first
// detection (confirmPolls=1) — the historical, pre-debounce behavior. Use
// NewWithConfirm to require sustained detection before yielding.
func New(det Detector, unloader Unloader, interval time.Duration) *Controller {
	return NewWithConfirm(det, unloader, interval, 1)
}

// NewWithConfirm returns a Controller that only enters yield after
// confirmPolls consecutive polls report the same contention reason. This
// filters brief single-poll process-match blips (launcher background
// housekeeping subprocesses that transiently match a gaming regex but are
// not sustained gameplay — see docs/adr and the 2026-07-15 research doc)
// without weakening ADR-0003's hard-yield response once contention is
// confirmed real. Clearing contention is never debounced: recovery only
// benefits inference and never risks starving gaming/Plex, so it should be
// as fast as detection allows. confirmPolls < 1 is treated as 1.
//
// New/NewWithConfirm are thin one-element wrappers around NewMulti/
// NewWithConfirmMulti (see those for the general N-instance form used by
// per-model backend routing) — they exist so every pre-existing
// single-instance caller and test keeps compiling and behaving identically.
func NewWithConfirm(det Detector, unloader Unloader, interval time.Duration, confirmPolls int) *Controller {
	var unloaders []Unloader
	if unloader != nil {
		unloaders = []Unloader{unloader}
	}
	return NewWithConfirmMulti(det, unloaders, nil, interval, confirmPolls)
}

// NewMulti is NewWithConfirmMulti with confirmPolls=1 (the historical,
// pre-debounce behavior) — the multi-instance counterpart to New.
func NewMulti(det Detector, unloaders []Unloader, labels []string, interval time.Duration) *Controller {
	return NewWithConfirmMulti(det, unloaders, labels, interval, 1)
}

// NewWithConfirmMulti generalizes NewWithConfirm to N independently-ordered
// Unloader instances — one per configured backend route. Each unloaders[i]
// gets its own ADR-0014 doUnload/doReload ordering chain (see actionDone's
// doc comment): a slow or permanently failing instance never blocks or
// reorders another instance's actions.
//
// unloaders is filtered at construction using the same direct
// interface-nil check ADR-0014 documents (`u != nil`), applied per element:
// a literal nil entry is dropped, since it carries nothing to act on. A
// typed nil (an interface value wrapping a nil concrete pointer) is
// deliberately NOT filtered here — it does not compare equal to nil, so it
// passes this check exactly as the old single-instance `c.unloader != nil`
// guard in applyLocked always did, and is left for doUnload/doReload's own
// recover() to catch if it ever panics on a nil receiver. This preserves
// the existing typed-nil-Unloader regression behavior unchanged (see
// internal/backend's TestOpenAIBackendUnloader* tests).
//
// labels[i] identifies unloaders[i] in doUnload/doReload's log lines, so an
// operator can tell which configured instance failed to unload/reload
// during an incident. labels is caller-supplied opaque data (e.g. a unit
// name, upstream URL, or route index) and may be nil, shorter than
// unloaders, or contain empty entries; any label missing or empty defaults
// to fmt.Sprintf("instance[%d]", i), where i is the position in the
// (pre-filter) unloaders argument — the position the caller configured it
// at, which stays stable even if an earlier entry gets filtered out.
// confirmPolls < 1 is treated as 1 (see NewWithConfirm).
func NewWithConfirmMulti(det Detector, unloaders []Unloader, labels []string, interval time.Duration, confirmPolls int) *Controller {
	// `< 1` vs `<= 1` is an equivalent mutant here (verified 2026-08-15,
	// gremlins mutation testing): the clamp target is 1, so confirmPolls=1
	// produces c.confirmPolls=1 whether or not this branch is taken — no
	// input can observe a `<` vs `<=` difference. Left as `< 1` since that
	// reads correctly ("clamp anything below 1"); do not chase this survivor
	// with more tests, there is no test that can kill it.
	if confirmPolls < 1 {
		confirmPolls = 1
	}

	var filteredUnloaders []Unloader
	var filteredLabels []string
	for i, u := range unloaders {
		if u == nil {
			continue
		}
		label := ""
		if i < len(labels) {
			label = labels[i]
		}
		if label == "" {
			label = fmt.Sprintf("instance[%d]", i)
		}
		filteredUnloaders = append(filteredUnloaders, u)
		filteredLabels = append(filteredLabels, label)
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Controller{
		det:          det,
		unloaders:    filteredUnloaders,
		labels:       filteredLabels,
		interval:     interval,
		confirmPolls: confirmPolls,
		serveCtx:     ctx,
		serveCancel:  cancel,
		actionDone:   make([]chan struct{}, len(filteredUnloaders)),
	}
}

// SetGPUManager registers an external GPU consumer to pause/resume alongside
// gaming-yield transitions. Safe to call before Run.
func (c *Controller) SetGPUManager(m GPUManager) {
	c.mu.Lock()
	c.gpuMgr = m
	c.mu.Unlock()
}

// Run polls the detector until ctx is cancelled, refreshing once immediately.
func (c *Controller) Run(ctx context.Context) {
	c.refresh()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh()
		}
	}
}

func (c *Controller) refresh() {
	reason, contended := c.det.Detect()
	c.mu.Lock()
	c.autoContended, c.autoReason = c.debounceLocked(reason, contended)
	c.lastPoll = time.Now()
	event, r := c.applyLocked()
	c.mu.Unlock()
	logTransition(event, r)
}

// PollAge reports how long it has been since the detector loop last actually
// ran (Run's first refresh, or its most recent tick). Zero time.Duration
// forever means Run was never started at all. /healthz uses this to catch a
// wedged or never-started detection loop — a dependency failure the process
// being "up" says nothing about (see internal/admin's health check).
func (c *Controller) PollAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastPoll.IsZero() {
		return time.Duration(math.MaxInt64)
	}
	return time.Since(c.lastPoll)
}

// debounceLocked requires confirmPolls consecutive detections of the same
// reason before reporting contention upward. A cleared detection or a
// change in reason resets the count immediately — flapping between two
// reasons (e.g. "gaming-steam" then "gaming-wine") must not accumulate
// confirmation for either. Caller holds c.mu.
func (c *Controller) debounceLocked(reason string, contended bool) (bool, string) {
	if !contended {
		c.confirmCount, c.confirmReason = 0, ""
		return false, ""
	}
	if reason == c.confirmReason {
		c.confirmCount++
	} else {
		c.confirmReason, c.confirmCount = reason, 1
	}
	if c.confirmCount < c.confirmPolls {
		return false, ""
	}
	return true, reason
}

// SetMode applies an operator override.
func (c *Controller) SetMode(m Mode) {
	c.mu.Lock()
	c.mode = m
	event, r := c.applyLocked()
	c.mu.Unlock()
	slog.Info("yield mode", "mode", m.String())
	logTransition(event, r)
}

// applyLocked recomputes the effective state and acts on a transition, doing
// only the non-blocking work (serve-context swap, unload spawns) under the
// lock and returning the transition so the caller can log it after
// unlocking. Every configured instance is spawned independently — each
// unloaders[i] gets its own doUnload/doReload goroutine chained only to its
// own actionDone[i] predecessor, so no instance can block or reorder
// another's actions (see actionDone's doc comment). Caller holds c.mu.
func (c *Controller) applyLocked() (event, reason string) {
	eff, r := c.computeLocked()
	if eff == c.effective {
		return "", ""
	}
	c.effective = eff
	mgr := c.gpuMgr
	if eff {
		c.serveCancel() // abort in-flight inference
		for i := range c.unloaders {
			wait, done := c.startAction(i)
			go c.doUnload(i, wait, done)
		}
		if mgr != nil {
			go c.pauseGPUMgr(mgr)
		}
		return "start", r
	}
	c.serveCtx, c.serveCancel = context.WithCancel(context.Background())
	for i := range c.unloaders {
		wait, done := c.startAction(i)
		go c.doReload(i, wait, done)
	}
	if mgr != nil {
		go c.resumeGPUMgr(mgr)
	}
	return "stop", ""
}

// startAction records a new pending unload/reload action for unloaders[i]
// and hands back the two channels doUnload/doReload need: wait (that
// instance's previous action's done channel, nil if this is the first
// action ever for instance i) and done (this action's own completion
// channel, which the caller must close when finished). Must only be called
// from applyLocked, while c.mu is held, so the handoff chain it builds
// matches applyLocked's own call order exactly — see actionDone's doc
// comment for why this ordering matters.
func (c *Controller) startAction(i int) (wait <-chan struct{}, done chan struct{}) {
	wait = c.actionDone[i]
	done = make(chan struct{})
	c.actionDone[i] = done
	return wait, done
}

// logTransition logs at WARN, not INFO: this is the single most
// operationally significant state change the broker has — every game
// stutter or ingest stall investigation starts by grepping for it (see
// broker-diagnostics-and-tooling's grep recipe) — and at INFO it was
// indistinguishable from the high-volume per-request access log
// (internal/queue/gate.go), which drowns it out (2026-08-01 audit,
// "log-levels" finding).
func logTransition(event, reason string) {
	switch event {
	case "start":
		slog.Warn("yield start", "reason", reason, "action", "cancel in-flight + unload VRAM")
	case "stop":
		slog.Warn("yield stop", "action", "resume service")
	}
}

// doUnload runs in its own goroutine (see applyLocked), so a panic here would
// otherwise crash the whole broker process — e.g. a typed-nil Unloader (an
// interface value that passes NewWithConfirmMulti's `u != nil` filter at
// construction but wraps a nil concrete pointer, see docs/openai-compatible-upstream-backend/plan.md's
// "Typed-nil safety" note) invoking Unload on a nil receiver. The recover
// here is cheap defense-in-depth against exactly that unrecovered-goroutine
// panic path.
//
// i identifies which configured instance this call belongs to; c.unloaders
// and c.labels are only ever read here (never mutated after construction),
// so indexing them without holding c.mu is safe. wait/done chain this
// action to its same-instance neighbors (see actionDone's doc comment and
// startAction): doUnload waits for instance i's previous action to finish
// before calling Unload, and always closes done on the way out (even on
// panic/recover) so instance i's next action isn't stuck waiting forever —
// this has no effect on any other instance's chain.
func (c *Controller) doUnload(i int, wait <-chan struct{}, done chan struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in vram unload", "instance", c.labels[i], "recover", r)
		}
	}()
	if wait != nil {
		<-wait
	}
	ctx, cancel := context.WithTimeout(context.Background(), unloadReloadTimeout)
	defer cancel()
	if err := c.unloaders[i].Unload(ctx); err != nil {
		slog.Warn("vram unload failed", "instance", c.labels[i], "err", err)
	} else {
		slog.Info("vram unload requested", "instance", c.labels[i])
	}
}

// doReload runs in its own goroutine (see applyLocked), so a panic here
// would otherwise crash the whole broker process — see doUnload's comment
// for the typed-nil Unloader hazard this guards against and for what i,
// wait, and done do.
func (c *Controller) doReload(i int, wait <-chan struct{}, done chan struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in vram reload", "instance", c.labels[i], "recover", r)
		}
	}()
	if wait != nil {
		<-wait
	}
	ctx, cancel := context.WithTimeout(context.Background(), unloadReloadTimeout)
	defer cancel()
	if err := c.unloaders[i].Reload(ctx); err != nil {
		slog.Warn("vram reload failed", "instance", c.labels[i], "err", err)
	} else {
		slog.Info("vram reload requested", "instance", c.labels[i])
	}
}

func (c *Controller) pauseGPUMgr(m GPUManager) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = m.PauseGPU(ctx)
}

func (c *Controller) resumeGPUMgr(m GPUManager) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = m.ResumeGPU(ctx)
}

func (c *Controller) computeLocked() (bool, string) {
	switch c.mode {
	case ModeForceYield:
		return true, "manual"
	case ModeForceServe:
		return false, ""
	default:
		return c.autoContended, c.autoReason
	}
}

// Yielding reports the effective state and a human reason.
func (c *Controller) Yielding() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.computeLocked()
}

// ServeContext returns the context that is cancelled when yielding begins.
// Gated requests derive their upstream context from it so they abort on yield.
func (c *Controller) ServeContext() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serveCtx
}

// State is a snapshot for the control/status endpoints.
type State struct {
	Mode       string `json:"mode"`
	Yielding   bool   `json:"yielding"`
	Reason     string `json:"reason"`
	AutoReason string `json:"auto_reason"`
}

// Snapshot returns the current state.
func (c *Controller) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	yielding, reason := c.computeLocked()
	return State{Mode: c.mode.String(), Yielding: yielding, Reason: reason, AutoReason: c.autoReason}
}
