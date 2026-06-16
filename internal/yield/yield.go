// Package yield tracks whether the broker should yield the GPU to gaming/Plex.
// Effective state combines a manual override with automatic detection. On a
// transition into yielding it cancels in-flight inference (via a serve context)
// and forces the upstream to unload models from VRAM.
package yield

import (
	"context"
	"log"
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

// Controller holds the effective yield state, refreshed by a polling loop.
type Controller struct {
	det      Detector
	unloader Unloader
	interval time.Duration

	mu            sync.Mutex
	mode          Mode
	autoContended bool
	autoReason    string
	effective     bool

	// serveCtx is alive while NOT yielding; cancelled the instant yielding
	// begins so in-flight upstream calls abort. A fresh one is made when
	// serving resumes.
	serveCtx    context.Context
	serveCancel context.CancelFunc
}

// New returns a Controller in ModeAuto, not yielding.
func New(det Detector, unloader Unloader, interval time.Duration) *Controller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Controller{
		det:         det,
		unloader:    unloader,
		interval:    interval,
		serveCtx:    ctx,
		serveCancel: cancel,
	}
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
	c.autoContended, c.autoReason = contended, reason
	c.applyLocked()
	c.mu.Unlock()
}

// SetMode applies an operator override.
func (c *Controller) SetMode(m Mode) {
	c.mu.Lock()
	c.mode = m
	c.applyLocked()
	c.mu.Unlock()
	log.Printf("yield: mode set to %s", m)
}

// applyLocked recomputes the effective state and acts on a transition. Caller
// holds c.mu.
func (c *Controller) applyLocked() {
	eff, reason := c.computeLocked()
	if eff == c.effective {
		return
	}
	c.effective = eff
	if eff {
		log.Printf("yield: START (%s) — cancelling in-flight, unloading VRAM", reason)
		c.serveCancel() // abort in-flight inference
		if c.unloader != nil {
			go c.doUnload()
		}
	} else {
		log.Print("yield: STOP — resuming service")
		c.serveCtx, c.serveCancel = context.WithCancel(context.Background())
	}
}

func (c *Controller) doUnload() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.unloader.Unload(ctx); err != nil {
		log.Printf("yield: VRAM unload error: %v", err)
	} else {
		log.Print("yield: VRAM unload requested")
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
