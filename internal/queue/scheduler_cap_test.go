package queue

import (
	"context"
	"errors"
	"testing"
)

// TestQueueFull: once a class queue is at MaxWaitersPerClass, further Acquire
// calls fail fast with ErrQueueFull instead of enqueuing unbounded waiters.
func TestQueueFull(t *testing.T) {
	s := New()
	if err := s.Acquire(context.Background(), Batch); err != nil { // occupy the slot
		t.Fatalf("acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // releases all parked waiters at test end

	for i := 0; i < MaxWaitersPerClass; i++ {
		go s.Acquire(ctx, Batch)
	}
	waitFor(t, func() bool { return s.Stats().Batch == MaxWaitersPerClass })

	// The queue is now full; an immediate Acquire must be rejected.
	if err := s.Acquire(context.Background(), Batch); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}

	// A different class is unaffected.
	if s.Stats().Interactive != 0 {
		t.Fatalf("interactive queue should be empty")
	}
}
