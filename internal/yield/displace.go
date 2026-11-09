package yield

import (
	"fmt"
	"log/slog"
	"time"
)

// Displacement (ADR-0020): a route's backend may displace yield units. A
// displaced unit (a BROKER_YIELD_UNITS entry, e.g. AudioMuse-AI's GPU worker)
// runs only while the Broker is not yielding AND every route that displaces it
// is idle-unloaded. So the GPU worker has the card whenever vLLM is unloaded,
// and vLLM gets the VRAM back on the first request.
//
// Ordering is what makes this safe on one card: a displacer's reload waits for
// its displaced units' stops to finish, and a displaced unit's start waits for
// its displacers' stops to finish, so the two never hold VRAM at once.
//
// State (all guarded by c.mu):
//   - displacers[j]: post-filter indexes of the instances that displace j.
//   - displaces[i]:  post-filter indexes of the units instance i displaces.
//   - unitWant[j]:   whether j was last commanded to run (true) or stop.
//   - pollCount:     refresh ticks so far; reconcile waits for confirmPolls of
//     them, so a broker started mid-game never starts a unit just to stop it.
//   - unitFreeSince[j]: when j's GPU last became free (zero while it is not).
//     With a hold-off set, j starts only once that is unitHoldoff old.

// ConfigureDisplace records which instances displace which, keyed by ORIG
// index (the space ConfigureIdle takes): displace[i] lists the orig indexes
// instance i displaces. Every displacer must already have an idle timeout,
// or it would hold the units off forever. Call it after ConfigureIdle and
// before Run.
//
// The Broker does not know at start whether a displacer's backend is already
// running, so each displacer starts idle-unloaded and is stopped once now. Its
// next request wakes it through TrackActivity as usual.
func (c *Controller) ConfigureDisplace(displace map[int][]int) {
	if len(displace) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.unloaders)
	c.displacers = make([][]int, n)
	c.displaces = make([][]int, n)
	c.unitWant = make([]bool, n)
	c.unitFreeSince = make([]time.Time, n)
	for origI, units := range displace {
		i := c.postIndex(origI, "displacer")
		if c.idleTimeouts[i] == 0 {
			panic(fmt.Sprintf("yield: ConfigureDisplace: %s displaces units but has no idle timeout — this should have been caught at config.Load()", c.labels[i]))
		}
		for _, origJ := range units {
			j := c.postIndex(origJ, "displaced unit")
			c.displaces[i] = append(c.displaces[i], j)
			c.displacers[j] = append(c.displacers[j], i)
		}
	}
	for i := range c.displaces {
		if len(c.displaces[i]) == 0 {
			continue
		}
		c.idleUnloaded[i].Store(true)
		wait, done := c.startAction(i)
		go c.doUnload(i, wait, done, triggerIdle)
	}
}

// SetUnitResumeHoldoff makes a displaced unit wait until its GPU has been
// free for d before it starts again (ADR-0021). A Plex session can yield and
// clear every minute, and a unit stopped seconds after it starts is stopped
// mid-setup, which once wedged the GPU. Stops are never held off. Call it
// before Run; d <= 0 turns the hold-off off.
func (c *Controller) SetUnitResumeHoldoff(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unitHoldoff = d
}

func (c *Controller) postIndex(orig int, what string) int {
	if orig < 0 || orig >= len(c.origToFiltered) || c.origToFiltered[orig] == -1 {
		panic(fmt.Sprintf("yield: ConfigureDisplace: %s orig index %d has no Unloader", what, orig))
	}
	return c.origToFiltered[orig]
}

// isDisplaced reports whether instance j is a displaced unit, whose start and
// stop reconcileDisplacedLocked owns outside a whole-Broker yield. Caller holds c.mu.
func (c *Controller) isDisplaced(j int) bool {
	return c.displacers != nil && len(c.displacers[j]) > 0
}

// reconcileDisplacedLocked starts or stops each displaced unit to match the
// rule above. Called from refresh, after applyLocked and checkIdleLocked, so
// it sees this tick's yield and idle-unload decisions. Caller holds c.mu.
func (c *Controller) reconcileDisplacedLocked() {
	if c.displacers == nil || c.pollCount < c.confirmPolls {
		return
	}
	now := time.Now()
	for j, ds := range c.displacers {
		if len(ds) == 0 {
			continue
		}
		want := !c.effective
		for _, i := range ds {
			if !c.idleUnloaded[i].Load() {
				want = false
			}
		}
		if !want {
			c.unitFreeSince[j] = time.Time{}
		} else if c.unitFreeSince[j].IsZero() {
			c.unitFreeSince[j] = now
		}
		if want == c.unitWant[j] {
			continue
		}
		if want && now.Sub(c.unitFreeSince[j]) < c.unitHoldoff {
			continue
		}
		c.unitWant[j] = want
		if !want {
			wait, done := c.startAction(j)
			go c.doUnload(j, wait, done, triggerDisplace)
			continue
		}
		// Start only after every displacer's pending stop has finished.
		waits := []<-chan struct{}{c.actionDone[j]}
		for _, i := range ds {
			waits = append(waits, c.actionDone[i])
		}
		_, done := c.startAction(j)
		go c.doReload(j, afterAll(waits...), done, triggerDisplace)
		slog.Info("displaced unit resumes", "unit", c.labels[j])
	}
}

// stopDisplacedLocked stops instance i's displaced units ahead of its wake and
// returns the channels its reload must wait for. Caller holds c.mu.
func (c *Controller) stopDisplacedLocked(i int) []<-chan struct{} {
	if c.displaces == nil {
		return nil
	}
	var waits []<-chan struct{}
	for _, j := range c.displaces[i] {
		if c.unitWant[j] {
			c.unitWant[j] = false
			wait, done := c.startAction(j)
			go c.doUnload(j, wait, done, triggerDisplace)
			slog.Info("displaced unit stops for a wake", "unit", c.labels[j], "by", c.labels[i])
		}
		// Even a unit already commanded to stop may still be stopping.
		waits = append(waits, c.actionDone[j])
	}
	return waits
}

// afterAll returns a channel closed once every non-nil input is closed.
func afterAll(chans ...<-chan struct{}) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		for _, ch := range chans {
			if ch != nil {
				<-ch
			}
		}
		close(out)
	}()
	return out
}
