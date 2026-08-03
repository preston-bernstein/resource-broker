// Package yield tracks whether the broker should yield the GPU to gaming/Plex.
// Effective state combines a manual override with automatic detection. On a
// transition into yielding it cancels in-flight inference (via a serve context)
// and forces the upstream to unload models from VRAM.
package yield

import (
	"context"
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

// Unloader frees GPU memory on the upstream. Optional (may be nil).
type Unloader interface {
	Unload(ctx context.Context) error
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
	unloader     Unloader
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
func NewWithConfirm(det Detector, unloader Unloader, interval time.Duration, confirmPolls int) *Controller {
	if confirmPolls < 1 {
		confirmPolls = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Controller{
		det:          det,
		unloader:     unloader,
		interval:     interval,
		confirmPolls: confirmPolls,
		serveCtx:     ctx,
		serveCancel:  cancel,
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
// only the non-blocking work (serve-context swap, unload spawn) under the lock
// and returning the transition so the caller can log it after unlocking.
// Caller holds c.mu.
func (c *Controller) applyLocked() (event, reason string) {
	eff, r := c.computeLocked()
	if eff == c.effective {
		return "", ""
	}
	c.effective = eff
	mgr := c.gpuMgr
	if eff {
		c.serveCancel() // abort in-flight inference
		if c.unloader != nil {
			go c.doUnload()
		}
		if mgr != nil {
			go c.pauseGPUMgr(mgr)
		}
		return "start", r
	}
	c.serveCtx, c.serveCancel = context.WithCancel(context.Background())
	if mgr != nil {
		go c.resumeGPUMgr(mgr)
	}
	return "stop", ""
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

func (c *Controller) doUnload() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.unloader.Unload(ctx); err != nil {
		slog.Warn("vram unload failed", "err", err)
	} else {
		slog.Info("vram unload requested")
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
