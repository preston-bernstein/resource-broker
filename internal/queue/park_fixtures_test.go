package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/testhttp"
)

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

// startGate serves next behind s.Gate(cfg) for the length of the test.
func startGate(t *testing.T, s *Scheduler, cfg GateConfig, next http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.Gate(cfg, next))
	t.Cleanup(srv.Close)
	return srv
}

// gateResult is what a client saw from the Gate: the HTTP status and the
// X-Broker-Status header.
type gateResult struct {
	status int
	header string
}

// getResult sends a GET and reports what came back. A failed call is reported
// on t and returns the zero result, so it is safe to call from a goroutine.
func getResult(t *testing.T, url string) gateResult {
	reply, err := testhttp.GetReply(url)
	if err != nil {
		t.Errorf("get: %v", err)
		return gateResult{}
	}
	return gateResult{status: reply.StatusCode, header: reply.Header.Get("X-Broker-Status")}
}
