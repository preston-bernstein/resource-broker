package job

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
)

// --- fakes ---

type fakeYield struct {
	mu       sync.Mutex
	yielding bool
	ctx      context.Context
	cancel   context.CancelFunc
}

func newFakeYield() *fakeYield {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeYield{ctx: ctx, cancel: cancel}
}
func (f *fakeYield) Yielding() (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.yielding, ""
}
func (f *fakeYield) ServeContext() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctx
}
func (f *fakeYield) startYield() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.yielding = true
	f.cancel()
}

type fakeGen struct {
	fn func(ctx context.Context, model, prompt string, opts map[string]any, onTokens func(int)) (string, error)
}

func (g fakeGen) Generate(ctx context.Context, model, prompt string, opts map[string]any, onTokens func(int)) (string, error) {
	return g.fn(ctx, model, prompt, opts, onTokens)
}

// blockGen blocks until its context is cancelled, then reports ctx.Err().
func blockGen() fakeGen {
	return fakeGen{fn: func(ctx context.Context, _, _ string, _ map[string]any, onTokens func(int)) (string, error) {
		if onTokens != nil {
			onTokens(1)
		}
		<-ctx.Done()
		return "", ctx.Err()
	}}
}

func waitState(t *testing.T, svc *Service, id string, want State) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := svc.Get(context.Background(), id); err == nil && st.State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	st, _ := svc.Get(context.Background(), id)
	t.Fatalf("job %s did not reach %s (last=%v)", id, want, st)
}

func newRig(t *testing.T, gen Generator, maxAttempts int, quantum time.Duration) (*Service, *queue.Scheduler, *fakeYield, *Worker) {
	t.Helper()
	store := newStore(t)
	svc := NewService(store, maxAttempts)
	sched := queue.New()
	yld := newFakeYield()
	w := NewWorker(svc, sched, yld, gen, quantum, 10*time.Millisecond)
	return svc, sched, yld, w
}

func runWorker(t *testing.T, w *Worker) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return cancel
}

func submitJob(t *testing.T, svc *Service, key string) string {
	t.Helper()
	j, _, err := svc.Submit(context.Background(), SubmitRequest{IdempotencyKey: key, Model: "m", Prompt: "p"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return j.ID
}

// --- tests ---

func TestWorkerSuccess(t *testing.T) {
	gen := fakeGen{fn: func(_ context.Context, _, _ string, _ map[string]any, onTokens func(int)) (string, error) {
		onTokens(5)
		return "the answer", nil
	}}
	svc, _, _, w := newRig(t, gen, 3, 10*time.Millisecond)
	runWorker(t, w)

	id := submitJob(t, svc, "ok")
	waitState(t, svc, id, StateSucceeded)

	res, err := svc.Result(context.Background(), id)
	if err != nil || res != "the answer" {
		t.Fatalf("result = %q err=%v", res, err)
	}
	// First fetch stamps fetched_at.
	st, _ := svc.Get(context.Background(), id)
	if st.FetchedAt == nil {
		t.Fatal("fetched_at not stamped after Result")
	}
}

func TestWorkerRetryThenFail(t *testing.T) {
	var calls int
	var mu sync.Mutex
	gen := fakeGen{fn: func(_ context.Context, _, _ string, _ map[string]any, _ func(int)) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "", errors.New("upstream boom")
	}}
	svc, _, _, w := newRig(t, gen, 2, 10*time.Millisecond) // cap 2 -> fails on 2nd
	runWorker(t, w)

	id := submitJob(t, svc, "bad")
	waitState(t, svc, id, StateFailed)

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("generator called %d times, want 2 (cap)", calls)
	}
}

func TestWorkerCancel(t *testing.T) {
	svc, _, _, w := newRig(t, blockGen(), 3, 10*time.Millisecond)
	runWorker(t, w)

	id := submitJob(t, svc, "cancel")
	waitState(t, svc, id, StateRunning)

	if _, err := svc.Cancel(context.Background(), id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitState(t, svc, id, StateCanceled)

	// Must stay canceled (not requeued by the worker).
	time.Sleep(50 * time.Millisecond)
	st, _ := svc.Get(context.Background(), id)
	if st.State != StateCanceled {
		t.Fatalf("state after cancel = %s", st.State)
	}
}

func TestWorkerGamingPreempt(t *testing.T) {
	svc, _, yld, w := newRig(t, blockGen(), 3, 10*time.Millisecond)
	runWorker(t, w)

	id := submitJob(t, svc, "game")
	waitState(t, svc, id, StateRunning)

	yld.startYield() // gaming takes the GPU
	waitState(t, svc, id, StateQueued)

	// Requeued, not failed, and no attempt burned by a clean preempt.
	st, _ := svc.Get(context.Background(), id)
	if st.Attempts != 0 {
		t.Fatalf("gaming preempt burned an attempt: %d", st.Attempts)
	}
}

func TestWorkerInteractivePreemptPastQuantum(t *testing.T) {
	quantum := 30 * time.Millisecond
	svc, sched, _, w := newRig(t, blockGen(), 3, quantum)
	runWorker(t, w)

	id := submitJob(t, svc, "interactive")
	waitState(t, svc, id, StateRunning)

	// An interactive request parks on the busy gate; past the quantum the worker
	// must preempt the running Job so the interactive caller proceeds. The
	// holder keeps the slot until we have asserted, so the worker cannot
	// re-claim the requeued Job and race the check.
	acquired := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = sched.Acquire(context.Background(), queue.Interactive)
		close(acquired)
		<-release
		sched.Release()
	}()

	select {
	case <-acquired: // implies the worker preempted and released the batch slot
	case <-time.After(2 * time.Second):
		t.Fatal("interactive request never acquired the slot")
	}

	// The Job was requeued (resume-first), not failed, with no attempt burned.
	st, _ := svc.Get(context.Background(), id)
	if st.State != StateQueued {
		t.Fatalf("state after interactive preempt = %s, want QUEUED", st.State)
	}
	if st.Attempts != 0 {
		t.Fatalf("interactive preempt burned an attempt: %d", st.Attempts)
	}
	close(release)
}
