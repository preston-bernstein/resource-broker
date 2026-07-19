package queue

import (
	"context"
	"testing"
	"time"
)

// TestMaxInflightConcurrency: with the cap raised, that many Acquires succeed
// immediately and the next one blocks until a Release frees a slot (ADR-0004).
func TestMaxInflightConcurrency(t *testing.T) {
	s := New()
	s.SetMaxInflight(2)

	if err := s.Acquire(context.Background(), Batch); err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if err := s.Acquire(context.Background(), Batch); err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if st := s.Stats(); st.Inflight != 2 || st.MaxInflight != 2 {
		t.Fatalf("stats = %+v, want inflight 2 / max 2", st)
	}

	granted := make(chan struct{})
	go func() {
		_ = s.Acquire(context.Background(), Batch)
		close(granted)
	}()
	select {
	case <-granted:
		t.Fatal("third acquire granted while at capacity")
	case <-time.After(100 * time.Millisecond):
	}

	s.Release()
	select {
	case <-granted:
	case <-time.After(2 * time.Second):
		t.Fatal("third acquire not granted after release")
	}
	s.Release()
	s.Release()
	if st := s.Stats(); st.Inflight != 0 {
		t.Fatalf("expected idle, got inflight %d", st.Inflight)
	}
}

// TestInteractiveWaitingSignal: parking an interactive request pings the
// channel the Job worker uses to decide preemption.
func TestInteractiveWaitingSignal(t *testing.T) {
	s := New()
	if err := s.Acquire(context.Background(), Batch); err != nil { // occupy the only slot
		t.Fatalf("acquire: %v", err)
	}

	go s.Acquire(context.Background(), Interactive) // parks (no free slot)

	select {
	case <-s.InteractiveWaiting():
	case <-time.After(2 * time.Second):
		t.Fatal("no interactive-waiting signal")
	}
	waitFor(t, func() bool { return s.Stats().Interactive == 1 })
}
