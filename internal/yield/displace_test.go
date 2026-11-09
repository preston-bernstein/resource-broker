package yield

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// timeline records Unload/Reload starts and finishes across several
// instances, so a test can check the order between instances: the point of
// displacement is that vLLM and the GPU worker never hold VRAM at once.
type timeline struct {
	mu     sync.Mutex
	events []string
}

func (t *timeline) add(e string) {
	t.mu.Lock()
	t.events = append(t.events, e)
	t.mu.Unlock()
}

func (t *timeline) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.events...)
}

func (t *timeline) index(e string) int {
	for i, x := range t.snapshot() {
		if x == e {
			return i
		}
	}
	return -1
}

func (t *timeline) count(e string) int {
	n := 0
	for _, x := range t.snapshot() {
		if x == e {
			n++
		}
	}
	return n
}

// gatedUnloader writes "<name> stop"/"<name> stopped" and "<name> start" to
// a shared timeline. A non-nil gate holds each Unload until the test sends.
type gatedUnloader struct {
	name string
	tl   *timeline
	gate chan struct{}
}

func (g *gatedUnloader) Unload(context.Context) error {
	g.tl.add(g.name + " stop")
	if g.gate != nil {
		<-g.gate
	}
	g.tl.add(g.name + " stopped")
	return nil
}

func (g *gatedUnloader) Reload(context.Context) error {
	g.tl.add(g.name + " start")
	return nil
}

// newDisplaceRig builds default(nil) + one route "vllm" (idle timeout) that
// displaces one yield unit "gpu", the same orig layout buildBroker uses:
// 0 default, 1 route, 2 unit.
func newDisplaceRig(t *testing.T, confirmPolls int, vllmGate chan struct{}) (*Controller, *fakeDet, *timeline, http.Handler) {
	t.Helper()
	d := &fakeDet{}
	tl := &timeline{}
	vllm := &gatedUnloader{name: "vllm", tl: tl, gate: vllmGate}
	gpu := &gatedUnloader{name: "gpu", tl: tl}
	c := NewWithConfirmMulti(d, []Unloader{nil, vllm, gpu}, []string{"default", "route", "unit:gpu"}, time.Hour, confirmPolls)
	c.ConfigureIdle([]time.Duration{0, time.Hour, 0})
	c.ConfigureDisplace(map[int][]int{1: {2}})
	h := c.TrackActivity(1, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	return c, d, tl, h
}

func TestDisplaceStopsDisplacerAtStartThenStartsUnitAfterIt(t *testing.T) {
	gate := make(chan struct{})
	c, _, tl, _ := newDisplaceRig(t, 1, gate)
	if !eventually(t, func() bool { return tl.index("vllm stop") >= 0 }) {
		t.Fatal("ConfigureDisplace did not stop the displacer once")
	}
	c.refresh()
	time.Sleep(50 * time.Millisecond)
	if tl.index("gpu start") >= 0 {
		t.Fatalf("unit started while the displacer was still stopping: %v", tl.snapshot())
	}
	close(gate)
	if !eventually(t, func() bool { return tl.index("gpu start") >= 0 }) {
		t.Fatalf("unit never started: %v", tl.snapshot())
	}
	if tl.index("vllm stopped") > tl.index("gpu start") {
		t.Fatalf("unit started before the displacer finished stopping: %v", tl.snapshot())
	}
}

func TestDisplaceWaitsForConfirmPollsBeforeStartingUnit(t *testing.T) {
	c, _, tl, _ := newDisplaceRig(t, 2, nil)
	c.refresh()
	time.Sleep(50 * time.Millisecond)
	if tl.index("gpu start") >= 0 {
		t.Fatal("unit started before confirmPolls ticks; a broker started mid-game would start then stop it")
	}
	c.refresh()
	if !eventually(t, func() bool { return tl.index("gpu start") >= 0 }) {
		t.Fatal("unit never started after confirmPolls ticks")
	}
}

func TestDisplaceWakeStopsUnitBeforeDisplacerStarts(t *testing.T) {
	c, _, tl, h := newDisplaceRig(t, 1, nil)
	c.refresh()
	if !eventually(t, func() bool { return tl.index("gpu start") >= 0 }) {
		t.Fatal("unit never started")
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/chat", nil))
	if !eventually(t, func() bool { return tl.index("vllm start") >= 0 }) {
		t.Fatalf("displacer never woke: %v", tl.snapshot())
	}
	if s, v := tl.index("gpu stopped"), tl.index("vllm start"); s < 0 || s > v {
		t.Fatalf("displacer started before the unit finished stopping: %v", tl.snapshot())
	}
	// While the displacer is loaded, later ticks must not restart the unit.
	c.refresh()
	c.refresh()
	time.Sleep(50 * time.Millisecond)
	if n := tl.count("gpu start"); n != 1 {
		t.Fatalf("unit restarted while the displacer held the GPU (%d starts): %v", n, tl.snapshot())
	}
}

func TestDisplaceIdleUnloadHandsGPUBackToUnit(t *testing.T) {
	c, _, tl, h := newDisplaceRig(t, 1, nil)
	c.refresh()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/chat", nil))
	if !eventually(t, func() bool { return tl.index("vllm start") >= 0 }) {
		t.Fatal("displacer never woke")
	}
	c.mu.Lock()
	c.lastDispatch[c.origToFiltered[1]].Store(time.Now().Add(-2 * time.Hour).UnixNano())
	c.mu.Unlock()
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu start") == 2 }) {
		t.Fatalf("unit not restarted after the displacer idle-unloaded: %v", tl.snapshot())
	}
	ev := tl.snapshot()
	last := -1
	for i, e := range ev {
		if e == "vllm stopped" {
			last = i
		}
	}
	if last < 0 || last > len(ev)-1 || ev[len(ev)-1] != "gpu start" {
		t.Fatalf("unit restarted before the displacer finished its idle stop: %v", ev)
	}
}

func TestDisplaceYieldStopsUnitAndClearRestartsItOnlyIfDisplacerIdle(t *testing.T) {
	c, d, tl, h := newDisplaceRig(t, 1, nil)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu start") == 1 }) {
		t.Fatal("unit never started")
	}
	d.set("gaming-steam", true)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu stopped") == 1 }) {
		t.Fatalf("yield did not stop the unit: %v", tl.snapshot())
	}
	d.set("", false)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu start") == 2 }) {
		t.Fatalf("clear did not restart the unit while the displacer was idle: %v", tl.snapshot())
	}

	// Now with the displacer loaded: a yield and its clear must leave the unit off.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/chat", nil))
	if !eventually(t, func() bool { return tl.count("vllm start") == 1 }) {
		t.Fatal("displacer never woke")
	}
	d.set("plex-transcode", true)
	c.refresh()
	d.set("", false)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("vllm start") == 2 }) {
		t.Fatalf("clear did not reload the in-use displacer: %v", tl.snapshot())
	}
	time.Sleep(50 * time.Millisecond)
	if n := tl.count("gpu start"); n != 2 {
		t.Fatalf("unit started beside a loaded displacer (%d starts): %v", n, tl.snapshot())
	}
}

func TestConfigureDisplacePanicsWithoutIdleTimeout(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic for a displacer with no idle timeout")
		}
	}()
	tl := &timeline{}
	c := NewWithConfirmMulti(&fakeDet{}, []Unloader{nil, &gatedUnloader{name: "v", tl: tl}, &gatedUnloader{name: "g", tl: tl}}, nil, time.Hour, 1)
	c.ConfigureIdle([]time.Duration{0, 0, 0})
	c.ConfigureDisplace(map[int][]int{1: {2}})
}

func TestNoDisplaceKeepsOldBehaviour(t *testing.T) {
	d := &fakeDet{}
	tl := &timeline{}
	gpu := &gatedUnloader{name: "gpu", tl: tl}
	c := NewWithConfirmMulti(d, []Unloader{nil, gpu}, nil, time.Hour, 1)
	c.ConfigureIdle([]time.Duration{0, 0})
	c.ConfigureDisplace(nil)
	c.refresh()
	time.Sleep(50 * time.Millisecond)
	if len(tl.snapshot()) != 0 {
		t.Fatalf("a plain yield unit was touched outside a yield: %v", tl.snapshot())
	}
	d.set("gaming-steam", true)
	c.refresh()
	d.set("", false)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu start") == 1 && tl.count("gpu stopped") == 1 }) {
		t.Fatalf("yield/clear did not stop and start the unit: %v", tl.snapshot())
	}
}

// backdateFree pretends the unit's GPU became free d ago.
func backdateFree(c *Controller, d time.Duration) {
	c.mu.Lock()
	j := c.origToFiltered[2]
	if !c.unitFreeSince[j].IsZero() {
		c.unitFreeSince[j] = c.unitFreeSince[j].Add(-d)
	}
	c.mu.Unlock()
}

func TestDisplaceHoldoffDelaysRestartAfterClear(t *testing.T) {
	c, d, tl, _ := newDisplaceRig(t, 1, nil)
	c.SetUnitResumeHoldoff(5 * time.Minute)
	c.refresh()
	backdateFree(c, 5*time.Minute)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu start") == 1 }) {
		t.Fatalf("unit never started once its GPU had been free for the hold-off: %v", tl.snapshot())
	}
	d.set("plex", true)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu stopped") == 1 }) {
		t.Fatalf("hold-off delayed the yield's stop: %v", tl.snapshot())
	}
	d.set("", false)
	c.refresh()
	c.refresh()
	time.Sleep(50 * time.Millisecond)
	if n := tl.count("gpu start"); n != 1 {
		t.Fatalf("unit restarted inside the hold-off (%d starts): %v", n, tl.snapshot())
	}
	backdateFree(c, 5*time.Minute)
	c.refresh()
	if !eventually(t, func() bool { return tl.count("gpu start") == 2 }) {
		t.Fatalf("unit not restarted after the hold-off: %v", tl.snapshot())
	}
}

func TestDisplaceHoldoffRestartsTheClockOnAFlap(t *testing.T) {
	c, d, tl, _ := newDisplaceRig(t, 1, nil)
	c.SetUnitResumeHoldoff(5 * time.Minute)
	c.refresh()
	backdateFree(c, 4*time.Minute) // 4 of 5 minutes free
	d.set("plex", true)
	c.refresh() // a yield resets the clock
	d.set("", false)
	c.refresh()
	backdateFree(c, 4*time.Minute) // 4 minutes since the clear, 8 in total
	c.refresh()
	time.Sleep(50 * time.Millisecond)
	if n := tl.count("gpu start"); n != 0 {
		t.Fatalf("free time before the yield counted toward the hold-off (%d starts): %v", n, tl.snapshot())
	}
}
