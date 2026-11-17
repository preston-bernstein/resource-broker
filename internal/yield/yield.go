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
	"sync/atomic"
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

	// idleTimeouts[i], lastDispatch[i], inFlight[i], and idleUnloaded[i] are
	// index-aligned with c.unloaders/c.labels (the post-nil-filter index
	// space). They are populated by ConfigureIdle; idleTimeouts, lastDispatch,
	// and idleUnloaded are read/written by checkIdleLocked. inFlight and the
	// remaining dedup/wake behavior (TrackActivity, IdleSummary) are later
	// tasks.
	idleTimeouts []time.Duration
	lastDispatch []atomic.Int64
	inFlight     []atomic.Int32
	idleUnloaded []atomic.Bool

	// origToFiltered maps an ORIG (pre-nil-filter, caller-facing) unloaders
	// index to its position in the POST-FILTER c.unloaders/c.labels slices,
	// or -1 if unloaders[i] was nil and got filtered out. Sized to
	// len(unloaders) as originally passed into NewWithConfirmMulti, before
	// filtering — orig-index 0 is the default backend, 1..N are route
	// unloaders, matching cmd/broker/main.go's
	// `append([]yield.Unloader{be.Unloader()}, routeUnloaders...)` numbering.
	// Built once, in NewWithConfirmMulti's filter loop; read-only afterward,
	// so it is safe to read without holding c.mu. See ConfigureIdle.
	origToFiltered []int

	// Displacement (ADR-0020); see displace.go. Nil unless ConfigureDisplace
	// was called. Guarded by mu.
	displacers [][]int
	displaces  [][]int
	unitWant   []bool
	pollCount  int
	// Resume hold-off (ADR-0021): unitFreeSince[j] is when unit j's GPU
	// last became free, zero while it is not free.
	unitHoldoff   time.Duration
	unitFreeSince []time.Time
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
	return NewWithConfirmMulti(det, unloaders, nil, Polling{Interval: interval, ConfirmPolls: confirmPolls})
}

// NewMulti is NewWithConfirmMulti with confirmPolls=1 (the historical,
// pre-debounce behavior) — the multi-instance counterpart to New.
func NewMulti(det Detector, unloaders []Unloader, labels []string, interval time.Duration) *Controller {
	return NewWithConfirmMulti(det, unloaders, labels, Polling{Interval: interval, ConfirmPolls: 1})
}

// Polling is how a Controller polls its Detector: every Interval, entering
// yield after ConfirmPolls consecutive same-reason detections.
type Polling struct {
	Interval     time.Duration
	ConfirmPolls int
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
func NewWithConfirmMulti(det Detector, unloaders []Unloader, labels []string, polling Polling) *Controller {
	// `< 1` vs `<= 1` is an equivalent mutant here (verified 2026-08-15,
	// gremlins mutation testing): the clamp target is 1, so confirmPolls=1
	// produces c.confirmPolls=1 whether or not this branch is taken — no
	// input can observe a `<` vs `<=` difference. Left as `< 1` since that
	// reads correctly ("clamp anything below 1"); do not chase this survivor
	// with more tests, there is no test that can kill it.
	confirmPolls := polling.ConfirmPolls
	if confirmPolls < 1 {
		confirmPolls = 1
	}

	filteredUnloaders, filteredLabels, origToFiltered := filterUnloaders(unloaders, labels)

	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{
		det:            det,
		unloaders:      filteredUnloaders,
		labels:         filteredLabels,
		interval:       polling.Interval,
		confirmPolls:   confirmPolls,
		serveCtx:       ctx,
		serveCancel:    cancel,
		actionDone:     make([]chan struct{}, len(filteredUnloaders)),
		idleTimeouts:   make([]time.Duration, len(filteredUnloaders)),
		lastDispatch:   make([]atomic.Int64, len(filteredUnloaders)),
		inFlight:       make([]atomic.Int32, len(filteredUnloaders)),
		idleUnloaded:   make([]atomic.Bool, len(filteredUnloaders)),
		origToFiltered: origToFiltered,
	}
	// lastDispatch[i] must start at construction time ("now"), never at its
	// atomic.Int64 zero-value (Unix epoch, 1970-01-01) — otherwise the very
	// first checkIdleLocked tick (Run's immediate first refresh(), before
	// the polling ticker even fires) would compute a multi-decade elapsed
	// time for any idle-configured instance that has not yet served a
	// request, and idle-unload it instantly regardless of the configured
	// idle timeout — including a freshly (re)started broker instantly
	// stopping a systemd unit it just as instantly needs to reload for the
	// next request. This is the "initialized to time.Now() at Controller
	// construction" behavior docs/vllm-idle-unload/plan.md's Risk-areas
	// section documents as the intended trade-off (a restart resets the
	// idle clock to "process start," not "true last real usage" — an
	// accepted, low-cost trade-off) — not "starts at the epoch."
	now := time.Now().UnixNano()
	for i := range c.lastDispatch {
		c.lastDispatch[i].Store(now)
	}
	return c
}

// filterUnloaders drops nil entries from unloaders and labels the rest. It
// returns the kept unloaders and their labels, plus origToFiltered: for each
// original index, its position in the kept slices or -1 if it was dropped.
func filterUnloaders(unloaders []Unloader, labels []string) (kept []Unloader, keptLabels []string, origToFiltered []int) {
	origToFiltered = make([]int, len(unloaders))
	for i, u := range unloaders {
		if u == nil {
			origToFiltered[i] = -1
			continue
		}
		label := ""
		if i < len(labels) {
			label = labels[i]
		}
		if label == "" {
			label = fmt.Sprintf("instance[%d]", i)
		}
		origToFiltered[i] = len(kept)
		kept = append(kept, u)
		keptLabels = append(keptLabels, label)
	}
	return kept, keptLabels, origToFiltered
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
	c.refresh(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh(ctx)
		}
	}
}

func (c *Controller) refresh(ctx context.Context) {
	reason, contended := c.det.Detect()
	c.mu.Lock()
	c.autoContended, c.autoReason = c.debounceLocked(reason, contended)
	c.lastPoll = time.Now()
	event, r := c.applyLocked(ctx)
	c.checkIdleLocked(ctx)
	c.pollCount++
	c.reconcileDisplacedLocked(ctx)
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
	event, r := c.applyLocked(context.Background())
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
func (c *Controller) applyLocked(ctx context.Context) (event, reason string) {
	eff, r := c.computeLocked()
	if eff == c.effective {
		return "", ""
	}
	c.effective = eff
	if eff {
		c.enterYieldLocked(ctx)
		return "start", r
	}
	c.leaveYieldLocked(ctx)
	return "stop", ""
}

// enterYieldLocked aborts in-flight inference and unloads every instance.
func (c *Controller) enterYieldLocked(ctx context.Context) {
	c.serveCancel() // abort in-flight inference
	for i := range c.unloaders {
		go c.doUnload(ctx, c.startAction(i), triggerYield)
		if c.isDisplaced(i) {
			c.unitWant[i] = false
		}
	}
	if mgr := c.gpuMgr; mgr != nil {
		go c.pauseGPUMgr(ctx, mgr)
	}
}

// leaveYieldLocked resumes service: a fresh serve context, and a reload for
// each instance that should come back.
func (c *Controller) leaveYieldLocked(ctx context.Context) {
	c.serveCtx, c.serveCancel = context.WithCancel(context.Background())
	for i := range c.unloaders {
		// A displaced unit is started by reconcileDisplacedLocked, which runs
		// later in this same refresh and only if no displacer holds the GPU.
		if c.isDisplaced(i) || !c.reloadAfterYieldLocked(i) {
			continue
		}
		go c.doReload(ctx, c.startAction(i), triggerYield)
	}
	if mgr := c.gpuMgr; mgr != nil {
		go c.resumeGPUMgr(ctx, mgr)
	}
}

// reloadAfterYieldLocked reports whether instance i reloads when a Yield
// clears. An instance with an idle timeout reloads only if it was in use
// before the Yield. One that was idle-unloaded, or has been idle past its
// timeout, stays unloaded and is marked idle-unloaded, so TrackActivity wakes
// it on the next request. lastDispatch is never touched here: a Yield is not
// traffic. Before 2026-10-20 every clear reloaded every instance and reset its
// idle clock, so Plex yields every ~35 minutes kept an unused vLLM resident
// forever (see ADR-0016's 2026-10-20 amendment). Idle time includes the Yield
// itself. Same index range as checkIdleLocked; c.idleTimeouts,
// c.lastDispatch, c.inFlight, and c.idleUnloaded are all sized to
// len(c.unloaders).
func (c *Controller) reloadAfterYieldLocked(i int) bool {
	if c.idleTimeouts[i] <= 0 || c.inFlight[i].Load() != 0 {
		return true
	}
	stamp := c.lastDispatch[i].Load()
	if !c.idleUnloaded[i].Load() && time.Since(time.Unix(0, stamp)) < c.idleTimeouts[i] {
		return true
	}
	c.idleUnloaded[i].Store(true)
	// TrackActivity runs without c.mu. A request that started after the reads
	// above saw idleUnloaded false and lost its wake. Re-check; if one
	// arrived, take the wake back here. The CAS keeps it to one reload if a
	// later request won it.
	if c.inFlight[i].Load() == 0 && c.lastDispatch[i].Load() == stamp {
		return false
	}
	return c.idleUnloaded[i].CompareAndSwap(true, false)
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
