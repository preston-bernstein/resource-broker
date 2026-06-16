// Package yield tracks whether the broker should yield the GPU to gaming/Plex.
// Effective state combines a manual override with automatic detection.
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

// Controller holds the effective yield state, refreshed by a polling loop.
type Controller struct {
	det      Detector
	interval time.Duration

	mu            sync.RWMutex
	mode          Mode
	autoContended bool
	autoReason    string
}

// New returns a Controller in ModeAuto.
func New(det Detector, interval time.Duration) *Controller {
	return &Controller{det: det, interval: interval}
}

// Run polls the detector until ctx is cancelled. It refreshes once immediately
// so state is valid before the first tick.
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
	prev, prevReason := c.autoContended, c.autoReason
	c.autoContended, c.autoReason = contended, reason
	c.mu.Unlock()
	if contended != prev || (contended && reason != prevReason) {
		if contended {
			log.Printf("yield: contention detected (%s)", reason)
		} else {
			log.Print("yield: contention cleared")
		}
	}
}

// SetMode applies an operator override.
func (c *Controller) SetMode(m Mode) {
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()
	log.Printf("yield: mode set to %s", m)
}

// Yielding reports the effective state and a human reason.
func (c *Controller) Yielding() (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch c.mode {
	case ModeForceYield:
		return true, "manual"
	case ModeForceServe:
		return false, ""
	default:
		return c.autoContended, c.autoReason
	}
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
	yielding, reason := c.Yielding()
	c.mu.RLock()
	mode, autoReason := c.mode, c.autoReason
	c.mu.RUnlock()
	return State{Mode: mode.String(), Yielding: yielding, Reason: reason, AutoReason: autoReason}
}
