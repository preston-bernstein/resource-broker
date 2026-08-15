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
	"net/http"
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

// unloadReloadTimeout bounds how long doUnload/doReload will wait for the
// Unloader call before giving up client-side. It is a give-up bound only:
// cancelling exec.CommandContext when this timeout fires kills the local
// client process, but does not cancel a systemd job already queued via
// D-Bus on the upstream — that job runs to completion (or failure)
// independently of whether our client gave up waiting on it. Callers of
// Unloader should not treat this timeout as proof the underlying
// unload/reload actually stopped.
const unloadReloadTimeout = 30 * time.Second

// unloadTrigger identifies what caused a doUnload/doReload call, so its log
// line tells an operator whether GPU contention (yield) or a per-instance
// idle timeout (idle) drove the action.
type unloadTrigger string

const (
	// triggerYield is a gaming/Plex contention transition, driven by
	// applyLocked.
	triggerYield unloadTrigger = "yield"
	// triggerIdle is a per-instance idle-unload timeout, driven by
	// checkIdleLocked.
	triggerIdle unloadTrigger = "idle"
)

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
	origToFiltered := make([]int, len(unloaders))
	postCount := 0
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
		filteredUnloaders = append(filteredUnloaders, u)
		filteredLabels = append(filteredLabels, label)
		origToFiltered[i] = postCount
		postCount++
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{
		det:            det,
		unloaders:      filteredUnloaders,
		labels:         filteredLabels,
		interval:       interval,
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
			wait, done := c.startAction(post)
			c.mu.Unlock()
			go c.doReload(post, wait, done, triggerIdle)
		}
		next.ServeHTTP(w, r)
	})
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
	c.checkIdleLocked()
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
			go c.doUnload(i, wait, done, triggerYield)
		}
		if mgr != nil {
			go c.pauseGPUMgr(mgr)
		}
		return "start", r
	}
	c.serveCtx, c.serveCancel = context.WithCancel(context.Background())
	for i := range c.unloaders {
		wait, done := c.startAction(i)
		go c.doReload(i, wait, done, triggerYield)
	}
	// A just-cleared (post-Yield) instance must not be immediately eligible
	// for a fresh Idle-unload before it has had any chance to serve real
	// traffic: reset lastDispatch[i] to "now" so its idle window starts
	// counting from when it actually became available again, and reset
	// idleUnloaded[i] to false so its state is consistent regardless of
	// whether it was previously idle-unloaded, Contention-unloaded (via the
	// yield-start branch above), or never unloaded at all — every configured
	// instance is treated as "fresh" on yield-clear. Same index range
	// checkIdleLocked iterates (0..len(c.idleTimeouts)-1); c.idleTimeouts,
	// c.lastDispatch, and c.idleUnloaded are all sized to len(c.unloaders).
	for i := range c.idleTimeouts {
		c.lastDispatch[i].Store(time.Now().UnixNano())
		c.idleUnloaded[i].Store(false)
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
// while c.mu is held, from applyLocked, checkIdleLocked, or TrackActivity's
// CAS-won wake branch (which acquires c.mu only for this case), so the
// handoff chain it builds matches those callers' own call order exactly —
// see actionDone's doc comment for why this ordering matters.
func (c *Controller) startAction(i int) (wait <-chan struct{}, done chan struct{}) {
	wait = c.actionDone[i]
	done = make(chan struct{})
	c.actionDone[i] = done
	return wait, done
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
func (c *Controller) checkIdleLocked() {
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
				wait, done := c.startAction(i)
				go c.doUnload(i, wait, done, triggerIdle)
			}
		}
	}
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
// this has no effect on any other instance's chain. trigger identifies what
// caused this call (yield-contention vs idle-timeout) and is only logged,
// never branched on.
func (c *Controller) doUnload(i int, wait <-chan struct{}, done chan struct{}, trigger unloadTrigger) {
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
		slog.Warn("vram unload failed", "instance", c.labels[i], "err", err, "trigger", string(trigger))
	} else {
		slog.Info("vram unload requested", "instance", c.labels[i], "trigger", string(trigger))
	}
}

// doReload runs in its own goroutine (see applyLocked), so a panic here
// would otherwise crash the whole broker process — see doUnload's comment
// for the typed-nil Unloader hazard this guards against and for what i,
// wait, done, and trigger do.
func (c *Controller) doReload(i int, wait <-chan struct{}, done chan struct{}, trigger unloadTrigger) {
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
		slog.Warn("vram reload failed", "instance", c.labels[i], "err", err, "trigger", string(trigger))
	} else {
		slog.Info("vram reload requested", "instance", c.labels[i], "trigger", string(trigger))
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

// IdleSummaryEntry is a single instance's idle-unload status in IdleSummary.
type IdleSummaryEntry struct {
	Label             string `json:"label"`
	IdleTimeout       string `json:"idle_timeout"`
	IdleUnloaded      bool   `json:"idle_unloaded"`
	SinceLastDispatch string `json:"since_last_dispatch"`
}

// Snapshot returns the current state.
func (c *Controller) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	yielding, reason := c.computeLocked()
	return State{Mode: c.mode.String(), Yielding: yielding, Reason: reason, AutoReason: c.autoReason}
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
