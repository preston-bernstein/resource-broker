package yield

import (
	"context"
	"log/slog"
	"time"
)

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
	// triggerDisplace is a displaced unit giving way to, or getting back,
	// the GPU from a displacing route (ADR-0020, displace.go).
	triggerDisplace unloadTrigger = "displace"
)

// action is one pending Unload or Reload of unloaders[i]. wait is that
// instance's previous action's done channel (nil if this is the first action
// ever for instance i); done is this action's own completion channel, which
// doUnload/doReload close when finished.
type action struct {
	i    int
	wait <-chan struct{}
	done chan struct{}
}

// startAction records a new pending unload/reload action for unloaders[i]
// and hands back what doUnload/doReload need. Must only be called
// while c.mu is held, from applyLocked, checkIdleLocked, or TrackActivity's
// CAS-won wake branch (which acquires c.mu only for this case), so the
// handoff chain it builds matches those callers' own call order exactly —
// see actionDone's doc comment for why this ordering matters.
func (c *Controller) startAction(i int) action {
	a := action{i: i, wait: c.actionDone[i], done: make(chan struct{})}
	c.actionDone[i] = a.done
	return a
}

// doUnload runs in its own goroutine (see applyLocked), so a panic here would
// otherwise crash the whole broker process — e.g. a typed-nil Unloader (an
// interface value that passes NewWithConfirmMulti's `u != nil` filter at
// construction but wraps a nil concrete pointer, see docs/openai-compatible-upstream-backend/plan.md's
// "Typed-nil safety" note) invoking Unload on a nil receiver. The recover
// here is cheap defense-in-depth against exactly that unrecovered-goroutine
// panic path.
//
// a.i identifies which configured instance this call belongs to; c.unloaders
// and c.labels are only ever read here (never mutated after construction),
// so indexing them without holding c.mu is safe. a.wait/a.done chain this
// action to its same-instance neighbors (see actionDone's doc comment and
// startAction): doUnload waits for instance i's previous action to finish
// before calling Unload, and always closes done on the way out (even on
// panic/recover) so instance i's next action isn't stuck waiting forever —
// this has no effect on any other instance's chain. trigger identifies what
// caused this call (yield-contention vs idle-timeout) and is only logged,
// never branched on. The call keeps ctx's values but not its cancellation,
// and is bounded by unloadReloadTimeout instead.
func (c *Controller) doUnload(ctx context.Context, a action, trigger unloadTrigger) {
	defer close(a.done)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in vram unload", "instance", c.labels[a.i], "recover", r)
		}
	}()
	if a.wait != nil {
		<-a.wait
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unloadReloadTimeout)
	defer cancel()
	if err := c.unloaders[a.i].Unload(actx); err != nil {
		slog.Warn("vram unload failed", "instance", c.labels[a.i], "err", err, "trigger", string(trigger))
	} else {
		slog.Info("vram unload requested", "instance", c.labels[a.i], "trigger", string(trigger))
	}
}

// doReload runs in its own goroutine (see applyLocked), so a panic here
// would otherwise crash the whole broker process — see doUnload's comment
// for the typed-nil Unloader hazard this guards against and for what ctx, a
// and trigger do.
func (c *Controller) doReload(ctx context.Context, a action, trigger unloadTrigger) {
	defer close(a.done)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in vram reload", "instance", c.labels[a.i], "recover", r)
		}
	}()
	if a.wait != nil {
		<-a.wait
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unloadReloadTimeout)
	defer cancel()
	if err := c.unloaders[a.i].Reload(actx); err != nil {
		slog.Warn("vram reload failed", "instance", c.labels[a.i], "err", err, "trigger", string(trigger))
	} else {
		slog.Info("vram reload requested", "instance", c.labels[a.i], "trigger", string(trigger))
	}
}

func (c *Controller) pauseGPUMgr(ctx context.Context, m GPUManager) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = m.PauseGPU(actx)
}

func (c *Controller) resumeGPUMgr(ctx context.Context, m GPUManager) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = m.ResumeGPU(actx)
}
