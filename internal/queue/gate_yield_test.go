package queue

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type alwaysServe struct{}

func (alwaysServe) Yielding() (bool, string)      { return false, "" }
func (alwaysServe) ServeContext() context.Context { return context.Background() }

type alwaysYield struct{}

func (alwaysYield) Yielding() (bool, string)      { return true, "gaming-steam" }
func (alwaysYield) ServeContext() context.Context { return context.Background() }

// TestGateRefusesWhenYielding never calls SetParkConfig, so it exercises
// the Batch-class NEVER-CONFIGURED fail-closed default (park.maxQueue's
// zero value means parking is off): a Batch request during yield takes the
// exact same immediate-deferRequest path Interactive always has. This is
// the fail-closed pin for FR-13 — reject immediately, no park — and for
// AC-4: it proves "parking being off doesn't break anything," not "parking
// works" (see TestGateParksDuringYield for that).
func TestGateRefusesWhenYielding(t *testing.T) {
	var hit bool
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		io.WriteString(w, "ok")
	})
	s := New()
	srv := httptest.NewServer(s.Gate(Batch, 2*time.Second, alwaysYield{}, nil, upstream))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if hit {
		t.Fatal("upstream was hit while yielding")
	}
	// Slot must not be left held.
	if st := s.Stats(); st.Busy {
		t.Fatalf("scheduler busy after refused request: %+v", st)
	}
}

// TestGateInteractiveNeverParksWhenConfigured proves FR-10 by the stronger
// case: even with parking explicitly configured (maxQueue > 0), an
// Interactive-class request during yield still returns immediately via
// deferRequest and never enters parkFor — the class != Batch gate itself,
// not merely "parking was never turned on here" (that's what
// TestGateRefusesWhenYielding pins instead).
func TestGateInteractiveNeverParksWhenConfigured(t *testing.T) {
	var hit bool
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		io.WriteString(w, "ok")
	})
	s := New()
	s.SetParkConfig(time.Second, 32, 8) // parking on; still must not affect Interactive
	srv := httptest.NewServer(s.Gate(Interactive, 2*time.Second, alwaysYield{}, nil, upstream))
	defer srv.Close()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("get (should return immediately, not park): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if hit {
		t.Fatal("upstream was hit while yielding")
	}
	if st := s.Stats(); st.Busy {
		t.Fatalf("scheduler busy after refused request: %+v", st)
	}
	if st := s.Stats(); st.Parked != 0 {
		t.Fatalf("interactive request parked: Stats().Parked = %d, want 0", st.Parked)
	}
}

// --- park test fixtures -----------------------------------------------

// yieldFlag is a thread-safe bool driving both an Admission.Yielding()
// probe and the plain yielding closure RunParkDrain expects — mirroring how
// gate.go and RunParkDrain each read yield.Controller independently in
// production (see the yieldingFn adapter in cmd/broker/main.go).
type yieldFlag struct {
	mu sync.Mutex
	v  bool
}

func newYieldFlag(v bool) *yieldFlag { return &yieldFlag{v: v} }

func (f *yieldFlag) set(v bool) {
	f.mu.Lock()
	f.v = v
	f.mu.Unlock()
}

func (f *yieldFlag) get() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

// flagAdm adapts a *yieldFlag to Admission for park tests. ServeContext
// never fires: park tests don't exercise in-flight preemption — that's
// TestGateCancelsInFlightOnYield's job in gate_cancel_test.go.
type flagAdm struct{ f *yieldFlag }

func (a flagAdm) Yielding() (bool, string) {
	if a.f.get() {
		return true, "test-yield"
	}
	return false, ""
}
func (a flagAdm) ServeContext() context.Context { return context.Background() }

// countRec is a minimal Recorder fake for park tests: tallies outcomes by
// name and captures each RecordPark wait, without pulling in
// internal/metrics.
type countRec struct {
	mu        sync.Mutex
	outcomes  map[string]int
	parkWaits []time.Duration
}

func newCountRec() *countRec { return &countRec{outcomes: map[string]int{}} }

func (r *countRec) Record(class, outcome string, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes[outcome]++
}

func (r *countRec) RecordPark(wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parkWaits = append(r.parkWaits, wait)
}

func (r *countRec) count(outcome string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcomes[outcome]
}

// TestGateParksDuringYield proves AC-1: a Batch request arriving while
// Yielding() is true, with parking configured, does not return until
// yielding clears, then is served normally — upstream is hit and the
// outcome is "served".
func TestGateParksDuringYield(t *testing.T) {
	var hit int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
		io.WriteString(w, "ok")
	})
	s := New()
	s.SetParkConfig(3*time.Second, 4, 4)
	rec := newCountRec()
	yf := newYieldFlag(true)
	srv := httptest.NewServer(s.Gate(Batch, 3*time.Second, flagAdm{yf}, rec, upstream))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunParkDrain(ctx, yf.get)

	type result struct {
		status int
		header string
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Errorf("get: %v", err)
			done <- result{}
			return
		}
		defer resp.Body.Close()
		done <- result{status: resp.StatusCode, header: resp.Header.Get("X-Broker-Status")}
	}()

	waitFor(t, func() bool { return s.Stats().Parked == 1 })
	yf.set(false) // yield ends; RunParkDrain releases on its next tick

	select {
	case r := <-done:
		if r.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.status)
		}
		if r.header != "served" {
			t.Fatalf("X-Broker-Status = %q, want %q", r.header, "served")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("request never returned after yield ended")
	}
	if atomic.LoadInt32(&hit) != 1 {
		t.Fatal("upstream not hit")
	}
	if got := rec.count("served"); got != 1 {
		t.Fatalf("served count = %d, want 1", got)
	}
	if st := s.Stats(); st.Parked != 0 {
		t.Fatalf("Parked = %d after release, want 0", st.Parked)
	}
}

// TestGateParkExpires proves AC-2: a request parked longer than
// BROKER_PARK_HOLD returns 503 with X-Broker-Status: deferred, upstream is
// never hit, and it is recorded with outcome=expired.
func TestGateParkExpires(t *testing.T) {
	var hit int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
	})
	s := New()
	s.SetParkConfig(120*time.Millisecond, 4, 4) // short hold, never drained
	rec := newCountRec()
	srv := httptest.NewServer(s.Gate(Batch, 2*time.Second, alwaysYield{}, rec, upstream))
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Broker-Status"); got != "deferred" {
		t.Fatalf("X-Broker-Status = %q, want %q", got, "deferred")
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
	if atomic.LoadInt32(&hit) != 0 {
		t.Fatal("upstream was hit for an expired park")
	}
	if elapsed < 80*time.Millisecond {
		t.Fatalf("returned in %v, before the hold bound had a chance to elapse", elapsed)
	}
	if got := rec.count("expired"); got != 1 {
		t.Fatalf("expired count = %d, want 1", got)
	}
	if st := s.Stats(); st.Parked != 0 {
		t.Fatalf("Parked = %d after expiry, want 0 (ghost entry)", st.Parked)
	}
}

// TestGateParkQueueCeiling proves AC-3: with the park queue at
// BROKER_PARK_MAX_QUEUE, the next arriving Batch request during yield is
// rejected immediately (503, outcome=park_rejected) without being parked
// and without blocking.
func TestGateParkQueueCeiling(t *testing.T) {
	const maxQueue = 3
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s := New()
	s.SetParkConfig(5*time.Second, maxQueue, maxQueue)
	rec := newCountRec()
	srv := httptest.NewServer(s.Gate(Batch, 5*time.Second, alwaysYield{}, rec, upstream))
	defer srv.Close()

	// Fill the ceiling with cancellable requests so they can be cleaned up
	// quickly at the end of the test rather than waiting out the hold.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < maxQueue; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	waitFor(t, func() bool { return s.Stats().Parked == maxQueue })

	start := time.Now()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ceiling rejection took %v, want near-immediate (no blocking)", elapsed)
	}
	if got := rec.count("park_rejected"); got != 1 {
		t.Fatalf("park_rejected count = %d, want 1", got)
	}
	if st := s.Stats(); st.Parked != maxQueue {
		t.Fatalf("Parked = %d, want unchanged at ceiling %d", st.Parked, maxQueue)
	}

	cancel() // release the maxQueue holders promptly (parkCanceled path)
	wg.Wait()
	waitFor(t, func() bool { return s.Stats().Parked == 0 })
}

// TestGateParkDrainBurst proves AC-5 at the Gate/HTTP layer: parked
// requests beyond BROKER_PARK_DRAIN_BURST are released in FIFO-ordered
// batches (oldest waiters first), never more than burst at a time.
func TestGateParkDrainBurst(t *testing.T) {
	const n = 4
	const burst = 2
	var mu sync.Mutex
	var arrival []int // order upstream was hit, by request id

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		mu.Lock()
		arrival = append(arrival, id)
		mu.Unlock()
	})
	s := New()
	s.SetParkConfig(5*time.Second, n, burst)
	yf := newYieldFlag(true)
	srv := httptest.NewServer(s.Gate(Batch, 5*time.Second, flagAdm{yf}, nil, upstream))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunParkDrain(ctx, yf.get)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		id := i
		go func() {
			defer wg.Done()
			resp, err := http.Get(fmt.Sprintf("%s?id=%d", srv.URL, id))
			if err == nil {
				resp.Body.Close()
			}
		}()
		// Sequence enqueue order deterministically: don't fire request i+1
		// until request i has actually parked.
		waitFor(t, func() bool { return s.Stats().Parked == id+1 })
	}

	yf.set(false) // yield ends; drain proceeds in bursts of `burst`

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("not all parked requests were released")
	}

	mu.Lock()
	got := append([]int(nil), arrival...)
	mu.Unlock()
	if len(got) != n {
		t.Fatalf("released count = %d, want %d", len(got), n)
	}

	// Membership per burst group is FIFO (oldest waiters released first).
	// Intra-group completion order is not asserted: maxInflight=1 serializes
	// released waiters through the scheduler's own FIFO Acquire queue, whose
	// exact interleaving among simultaneously-woken goroutines is not a
	// contract this test needs to pin.
	wantGroups := [][]int{{0, 1}, {2, 3}}
	idx := 0
	for _, want := range wantGroups {
		group := got[idx : idx+len(want)]
		idx += len(want)
		seen := map[int]bool{}
		for _, id := range group {
			seen[id] = true
		}
		for _, id := range want {
			if !seen[id] {
				t.Fatalf("burst group %v missing id %d (got %v); FIFO release order violated", want, id, group)
			}
		}
	}
}

// TestGateParkClientDisconnect proves AC-6: cancelling a parked request's
// context returns promptly, with outcome=canceled, no leaked goroutine, and
// no response written (client is gone).
func TestGateParkClientDisconnect(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit for a canceled parked request")
	})
	s := New()
	s.SetParkConfig(5*time.Second, 5, 5)
	rec := newCountRec()
	srv := httptest.NewServer(s.Gate(Batch, 5*time.Second, alwaysYield{}, rec, upstream))
	defer srv.Close()

	baseGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	done := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		close(done)
	}()

	waitFor(t, func() bool { return s.Stats().Parked == 1 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client call did not return after context cancel")
	}

	waitFor(t, func() bool { return s.Stats().Parked == 0 })
	// The client's Do() returns the instant its context cancels — the server
	// handler may still be mid-record. Wait for the outcome, don't assert it
	// immediately.
	waitFor(t, func() bool { return rec.count("canceled") == 1 })
	if got := rec.count("served"); got != 0 {
		t.Fatalf("served count = %d, want 0", got)
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= baseGoroutines+2 })
}

// TestGateParkShutdown proves AC-7: cancelling the shutdown context resolves
// every parked request immediately with 503 / X-Broker-Status: crash_failed
// / outcome=crash_failed, well within the real 10s shutCtx window.
func TestGateParkShutdown(t *testing.T) {
	const n = 3
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit for a shutdown-failed parked request")
	})
	s := New()
	s.SetParkConfig(10*time.Second, n, n)
	shutCtx, shutCancel := context.WithCancel(context.Background())
	s.SetShutdownContext(shutCtx)
	rec := newCountRec()
	srv := httptest.NewServer(s.Gate(Batch, 10*time.Second, alwaysYield{}, rec, upstream))
	defer srv.Close()

	type result struct {
		status int
		header string
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Errorf("get: %v", err)
				results <- result{}
				return
			}
			defer resp.Body.Close()
			results <- result{status: resp.StatusCode, header: resp.Header.Get("X-Broker-Status")}
		}()
	}
	waitFor(t, func() bool { return s.Stats().Parked == n })

	start := time.Now()
	shutCancel()

	for i := 0; i < n; i++ {
		select {
		case r := <-results:
			if r.status != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", r.status)
			}
			if r.header != "crash_failed" {
				t.Errorf("X-Broker-Status = %q, want %q", r.header, "crash_failed")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("parked request did not resolve after shutdown signal")
		}
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("shutdown unwind took %v, want well under the 10s shutCtx window", elapsed)
	}
	if got := rec.count("crash_failed"); got != n {
		t.Fatalf("crash_failed count = %d, want %d", got, n)
	}
	waitFor(t, func() bool { return s.Stats().Parked == 0 })
}
