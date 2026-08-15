package job

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/backend"
	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/queue"
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

// openaiBlockGen is the openai-compat analogue of blockGen: it wires a real
// *backend.Backend* (the "openai" backend, from internal/backend, backed by
// internal/openaicompat.Client) to a mock OpenAI-compatible SSE server that
// streams one chunk then blocks until the caller's context is cancelled.
// Used so the preemption tests (AC-7, AC-8) prove identical outcomes across
// backends using the real openai backend/client wiring rather than a
// shape-alike fake.
func openaiBlockGen(t *testing.T) Generator {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush support", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		flusher.Flush()
		<-r.Context().Done() // hold the stream open until preemption cancels it
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse mock openai url: %v", err)
	}
	be, err := backend.New(&config.Config{UpstreamBackend: "openai", UpstreamURL: u})
	if err != nil {
		t.Fatalf("backend.New(openai): %v", err)
	}
	return be
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

// backendGens is the shared (ollama, openai) parameterization used by the
// preemption tests below: blockGen is the existing fakeGen-style, Ollama-
// shaped generator; openaiBlockGen wires the real openai backend/client to a
// mock OpenAI-compatible server. Both block until their context is cancelled,
// so both exercise the exact same worker preemption codepath
// (runJob/monitor/shouldPreempt) — proving the outcome is backend-agnostic
// (AC-7, AC-8), not something either fake merely simulates.
func backendGens() []struct {
	name string
	gen  func(t *testing.T) Generator
} {
	return []struct {
		name string
		gen  func(t *testing.T) Generator
	}{
		{"ollama", func(t *testing.T) Generator { return blockGen() }},
		{"openai", openaiBlockGen},
	}
}

// TestWorkerGamingPreempt pins AC-7: a Job in flight when Yield begins (mock
// gaming/Plex contention) is preempted and requeues to the front of the
// Queue — asserted as Position == 1 in State QUEUED — identically for both
// backends.
func TestWorkerGamingPreempt(t *testing.T) {
	for _, tc := range backendGens() {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, yld, w := newRig(t, tc.gen(t), 3, 10*time.Millisecond)
			runWorker(t, w)

			id := submitJob(t, svc, "game-"+tc.name)
			waitState(t, svc, id, StateRunning)

			yld.startYield() // gaming takes the GPU
			waitState(t, svc, id, StateQueued)

			// Requeued to the front of the Queue (position 1), not failed, and no
			// attempt burned by a clean preempt (AC-7) — identical for both
			// backends.
			st, _ := svc.Get(context.Background(), id)
			if st.Position != 1 {
				t.Fatalf("position after gaming preempt = %d, want 1 (front of queue)", st.Position)
			}
			if st.State != StateQueued {
				t.Fatalf("state after gaming preempt = %s, want QUEUED", st.State)
			}
			if st.Attempts != 0 {
				t.Fatalf("gaming preempt burned an attempt: %d", st.Attempts)
			}
		})
	}
}

// TestWorkerInteractivePreemptPastQuantum pins AC-8: an interactive request
// preempts a running batch Job once BROKER_BATCH_QUANTUM has elapsed, and not
// before — with the same requeue/no-burned-attempt outcome as the gaming
// preempt path — identically for both backends.
func TestWorkerInteractivePreemptPastQuantum(t *testing.T) {
	for _, tc := range backendGens() {
		t.Run(tc.name, func(t *testing.T) {
			quantum := 400 * time.Millisecond
			svc, sched, _, w := newRig(t, tc.gen(t), 3, quantum)
			runWorker(t, w)

			id := submitJob(t, svc, "interactive-"+tc.name)
			waitState(t, svc, id, StateRunning)
			start := time.Now()

			// An interactive request parks on the busy gate; the holder keeps the
			// slot until we have asserted, so the worker cannot re-claim the
			// requeued Job and race the check.
			acquired := make(chan struct{})
			release := make(chan struct{})
			go func() {
				_ = sched.Acquire(context.Background(), queue.Interactive)
				close(acquired)
				<-release
				sched.Release()
			}()

			// Must NOT preempt before the quantum elapses (AC-8's "not before").
			// Checked at a third of the quantum, well short of both the quantum
			// itself and the monitor's 200ms re-check ticker, so ticker/store
			// jitter can't produce a false positive here.
			select {
			case <-acquired:
				t.Fatalf("interactive request preempted the batch Job after only %v, before quantum %v elapsed", time.Since(start), quantum)
			case <-time.After(quantum / 3):
			}

			// Past the quantum the worker must preempt so the interactive caller
			// proceeds.
			select {
			case <-acquired: // implies the worker preempted and released the batch slot
			case <-time.After(3 * time.Second):
				t.Fatal("interactive request never acquired the slot")
			}
			// tolerance absorbs the small, expected skew between this
			// externally-observed start (captured after waitState's polling
			// detects RUNNING) and the worker's internal start (captured before
			// Generate begins, inside runJob) — not a loosening of AC-8's "not
			// before" guarantee, which the earlier select already pins.
			const tolerance = 60 * time.Millisecond
			if elapsed := time.Since(start); elapsed < quantum-tolerance {
				t.Fatalf("interactive preempted after %v, want >= ~quantum %v (preempted before its trigger time)", elapsed, quantum)
			}

			// The Job was requeued (resume-first), not failed, with no attempt
			// burned (AC-8) — identical for both backends.
			st, _ := svc.Get(context.Background(), id)
			if st.State != StateQueued {
				t.Fatalf("state after interactive preempt = %s, want QUEUED", st.State)
			}
			if st.Attempts != 0 {
				t.Fatalf("interactive preempt burned an attempt: %d", st.Attempts)
			}
			close(release)
		})
	}
}
