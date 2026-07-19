package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func submit(t *testing.T, s Store, key string) *Job {
	t.Helper()
	j := &Job{ID: "id-" + key, IdempotencyKey: key, Source: "test", Model: "m", Prompt: "p"}
	stored, created, err := s.Submit(context.Background(), j)
	if err != nil {
		t.Fatalf("submit %s: %v", key, err)
	}
	if !created {
		t.Fatalf("submit %s: expected created", key)
	}
	return stored
}

func TestSubmitIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	j1 := submit(t, s, "k1")
	if j1.State != StateQueued || j1.Attempts != 0 {
		t.Fatalf("new job state=%s attempts=%d", j1.State, j1.Attempts)
	}

	// Same key, different id -> returns the original, not a duplicate.
	dup := &Job{ID: "other", IdempotencyKey: "k1", Model: "m"}
	stored, created, err := s.Submit(ctx, dup)
	if err != nil {
		t.Fatalf("dup submit: %v", err)
	}
	if created {
		t.Fatal("expected created=false on repeated key")
	}
	if stored.ID != j1.ID {
		t.Fatalf("dup id = %s, want %s", stored.ID, j1.ID)
	}
}

func TestClaimOrderAndSucceed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := submit(t, s, "a")
	b := submit(t, s, "b")

	// FIFO by position_hint: a before b.
	got, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.ID != a.ID || got.State != StateRunning {
		t.Fatalf("claim1 = %s/%s, want %s/RUNNING", got.ID, got.State, a.ID)
	}

	if err := s.Succeed(ctx, a.ID, "answer"); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	done, _ := s.Get(ctx, a.ID)
	if done.State != StateSucceeded || done.Result != "answer" || done.FinishedAt == nil {
		t.Fatalf("succeeded job = %+v", done)
	}

	got2, _ := s.ClaimNext(ctx)
	if got2.ID != b.ID {
		t.Fatalf("claim2 = %s, want %s", got2.ID, b.ID)
	}
	if _, err := s.ClaimNext(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty claim err = %v, want ErrNotFound", err)
	}
}

func TestPreemptGoesToFront(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := submit(t, s, "a")
	_ = submit(t, s, "b")

	claimed, _ := s.ClaimNext(ctx) // a -> RUNNING
	if claimed.ID != a.ID {
		t.Fatalf("claimed %s", claimed.ID)
	}
	if err := s.Preempt(ctx, a.ID); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	// a must be QUEUED again and ahead of b.
	pos, _ := s.Position(ctx, a.ID)
	if pos != 1 {
		t.Fatalf("preempted position = %d, want 1", pos)
	}
	again, _ := s.ClaimNext(ctx)
	if again.ID != a.ID {
		t.Fatalf("re-claim = %s, want %s (resume-first)", again.ID, a.ID)
	}
	if again.Attempts != 0 {
		t.Fatalf("preempt must not burn an attempt, got %d", again.Attempts)
	}
}

func TestFailOrRetryCap(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := submit(t, s, "x")
	const max = 3

	for i := 1; i <= max; i++ {
		if _, err := s.ClaimNext(ctx); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		failed, err := s.FailOrRetry(ctx, j.ID, "boom", max)
		if err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		cur, _ := s.Get(ctx, j.ID)
		if cur.Attempts != i {
			t.Fatalf("after fail %d attempts=%d", i, cur.Attempts)
		}
		if i < max {
			if failed || cur.State != StateQueued {
				t.Fatalf("fail %d: failed=%v state=%s, want retry", i, failed, cur.State)
			}
		} else {
			if !failed || cur.State != StateFailed {
				t.Fatalf("fail %d: failed=%v state=%s, want FAILED", i, failed, cur.State)
			}
		}
	}
}

func TestRecoverRunning(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := submit(t, s, "r")
	if _, err := s.ClaimNext(ctx); err != nil { // -> RUNNING (simulates pre-crash)
		t.Fatalf("claim: %v", err)
	}
	n, err := s.RecoverRunning(ctx, 3)
	if err != nil || n != 1 {
		t.Fatalf("recover n=%d err=%v", n, err)
	}
	cur, _ := s.Get(ctx, j.ID)
	if cur.State != StateQueued || cur.Attempts != 1 || cur.StartedAt != nil {
		t.Fatalf("recovered = %+v", cur)
	}
}

func TestRecoverRunningCap(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := submit(t, s, "loop") // sole job, so claim always picks it

	// Each crash-recovery cycle costs one attempt; at the cap it FAILs.
	for i := 1; i <= 3; i++ {
		if _, err := s.ClaimNext(ctx); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if _, err := s.RecoverRunning(ctx, 3); err != nil {
			t.Fatalf("recover %d: %v", i, err)
		}
		cur, _ := s.Get(ctx, j.ID)
		if cur.Attempts != i {
			t.Fatalf("cycle %d attempts=%d", i, cur.Attempts)
		}
		if i < 3 && cur.State != StateQueued {
			t.Fatalf("cycle %d state=%s, want QUEUED", i, cur.State)
		}
		if i == 3 && cur.State != StateFailed {
			t.Fatalf("cycle %d state=%s, want FAILED", i, cur.State)
		}
	}
}

func TestCancel(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := submit(t, s, "c")
	got, err := s.Cancel(ctx, j.ID)
	if err != nil || got.State != StateCanceled {
		t.Fatalf("cancel = %+v err=%v", got, err)
	}
	// Cancelling a terminal job is a no-op.
	again, _ := s.Cancel(ctx, j.ID)
	if again.State != StateCanceled {
		t.Fatalf("re-cancel state=%s", again.State)
	}
}

func TestStampFetchedAndPrune(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := submit(t, s, "f")
	s.ClaimNext(ctx)
	s.Succeed(ctx, j.ID, "out")

	if err := s.StampFetched(ctx, j.ID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	first, _ := s.Get(ctx, j.ID)
	if first.FetchedAt == nil {
		t.Fatal("fetched_at not stamped")
	}
	// Second stamp must not move the timestamp.
	time.Sleep(2 * time.Millisecond)
	s.StampFetched(ctx, j.ID)
	second, _ := s.Get(ctx, j.ID)
	if !second.FetchedAt.Equal(*first.FetchedAt) {
		t.Fatal("fetched_at changed on re-stamp")
	}

	// Prune: fetched + grace elapsed (simulate via a far-future now).
	future := time.Now().Add(2 * time.Hour)
	n, err := s.Prune(ctx, time.Hour, 7*24*time.Hour, future)
	if err != nil || n != 1 {
		t.Fatalf("prune n=%d err=%v", n, err)
	}
	if _, err := s.Get(ctx, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job not pruned: %v", err)
	}
}

func TestCounts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	submit(t, s, "q1")
	submit(t, s, "q2")
	j := submit(t, s, "run")
	s.ClaimNext(ctx) // q1 -> RUNNING (lowest position)

	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c.Queued != 2 || c.Running != 1 {
		t.Fatalf("counts = %+v, want queued=2 running=1", c)
	}
	_ = j
}
