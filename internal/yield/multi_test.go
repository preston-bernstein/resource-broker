package yield

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestMultiInstanceUnloadReloadBothFire proves NewMulti/NewWithConfirmMulti
// actually drive every configured instance, not just unloaders[0]: on a
// yield-start transition both instances' Unload must be called, and on the
// following clear transition both instances' Reload must be called.
func TestMultiInstanceUnloadReloadBothFire(t *testing.T) {
	d := &fakeDet{}
	u1 := &fakeUnloader{}
	u2 := &fakeUnloader{}
	c := NewMulti(d, []Unloader{u1, u2}, nil, time.Hour)

	d.set("gaming-steam", true)
	c.refresh(context.Background())
	if !eventually(t, func() bool { return u1.calls() == 1 }) {
		t.Fatalf("instance 1 Unload called %d times, want 1", u1.calls())
	}
	if !eventually(t, func() bool { return u2.calls() == 1 }) {
		t.Fatalf("instance 2 Unload called %d times, want 1", u2.calls())
	}

	d.set("", false)
	c.refresh(context.Background())
	if !eventually(t, func() bool { return u1.reloadCalls() == 1 }) {
		t.Fatalf("instance 1 Reload called %d times, want 1", u1.reloadCalls())
	}
	if !eventually(t, func() bool { return u2.reloadCalls() == 1 }) {
		t.Fatalf("instance 2 Reload called %d times, want 1", u2.reloadCalls())
	}
}

// TestMultiInstanceFlapPreservesPerInstanceOrder is the multi-instance
// counterpart to TestActionsPreserveTransitionOrderAcrossFlap: it drives the
// exact same fast yield-then-clear-while-still-unloading race, but against
// two simultaneously configured instances, each with its own independent
// orderedUnloader (own release channel, own recorded order). It proves
// actionDone's per-index chaining (see yield.go) is truly independent per
// instance — a shared/global chain would still happen to pass a
// single-instance test, but would show up here as one instance's Reload
// leaking onto the wrong chain or waiting on the other instance's release.
func TestMultiInstanceFlapPreservesPerInstanceOrder(t *testing.T) {
	d := &fakeDet{}
	u1 := &orderedUnloader{release: make(chan struct{})}
	u2 := &orderedUnloader{release: make(chan struct{})}
	c := NewMulti(d, []Unloader{u1, u2}, nil, time.Hour)

	d.set("gaming-steam", true)
	c.refresh(context.Background())
	if !eventually(t, func() bool { return len(u1.snapshot()) == 1 }) {
		t.Fatal("instance 1: doUnload never called Unload")
	}
	if !eventually(t, func() bool { return len(u2.snapshot()) == 1 }) {
		t.Fatal("instance 2: doUnload never called Unload")
	}

	// Clear immediately, while both instances' Unload are still blocked on
	// release: this spawns doReload for both instances before either
	// doUnload has finished.
	d.set("", false)
	c.refresh(context.Background())

	// Give the scheduler every chance to run either doReload first if
	// nothing were ordering it against that same instance's still-pending
	// doUnload completion.
	time.Sleep(50 * time.Millisecond)
	if got := u1.snapshot(); len(got) != 1 || got[0] != "unload" {
		t.Fatalf("instance 1: Reload ran before Unload finished: order = %v, want [unload]", got)
	}
	if got := u2.snapshot(); len(got) != 1 || got[0] != "unload" {
		t.Fatalf("instance 2: Reload ran before Unload finished: order = %v, want [unload]", got)
	}

	close(u1.release)
	close(u2.release)

	if !eventually(t, func() bool {
		got := u1.snapshot()
		return len(got) == 2 && got[0] == "unload" && got[1] == "reload"
	}) {
		t.Fatalf("instance 1: final order = %v, want [unload reload]", u1.snapshot())
	}
	if !eventually(t, func() bool {
		got := u2.snapshot()
		return len(got) == 2 && got[0] == "unload" && got[1] == "reload"
	}) {
		t.Fatalf("instance 2: final order = %v, want [unload reload]", u2.snapshot())
	}
}

// slowUnloader's Unload records that it started, sleeps a short fixed delay,
// then records completion and returns nil — used as
// TestMultiInstanceOneInstanceErrorsDoesNotBlockOther's "instance B", so the
// test can distinguish "B's Unload eventually got called" (weak) from "B's
// Unload actually ran to completion promptly" (the real FR-13 isolation
// claim) instead of only observing a call count.
type slowUnloader struct {
	mu     sync.Mutex
	called int
	done   bool
	delay  time.Duration
}

func (s *slowUnloader) Unload(context.Context) error {
	s.mu.Lock()
	s.called++
	s.mu.Unlock()
	time.Sleep(s.delay)
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	return nil
}

func (s *slowUnloader) Reload(context.Context) error { return nil }

func (s *slowUnloader) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called
}

func (s *slowUnloader) isDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// TestMultiInstanceOneInstanceErrorsDoesNotBlockOther proves FR-13's
// independence guarantee: instance A permanently errors on Unload (returning
// immediately, simulating a jammed backend that will never succeed), while
// instance B's Unload takes a short but nonzero amount of time to complete.
// Because each configured instance gets its own actionDone chain and its own
// `go c.doUnload()` goroutine (see applyLocked/startAction in yield.go), A's
// error must never delay or block B: B's Unload must still be called and
// must still run to completion within its own delay, not stall waiting on A.
func TestMultiInstanceOneInstanceErrorsDoesNotBlockOther(t *testing.T) {
	d := &fakeDet{}
	a := &erroringUnloader{err: errors.New("instance A permanently jammed")}
	b := &slowUnloader{delay: 50 * time.Millisecond}
	c := NewMulti(d, []Unloader{a, b}, nil, time.Hour)

	start := time.Now()
	d.set("gaming-steam", true)
	c.refresh(context.Background())

	if !eventually(t, func() bool { return a.calls() > 0 }) {
		t.Fatal("instance A (jammed): Unload never called")
	}
	if !eventually(t, func() bool { return b.calls() > 0 }) {
		t.Fatal("instance B: Unload never called — A's error should not block B from even starting")
	}
	if !eventually(t, func() bool { return b.isDone() }) {
		t.Fatal("instance B: Unload never completed — appears blocked by A's error")
	}

	// B's Unload only sleeps 50ms; give a generous bound (well under
	// eventually's 1s poll ceiling) to prove it wasn't stalled waiting on A.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("instance B completed after %v, want ~%v (not blocked by instance A)", elapsed, b.delay)
	}
}
