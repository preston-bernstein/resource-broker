package yield

import (
	"context"
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
	c := New(d, time.Hour)

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
	c := New(d, 5*time.Millisecond)
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
