package queue

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestSingleConcurrency(t *testing.T) {
	s := New()
	if err := s.Acquire(context.Background(), Batch); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	granted := make(chan struct{})
	go func() {
		_ = s.Acquire(context.Background(), Batch)
		close(granted)
	}()

	// Second acquire must not be granted while the first holds the slot.
	select {
	case <-granted:
		t.Fatal("second acquire granted while slot held")
	case <-time.After(100 * time.Millisecond):
	}

	s.Release()
	select {
	case <-granted:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire not granted after release")
	}
	s.Release()
	if st := s.Stats(); st.Busy {
		t.Fatalf("expected idle, got %+v", st)
	}
}

func TestInteractivePriority(t *testing.T) {
	s := New()
	if err := s.Acquire(context.Background(), Batch); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	order := make(chan string, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	// Enqueue a batch waiter first...
	go func() {
		defer wg.Done()
		_ = s.Acquire(context.Background(), Batch)
		order <- "batch"
		s.Release()
	}()
	waitFor(t, func() bool { return s.Stats().Batch == 1 })

	// ...then an interactive waiter. Despite arriving later, it must win.
	go func() {
		defer wg.Done()
		_ = s.Acquire(context.Background(), Interactive)
		order <- "interactive"
		s.Release()
	}()
	waitFor(t, func() bool { return s.Stats().Interactive == 1 })

	s.Release() // free the original slot; scheduler picks next

	first := <-order
	second := <-order
	if first != "interactive" || second != "batch" {
		t.Fatalf("order = %q,%q; want interactive,batch", first, second)
	}
	wg.Wait()
}

func TestContextCancelRemovesWaiter(t *testing.T) {
	s := New()
	if err := s.Acquire(context.Background(), Batch); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Acquire(ctx, Batch) }()
	waitFor(t, func() bool { return s.Stats().Batch == 1 })

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not return after cancel")
	}
	waitFor(t, func() bool { return s.Stats().Batch == 0 })

	// The cancelled waiter must not have stolen or leaked the slot: releasing
	// the original returns the scheduler to idle.
	s.Release()
	if st := s.Stats(); st.Busy {
		t.Fatalf("expected idle after release, got %+v", st)
	}
}

func TestStatsImmediate(t *testing.T) {
	s := New()
	if st := s.Stats(); st.Busy || st.Batch != 0 || st.Interactive != 0 {
		t.Fatalf("fresh scheduler not idle: %+v", st)
	}
	_ = s.Acquire(context.Background(), Interactive)
	if st := s.Stats(); !st.Busy {
		t.Fatal("expected busy after acquire")
	}
	s.Release()
}
