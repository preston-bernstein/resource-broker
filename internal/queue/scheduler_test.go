package queue

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
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
	// A fresh Scheduler (SetParkConfig never called) must report Parked == 0
	// — the zero-value park queue, not an uninitialized/garbage depth.
	if st := s.Stats(); st.Busy || st.Batch != 0 || st.Interactive != 0 || st.Parked != 0 {
		t.Fatalf("fresh scheduler not idle: %+v", st)
	}
	_ = s.Acquire(context.Background(), Interactive)
	if st := s.Stats(); !st.Busy {
		t.Fatal("expected busy after acquire")
	}
	s.Release()
}

// TestParkGhostCleanup is the FR-15 regression guard: parked requests that
// exit via expiry, cancellation, or shutdown (every path OTHER than
// release) must fully splice themselves out of the park queue — not just
// return the right result to their own caller. Stats().Parked (and the
// underlying park.q slice) must return to the true live count, and new
// parks must be able to succeed afterward rather than being wrongly
// park_rejected against a depth inflated by ghost entries.
func TestParkGhostCleanup(t *testing.T) {
	s := New()
	s.SetParkConfig(300*time.Millisecond, 3, 8)

	// Exit path 1: hold-bound expiry. Park one entry, let it age out, and
	// require the depth to return to zero (not just the right result code).
	r1 := parkAsync(context.Background(), t, s)
	requireParkResult(t, s, r1, parkExpired)

	// Exit path 2: caller-context cancellation.
	ctx2, cancel2 := context.WithCancel(context.Background())
	r2 := parkAsync(ctx2, t, s)
	cancel2()
	requireParkResult(t, s, r2, parkCanceled)

	// Exit path 3: shutdown signal.
	shutCtx, shutCancel := context.WithCancel(context.Background())
	s.SetShutdownContext(shutCtx)
	r3 := parkAsync(context.Background(), t, s)
	shutCancel()
	requireParkResult(t, s, r3, parkShutdown)

	s.park.mu.Lock()
	depth := len(s.park.q)
	s.park.mu.Unlock()
	if depth != 0 {
		t.Fatalf("park.q depth = %d, want 0 (ghost entries left behind)", depth)
	}

	// Reset the shutdown signal to a live (non-fired) context so the refill
	// parks below actually block instead of resolving instantly.
	s.SetShutdownContext(context.Background())

	// After all three non-release exits, the queue must accept a full refill
	// up to the ceiling — proving no ghost entry is eating capacity.
	const refill = 3
	rns := make([]<-chan parkResult, refill)
	for i := range rns {
		rns[i] = parkAsync(context.Background(), t, s)
		waitFor(t, func() bool { return s.Stats().Parked == i+1 })
	}
	if st := s.Stats(); st.Parked != refill {
		t.Fatalf("re-park after cleanup: Parked = %d, want %d (ghost depth drift)", st.Parked, refill)
	}

	s.park.drainOneBurst(refill)
	for _, r := range rns {
		requireParkResult(t, s, r, parkReleased)
	}
}

// parkAsync parks one request on s and waits until it is queued. The result
// arrives on the returned channel once the park ends.
func parkAsync(ctx context.Context, t *testing.T, s *Scheduler) <-chan parkResult {
	t.Helper()
	before := s.Stats().Parked
	ch := make(chan parkResult, 1)
	go func() { ch <- s.parkFor(ctx) }()
	waitFor(t, func() bool { return s.Stats().Parked == before+1 })
	return ch
}

// requireParkResult checks the park ended with want, then that the queue is
// empty again: every exit path must splice its entry out.
func requireParkResult(t *testing.T, s *Scheduler, ch <-chan parkResult, want parkResult) {
	t.Helper()
	if got := <-ch; got != want {
		t.Fatalf("park result = %v, want %v", got, want)
	}
	waitFor(t, func() bool { return s.Stats().Parked == 0 })
}

// TestParkConcurrentCeilingRace proves FR-14's atomicity fix under real
// contention: many goroutines call parkFor simultaneously against a small
// maxQueue; the queue must never exceed maxQueue and exactly maxQueue must
// be accepted, with the remainder rejected — no overshoot from a
// check-then-append race. Run with -race.
func TestParkConcurrentCeilingRace(t *testing.T) {
	const maxQueue = 5
	const extra = 7
	const total = maxQueue + extra

	s := New()
	// Hold is far longer than the test window so expiry never races the
	// release accounting below.
	s.SetParkConfig(30*time.Second, maxQueue, maxQueue)

	results, wg, startRace := launchParkers(s, total)
	stop := make(chan struct{})
	overshoot := watchParkDepth(s, maxQueue, stop)
	startRace()

	// Wait for ALL racers before draining (maxQueue parked, `extra` rejected):
	// draining earlier lets a late goroutine park after the drain and sit
	// until hold expiry — the flake this guard exists to prevent.
	waitFor(t, func() bool {
		return s.Stats().Parked == maxQueue && len(results) == extra
	})
	s.park.drainOneBurst(maxQueue)
	wg.Wait()
	close(stop)
	close(results)

	if got := overshoot.Load(); got != 0 {
		t.Fatalf("park depth overshot the ceiling: observed %d, maxQueue %d", got, maxQueue)
	}

	var released, rejected int
	for r := range results {
		switch r {
		case parkReleased:
			released++
		case parkRejected:
			rejected++
		default:
			t.Fatalf("unexpected parkFor result: %v", r)
		}
	}
	if released != maxQueue {
		t.Fatalf("released = %d, want %d", released, maxQueue)
	}
	if rejected != extra {
		t.Fatalf("rejected = %d, want %d", rejected, extra)
	}
	waitFor(t, func() bool { return s.Stats().Parked == 0 })
}

// launchParkers starts n goroutines that each call parkFor. They all wait at a
// gate until startRace is called, so they are released at once to maximize
// contention.
func launchParkers(s *Scheduler, n int) (results chan parkResult, wg *sync.WaitGroup, startRace func()) {
	results = make(chan parkResult, n)
	var ready, gate sync.WaitGroup
	ready.Add(n)
	gate.Add(1)
	wg = &sync.WaitGroup{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			gate.Wait()
			results <- s.parkFor(context.Background())
		}()
	}
	ready.Wait() // every goroutine has started and is waiting at the gate
	return results, wg, gate.Done
}

// watchParkDepth samples the park depth until stop closes and records the
// highest value seen above ceiling (0 when it never went over).
func watchParkDepth(s *Scheduler, ceiling int, stop <-chan struct{}) *atomic.Int64 {
	var overshoot atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if st := s.Stats(); st.Parked > ceiling {
				overshoot.Store(int64(st.Parked))
			}
			// Back off between samples: a hot Stats() spin contends the same
			// mutexes parkFor needs and can starve the parkers under -race
			// scheduling (observed as a waitFor deadline flake).
			time.Sleep(100 * time.Microsecond)
		}
	}()
	return &overshoot
}

// TestRunParkDrainPacing proves the drain loop releases in bursts paced by
// parkDrainInterval, not all at once: with parked count > drainBurst,
// successive release ticks land roughly parkDrainInterval apart. Generous
// tolerance to avoid flake, per plan.
func TestRunParkDrainPacing(t *testing.T) {
	const maxQueue = 6
	const burst = 2
	s := New()
	s.SetParkConfig(5*time.Second, maxQueue, burst)

	var mu sync.Mutex
	var releaseTimes []time.Time
	var wg sync.WaitGroup
	for i := 0; i < maxQueue; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := s.parkFor(context.Background()); res == parkReleased {
				mu.Lock()
				releaseTimes = append(releaseTimes, time.Now())
				mu.Unlock()
			}
		}()
		waitFor(t, func() bool { return s.Stats().Parked == i+1 })
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	yielding := func() bool { return false } // yield already ended; drain freely
	start := time.Now()
	go s.RunParkDrain(ctx, yielding)
	waitOrFail(t, &wg, 6*time.Second, "not all parked requests were released")
	cancel()

	mu.Lock()
	times := append([]time.Time(nil), releaseTimes...)
	mu.Unlock()
	if len(times) != maxQueue {
		t.Fatalf("released count = %d, want %d", len(times), maxQueue)
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })

	// 6 parked at burst 2 releases across 3 ticks: [0,1] then [2,3] then
	// [4,5]. The gap between the first release and the last-tick's first
	// release should span roughly two drain intervals — proves pacing, not
	// an eventual-consistency check alone.
	if gap := times[4].Sub(times[0]); gap < 1500*time.Millisecond {
		t.Fatalf("releases too close together: burst1->burst3 gap = %v, want >= ~2 drain intervals", gap)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("drain took too long: %v", elapsed)
	}
}

// waitOrFail waits for wg, failing with msg if it takes longer than d.
func waitOrFail(t *testing.T, wg *sync.WaitGroup, d time.Duration, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

// TestRunParkDrainBoundedUnderFlap is the busy-loop regression guard: the
// yielding func() bool closure flips rapidly (~every 10ms) over a fixed
// wall-clock window, and the number of times RunParkDrain actually calls it
// must stay bounded near window/parkDrainInterval — proportional to elapsed
// ticks, not to the number of flaps. This is the design's structural
// defense (a plain time.Ticker poll) against the busy-loop failure mode the
// rejected event-callback design would have had.
func TestRunParkDrainBoundedUnderFlap(t *testing.T) {
	s := New()
	s.SetParkConfig(5*time.Second, 8, 8)

	var flipping int32
	stopFlap := make(chan struct{})
	var flapWG sync.WaitGroup
	flapWG.Add(1)
	go func() {
		defer flapWG.Done()
		for {
			select {
			case <-stopFlap:
				return
			default:
			}
			old := atomic.LoadInt32(&flipping)
			atomic.StoreInt32(&flipping, 1-old)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var calls int32
	yielding := func() bool {
		atomic.AddInt32(&calls, 1)
		return atomic.LoadInt32(&flipping) == 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	const window = 3 * time.Second
	go s.RunParkDrain(ctx, yielding)

	time.Sleep(window)
	cancel()
	close(stopFlap)
	flapWG.Wait()

	got := atomic.LoadInt32(&calls)
	// Generous tolerance: expect ~window/parkDrainInterval calls, allow up
	// to 2x plus slack for scheduling jitter — nowhere near the hundreds a
	// busy loop driven by the 10ms flap would produce.
	wantMax := int32(window/parkDrainInterval)*2 + 3
	if got > wantMax {
		t.Fatalf("yielding() called %d times over %v; want <= %d (bounded near window/%v) — RunParkDrain may be busy-looping", got, window, wantMax, parkDrainInterval)
	}
	if got < 1 {
		t.Fatal("yielding() was never called")
	}
}
