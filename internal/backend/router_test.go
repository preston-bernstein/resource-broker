package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/httpx"
	"github.com/preston-bernstein/resource-broker/internal/inference"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// fakeBackend is a minimal Backend used only by these tests: Proxy()
// forwards to an in-memory http.Handler that records what it received;
// Generate/Reachable/Unloader are stubs Router doesn't exercise via
// ProxyForLane/Proxy.
type fakeBackend struct {
	name    string
	handler http.HandlerFunc
}

func (f *fakeBackend) Proxy() http.Handler { return f.handler }
func (f *fakeBackend) Generate(ctx context.Context, req inference.Request) (string, error) {
	return "", nil
}
func (f *fakeBackend) Reachable(ctx context.Context) error { return nil }
func (f *fakeBackend) Unloader() yield.Unloader            { return nil }

// recordingBackend builds a fakeBackend that records every request body it
// receives (fully, for assertion purposes only — this is test-side
// buffering, not Router's) and reports which backend handled it via hitCh.
func recordingBackend(name string, hit chan<- string, bodies chan<- []byte) *fakeBackend {
	return &fakeBackend{
		name: name,
		handler: func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bodies <- b
			hit <- name
			w.WriteHeader(http.StatusOK)
		},
	}
}

func newTestRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/generate", strings.NewReader(body))
	return req
}

// TestRouterHappyPathDispatch covers FR-style happy-path dispatch: a request
// for a routed model ("qwen") reaches its configured backend, and a request
// for an unrouted model ("bert") falls back to the default backend.
func TestRouterHappyPathDispatch(t *testing.T) {
	hit := make(chan string, 2)
	bodies := make(chan []byte, 2)

	def := recordingBackend("default", hit, bodies)
	b1 := recordingBackend("b1", hit, bodies)

	r := NewRouter(def)
	r.AddRoute("qwen", "", b1)

	handler := r.ProxyForLane("interactive")

	// Request for the routed model reaches b1.
	req1 := newTestRequest(t, `{"model":"qwen","prompt":"hi"}`)
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if got := <-hit; got != "b1" {
		t.Fatalf("routed model %q reached backend %q, want b1", "qwen", got)
	}

	// Request for an unrouted model falls back to default.
	req2 := newTestRequest(t, `{"model":"bert","prompt":"hi"}`)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if got := <-hit; got != "default" {
		t.Fatalf("unrouted model %q reached backend %q, want default", "bert", got)
	}
}

// TestRouterLargeBodyModelPeekNotFullyBuffered covers FR-6: a multi-MB
// vision-style payload with "model" as the first JSON field must be
// dispatched correctly, with the forwarded body byte-identical to what was
// sent, AND the peek step itself must not have buffered the large payload —
// verified directly against httpx.PeekModel, which is the only place Router reads
// from the body before forwarding.
func TestRouterLargeBodyModelPeekNotFullyBuffered(t *testing.T) {
	// Build an ~8MB body: {"model":"vision-model","images":["<8MB of base64-ish filler>"]}
	filler := strings.Repeat("A", 8*1024*1024)
	body := `{"model":"vision-model","images":["` + filler + `"]}`

	// 1. Prove httpx.PeekModel itself doesn't buffer the large payload: consumed
	// bytes should be a tiny fraction of the 8MB body, since "model" is the
	// first field and httpx.PeekModel stops right after decoding it.
	model, consumed := httpx.PeekModel(strings.NewReader(body))
	if model != "vision-model" {
		t.Fatalf("httpx.PeekModel() model = %q, want %q", model, "vision-model")
	}
	if len(consumed) > 4096 {
		t.Fatalf("httpx.PeekModel() consumed %d bytes finding an early model field, want a small peek (not the full %d-byte body)", len(consumed), len(body))
	}

	// 2. Prove the full dispatch path still forwards a byte-identical body
	// to the resolved backend.
	hit := make(chan string, 1)
	bodies := make(chan []byte, 1)
	r := NewRouter(recordingBackend("default", hit, bodies))
	r.AddRoute("vision-model", "", recordingBackend("vision", hit, bodies))

	r.ProxyForLane("interactive").ServeHTTP(httptest.NewRecorder(), newTestRequest(t, body))

	if got := <-hit; got != "vision" {
		t.Fatalf("large vision request reached backend %q, want vision", got)
	}
	got := <-bodies
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("forwarded body not byte-identical: got %d bytes, want %d bytes", len(got), len(body))
	}
	requireForwardedModel(t, got, "vision-model")
}

// requireForwardedModel checks the decoded model field round-trips
// byte-for-byte through the forwarded body (FR-6 requirement stated
// explicitly).
func requireForwardedModel(t *testing.T, forwarded []byte, want string) {
	t.Helper()
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(forwarded, &decoded); err != nil {
		t.Fatalf("forwarded body is not valid JSON: %v", err)
	}
	if decoded.Model != want {
		t.Fatalf("forwarded body model = %q, want %q", decoded.Model, want)
	}
}

// TestRouterProxyFallsThroughToDefault covers Proxy() explicitly: with no
// lane context, a request for a model routed to a lane-scoped backend must
// still fall through to the default backend, since Proxy() (unlike
// ProxyForLane("interactive")/ProxyForLane("batch")) has no lane to match
// against.
func TestRouterProxyFallsThroughToDefault(t *testing.T) {
	hit := make(chan string, 1)
	bodies := make(chan []byte, 1)
	def := recordingBackend("default", hit, bodies)
	b1 := recordingBackend("b1", hit, bodies)

	r := NewRouter(def)
	r.AddRoute("qwen", "interactive", b1) // lane-scoped route

	// Proxy() has no lane context, so even a request for "qwen" (routed,
	// but only for the "interactive" lane) must fall through to default.
	req := newTestRequest(t, `{"model":"qwen","prompt":"hi"}`)
	w := httptest.NewRecorder()
	r.Proxy().ServeHTTP(w, req)

	if got := <-hit; got != "default" {
		t.Fatalf("Proxy() for lane-scoped route reached backend %q, want default", got)
	}
}

// TestRouterProxyIsAliasForProxyForLaneEmpty verifies Proxy() really is
// exactly ProxyForLane(""): a universally-scoped route (lane "") still
// matches under Proxy(), same as it would under any ProxyForLane call.
func TestRouterProxyIsAliasForProxyForLaneEmpty(t *testing.T) {
	hit := make(chan string, 1)
	bodies := make(chan []byte, 1)
	def := recordingBackend("default", hit, bodies)
	b1 := recordingBackend("b1", hit, bodies)

	r := NewRouter(def)
	r.AddRoute("qwen", "", b1) // unscoped route — applies to both lanes

	req := newTestRequest(t, `{"model":"qwen","prompt":"hi"}`)
	w := httptest.NewRecorder()
	r.Proxy().ServeHTTP(w, req)

	if got := <-hit; got != "b1" {
		t.Fatalf("Proxy() for unscoped route reached backend %q, want b1", got)
	}
}

// TestRouterMissingModelFallsBackToDefault covers the "model" field not
// found within the peek cap: the request must still forward correctly
// (byte-identical) to the default backend.
func TestRouterMissingModelFallsBackToDefault(t *testing.T) {
	hit := make(chan string, 1)
	bodies := make(chan []byte, 1)
	def := recordingBackend("default", hit, bodies)
	b1 := recordingBackend("b1", hit, bodies)

	r := NewRouter(def)
	r.AddRoute("qwen", "", b1)

	body := `{"prompt":"no model field here"}`
	req := newTestRequest(t, body)
	w := httptest.NewRecorder()
	r.ProxyForLane("interactive").ServeHTTP(w, req)

	if got := <-hit; got != "default" {
		t.Fatalf("request with no model field reached backend %q, want default", got)
	}
	got := <-bodies
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("forwarded body not byte-identical: got %q, want %q", got, body)
	}
}

// TestRouterLaneScopedRouteOnlyMatchesItsLane covers lane-scoping directly
// through ProxyForLane with both lanes exercised against the same
// lane-scoped route (TestRouterProxyFallsThroughToDefault above only covers
// Proxy()'s no-lane-context case). A route scoped to "interactive" must be
// reached by an interactive-lane request but must NOT be reached by a
// batch-lane request for the same model — that request instead falls
// through to the default backend.
func TestRouterLaneScopedRouteOnlyMatchesItsLane(t *testing.T) {
	hit := make(chan string, 2)
	bodies := make(chan []byte, 2)
	def := recordingBackend("default", hit, bodies)
	interactiveOnly := recordingBackend("interactive-only", hit, bodies)

	r := NewRouter(def)
	r.AddRoute("qwen", "interactive", interactiveOnly)

	// A batch request for the interactive-only-routed model must fall
	// through to default.
	reqBatch := newTestRequest(t, `{"model":"qwen","prompt":"hi"}`)
	wBatch := httptest.NewRecorder()
	r.ProxyForLane("batch").ServeHTTP(wBatch, reqBatch)
	if got := <-hit; got != "default" {
		t.Fatalf("batch request for interactive-only route reached backend %q, want default", got)
	}
	<-bodies

	// An interactive request for the same model must reach the routed
	// backend.
	reqInteractive := newTestRequest(t, `{"model":"qwen","prompt":"hi"}`)
	wInteractive := httptest.NewRecorder()
	r.ProxyForLane("interactive").ServeHTTP(wInteractive, reqInteractive)
	if got := <-hit; got != "interactive-only" {
		t.Fatalf("interactive request for interactive-only route reached backend %q, want interactive-only", got)
	}
	<-bodies
}

// TestRouterErrorPassthroughUnmodified covers error passthrough: when a
// routed backend's Proxy() handler writes an error response (status code,
// headers, and body), Router.ProxyForLane's dispatch must deliver it to the
// Consumer completely unmodified — Router never intercepts, rewrites, or
// swallows a routed backend's response, success or error.
func TestRouterErrorPassthroughUnmodified(t *testing.T) {
	def := &fakeBackend{
		name: "default",
		handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}
	const errBody = `{"error":"model not loaded","code":"MODEL_UNAVAILABLE"}`
	rejecting := &fakeBackend{
		name: "rejecting",
		handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Backend-Detail", "rejected-by-fake-backend")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(errBody))
		},
	}

	r := NewRouter(def)
	r.AddRoute("broken-model", "", rejecting)

	req := newTestRequest(t, `{"model":"broken-model","prompt":"hi"}`)
	w := httptest.NewRecorder()
	r.ProxyForLane("interactive").ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := w.Body.String(); got != errBody {
		t.Fatalf("body = %q, want %q (error response must pass through unmodified)", got, errBody)
	}
	if got := w.Header().Get("X-Backend-Detail"); got != "rejected-by-fake-backend" {
		t.Fatalf("header X-Backend-Detail = %q, want unmodified passthrough of the backend's own header", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want %q (unmodified passthrough)", got, "application/json")
	}
}

// TestRouterRoutingSummary covers RoutingSummary(): a Router with several
// configured routes — including two model names that share an identical
// backend+lane — must group the shared pair into one RouteSummary entry
// (not one entry per model) while keeping the differently-scoped route
// separate.
func TestRouterRoutingSummary(t *testing.T) {
	def := &genBackend{name: "default"}
	shared := &genBackend{name: "shared"}
	solo := &genBackend{name: "solo"}

	r := NewRouter(def)
	r.AddRoute("model-a", "interactive", shared)
	r.AddRoute("model-b", "interactive", shared) // same backend+lane as model-a
	r.AddRoute("model-c", "batch", solo)

	got := r.RoutingSummary()
	summaries, ok := got.([]RouteSummary)
	if !ok {
		t.Fatalf("RoutingSummary() returned %T, want []RouteSummary", got)
	}
	if len(summaries) != 2 {
		t.Fatalf("RoutingSummary() returned %d entries, want 2 (model-a/model-b grouped, model-c separate); got %+v", len(summaries), summaries)
	}
	// The grouped entry first, the solitary one second.
	sort.Slice(summaries, func(i, j int) bool { return len(summaries[i].Models) > len(summaries[j].Models) })
	requireSummary(t, summaries[0], "interactive", []string{"model-a", "model-b"})
	requireSummary(t, summaries[1], "batch", []string{"model-c"})
}

// requireSummary checks one RouteSummary entry's lane and (sorted) models.
func requireSummary(t *testing.T, got RouteSummary, lane string, models []string) {
	t.Helper()
	if got.Lane != lane {
		t.Fatalf("entry %v lane = %q, want %q", got.Models, got.Lane, lane)
	}
	sort.Strings(got.Models)
	if !reflect.DeepEqual(got.Models, models) {
		t.Fatalf("entry models = %v, want %v", got.Models, models)
	}
}

// genBackend is a minimal Backend used only by the Generate/Reachable/
// Unloader tests below: unlike fakeBackend (whose Generate/Reachable/
// Unloader are fixed stubs), genBackend's are configurable per test so we
// can tell which backend a call actually reached and assert on
// Reachable-error behavior.
type genBackend struct {
	name         string
	generateErr  error
	reachableErr error
}

func (g *genBackend) Proxy() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}
func (g *genBackend) Generate(ctx context.Context, req inference.Request) (string, error) {
	if g.generateErr != nil {
		return "", g.generateErr
	}
	return g.name + ":" + req.Model, nil
}
func (g *genBackend) Reachable(ctx context.Context) error { return g.reachableErr }
func (g *genBackend) Unloader() yield.Unloader            { return nil }

// TestRouterGenerateDispatchesByModel covers Generate: a routed model
// dispatches to its configured backend's own Generate (not the default's),
// and an unrouted model falls back to the default backend's Generate. This
// mirrors ProxyForLane's dispatch behavior but on the direct-argument Job
// path rather than by peeking a request body, and — per Generate's doc
// comment — ignores lane entirely (Jobs have no lane).
func TestRouterGenerateDispatchesByModel(t *testing.T) {
	def := &genBackend{name: "default"}
	alt := &genBackend{name: "alt"}

	r := NewRouter(def)
	r.AddRoute("qwen", "interactive", alt) // lane-scoped route: Generate must ignore the lane scoping

	got, err := r.Generate(context.Background(), inference.Request{Model: "qwen", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Generate(routed model) error = %v", err)
	}
	if want := "alt:qwen"; got != want {
		t.Fatalf("Generate(routed model) = %q, want %q", got, want)
	}

	got, err = r.Generate(context.Background(), inference.Request{Model: "bert", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Generate(unrouted model) error = %v", err)
	}
	if want := "default:bert"; got != want {
		t.Fatalf("Generate(unrouted model) = %q, want %q", got, want)
	}
}

// TestRouterReachableChecksOnlyDefault covers Reachable: even with a
// configured route whose backend would report itself unreachable, Router's
// Reachable() must reflect only the default backend's health — per-route
// liveness is deliberately out of scope here (RoutingSummary, task 3c).
func TestRouterReachableChecksOnlyDefault(t *testing.T) {
	def := &genBackend{name: "default"}
	alt := &genBackend{name: "alt", reachableErr: errors.New("alt backend is down")}

	r := NewRouter(def)
	r.AddRoute("qwen", "", alt)

	if err := r.Reachable(context.Background()); err != nil {
		t.Fatalf("Reachable() = %v, want nil (default backend is healthy; routed backend's error must be ignored)", err)
	}

	// Now make the default unreachable too, to prove Reachable really is
	// wired to def and not some hardcoded nil.
	def.reachableErr = errors.New("default backend is down")
	if err := r.Reachable(context.Background()); err == nil {
		t.Fatal("Reachable() = nil, want an error once the default backend is unreachable")
	}
}

// TestRouterUnloaderReturnsTrueNil covers the typed-nil safety requirement
// from backend.go's Backend interface doc: Unloader() must return a direct,
// literal nil of the yield.Unloader interface type. A typed-nil concrete
// pointer boxed into the interface would make `router.Unloader() != nil`
// true even though the underlying pointer is nil — this test guards against
// exactly that class of bug.
func TestRouterUnloaderReturnsTrueNil(t *testing.T) {
	r := NewRouter(&genBackend{name: "default"})
	if r.Unloader() != nil {
		t.Fatal("Router.Unloader() != nil, want a true nil (typed-nil safety violation)")
	}
}

// --- AC7: idle-Yield race, exercised through the REAL Router + Controller +
// activityBackend chain (docs/vllm-idle-unload/requirements.md AC7/FR11/FR12) ---

// raceTestDetector is a yield.Detector whose contention flag the test flips
// at runtime, driving ctrl.Run's real poll loop through a genuine
// Contention-start / Contention-clear transition — the second event source
// AC7 requires alongside Idle's own poll-driven fire, so the sequencing is
// produced by the real machinery (refresh -> applyLocked -> checkIdleLocked)
// rather than asserted about in isolation.
type raceTestDetector struct {
	mu        sync.Mutex
	contended bool
}

func (d *raceTestDetector) setContended(v bool) {
	d.mu.Lock()
	d.contended = v
	d.mu.Unlock()
}

func (d *raceTestDetector) Detect() (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.contended {
		return "gaming-steam", true
	}
	return "", false
}

// orderedIdleUnloader is this package's counterpart to
// internal/yield/yield_test.go's orderedUnloader: it records each
// Unload/Reload call, in the order actually invoked, and lets EVERY Unload
// call block on release until the test closes it — reproducing the exact
// "idle-triggered Unload still in flight when Contention begins" race AC7
// describes. Once release is closed, every subsequent receive on it returns
// immediately (closed-channel semantics), so a later, chained Unload call
// (Contention's own idempotent second call) proceeds without blocking again.
type orderedIdleUnloader struct {
	mu      sync.Mutex
	order   []string
	release chan struct{}
}

func (o *orderedIdleUnloader) Unload(context.Context) error {
	o.mu.Lock()
	o.order = append(o.order, "unload")
	o.mu.Unlock()
	<-o.release
	return nil
}

func (o *orderedIdleUnloader) Reload(context.Context) error {
	o.mu.Lock()
	o.order = append(o.order, "reload")
	o.mu.Unlock()
	return nil
}

func (o *orderedIdleUnloader) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.order...)
}

func (o *orderedIdleUnloader) orderLen() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.order)
}

// waitUntil polls cond, matching internal/yield/yield_test.go's eventually
// helper (unavailable here — different package), used by the AC7 test below
// so it doesn't have to guess a fixed sleep for the real poll loop to act.
func waitUntil(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestRouterIdleThenContentionUnloadOrderingIsNeverConflicting is the AC7
// integration test: it drives a request through the REAL Router ->
// *activityBackend (backend.WithActivityTracking) -> *yield.Controller
// chain — the same composition cmd/broker/main.go's buildBroker wires up
// (see cmd/broker/main_test.go's TestBuildBrokerWiresActivityTrackingIntoRouter
// for the construction-order proof this test builds on) — lets the
// instance's short idle duration elapse so checkIdleLocked fires a real
// idle-triggered Unload via the controller's own poll loop (never a direct
// unit-test call into unexported yield internals, which this package cannot
// reach anyway), then triggers a genuine gaming/Plex Contention transition
// on that SAME instance while the idle-triggered Unload is still blocked in
// flight.
//
// This reproduces plan.md's Architecture "case 2" exactly: Idle already
// fired in an earlier tick; Contention begins later and its own
// unconditional per-instance unload loop (applyLocked) fires Unload again,
// idempotently, on the already-idle-unloaded instance. FR11/AC7 explicitly
// carve this out as an ACCEPTABLE, non-conflicting outcome — this test
// asserts exactly that shape is what happens (order == [unload, unload,
// reload]) and treats it as a PASS, while still failing hard on the one
// thing AC7 actually forbids: a Reload racing ahead of, or interleaved with,
// either Unload (a genuinely conflicting/out-of-order pair).
func TestRouterIdleThenContentionUnloadOrderingIsNeverConflicting(t *testing.T) {
	rig := newIdleRouterRig(t)
	// Dispatch BEFORE starting ctrl.Run, so lastDispatch reflects real routed
	// traffic and the race starts from a known, freshly-dispatched baseline.
	rig.dispatch(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rig.ctrl.Run(ctx)

	// The real poll loop idle-fires: exactly one Unload, blocked in flight.
	rig.requireOrderLen(t, 1, "checkIdleLocked never fired an idle-triggered Unload through the real Router+Controller+activityBackend wiring")

	// Contention begins on the SAME instance while that Unload is in flight.
	// The actionDone chain (ADR-0014) must keep Contention's Unload queued
	// behind it: the core guarantee AC7 exists to prove.
	rig.det.setContended(true)
	time.Sleep(50 * time.Millisecond)
	if got := rig.u.orderLen(); got != 1 {
		t.Fatalf("a second Unload call ran before the idle-triggered Unload finished: order = %v, want [unload] (actionDone ordering violated)", rig.u.snapshot())
	}

	// Release it. Contention's chained second Unload now runs: an ACCEPTABLE
	// idempotent outcome per FR11/AC7, so asserting it is the PASS case.
	close(rig.u.release)
	rig.requireOrderLen(t, 2, "Contention's own unload never fired for the idle-unloaded instance")
	if y, reason := rig.ctrl.Yielding(); !y {
		t.Fatalf("Yielding() = (%v,%q), want yielding=true once Contention is active", y, reason)
	}

	// Clearing Contention must leave the idle-unloaded instance unloaded
	// (2026-10-20 change, ADR-0016 amendment); the next routed request wakes
	// it, and that Reload must land strictly after both Unload calls.
	rig.clearContention(t)
	rig.dispatch(t)
	rig.requireOrderLen(t, 3, "a routed request never woke the idle-unloaded instance after Contention cleared")

	got := rig.u.snapshot()
	want := []string{"unload", "unload", "reload"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("action order = %v, want %v — anything else (a Reload racing ahead of either Unload, or any other interleaving) would be the genuinely conflicting/out-of-order pair FR11/AC7 forbids; a second, later, strictly-ordered Unload (as seen here) is the documented, acceptable idempotent carve-out", got, want)
	}
}

// idleRouterRig is the REAL Router -> *activityBackend
// (backend.WithActivityTracking) -> *yield.Controller chain — the same
// composition cmd/broker/main.go's buildBroker wires up (see
// cmd/broker/main_test.go's TestBuildBrokerWiresActivityTrackingIntoRouter for
// the construction-order proof this builds on) — with a short idle duration so
// checkIdleLocked fires a real idle-triggered Unload via the controller's own
// poll loop (never a direct call into unexported yield internals, which this
// package cannot reach anyway).
//
// It reproduces plan.md's Architecture "case 2" exactly: Idle fires in an
// earlier tick; Contention begins later and its own unconditional
// per-instance unload loop (applyLocked) fires Unload again, idempotently, on
// the already-idle-unloaded instance. FR11/AC7 carve this out as an
// ACCEPTABLE outcome (order == [unload, unload, reload]), while still failing
// hard on what AC7 forbids: a Reload racing ahead of, or interleaved with,
// either Unload.
type idleRouterRig struct {
	det    *raceTestDetector
	u      *orderedIdleUnloader
	ctrl   *yield.Controller
	router *Router
}

func newIdleRouterRig(t *testing.T) *idleRouterRig {
	t.Helper()
	det := &raceTestDetector{}
	u := &orderedIdleUnloader{release: make(chan struct{})}

	// Short poll interval and idle timeout so the real poll loop fires
	// idle-unload within the test's time budget.
	const pollInterval = 5 * time.Millisecond
	const idleTimeout = 20 * time.Millisecond
	ctrl := yield.NewWithConfirmMulti(det, []yield.Unloader{u}, []string{"vllm-route"}, yield.Polling{Interval: pollInterval, ConfirmPolls: 1})
	ctrl.ConfigureIdle([]time.Duration{idleTimeout})

	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	tracked := WithActivityTracking(&fakeBackend{name: "vllm", handler: ok}, ctrl, 0)
	router := NewRouter(&fakeBackend{name: "default", handler: ok})
	router.AddRoute("vllm-model", "", tracked)
	return &idleRouterRig{det: det, u: u, ctrl: ctrl, router: router}
}

// dispatch sends one request for the routed model and expects a 200.
func (g *idleRouterRig) dispatch(t *testing.T) {
	t.Helper()
	w := httptest.NewRecorder()
	g.router.ProxyForLane("interactive").ServeHTTP(w, newTestRequest(t, `{"model":"vllm-model"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("dispatch status = %d, want 200", w.Code)
	}
}

// clearContention ends Contention and checks the instance was not reloaded.
func (g *idleRouterRig) clearContention(t *testing.T) {
	t.Helper()
	g.det.setContended(false)
	if !waitUntil(t, func() bool { y, _ := g.ctrl.Yielding(); return !y }) {
		t.Fatal("Contention never cleared")
	}
	time.Sleep(50 * time.Millisecond)
	if got := g.u.orderLen(); got != 2 {
		t.Fatalf("Contention-clear reloaded an idle-unloaded instance: order = %v, want [unload unload]", g.u.snapshot())
	}
}

func (g *idleRouterRig) requireOrderLen(t *testing.T, want int, failure string) {
	t.Helper()
	if !waitUntil(t, func() bool { return g.u.orderLen() == want }) {
		t.Fatal(failure)
	}
}

// TestRouterRoutingSummaryDoesNotPanicWithActivityTrackedBackend guards the
// comparability fix documented on WithActivityTracking (see activity.go):
// activityBackend must be returned as a pointer, never a bare value, because
// its proxy field holds an uncomparable closure — a bare value would panic
// with "comparing uncomparable type" the instant two model names shared the
// same decorated backend, which is exactly what RoutingSummary's map-key
// grouping does. TestWithActivityTrackingIsUsableAsMapKey (activity_test.go)
// proves the underlying comparability fact directly; this test proves it
// through Router.RoutingSummary(), the real production call site (/status's
// "routing" key) that would actually panic if the fix ever regressed.
func TestRouterRoutingSummaryDoesNotPanicWithActivityTrackedBackend(t *testing.T) {
	det := &raceTestDetector{}
	u := &orderedIdleUnloader{release: make(chan struct{})}
	close(u.release) // this test never drives an Unload/Reload call; avoid any accidental block

	ctrl := yield.NewWithConfirmMulti(det, []yield.Unloader{u}, []string{"vllm-route"}, yield.Polling{Interval: time.Hour, ConfirmPolls: 1})
	ctrl.ConfigureIdle([]time.Duration{5 * time.Minute})

	raw := &fakeBackend{name: "vllm", handler: func(w http.ResponseWriter, r *http.Request) {}}
	tracked := WithActivityTracking(raw, ctrl, 0)

	def := &fakeBackend{name: "default", handler: func(w http.ResponseWriter, r *http.Request) {}}
	r := NewRouter(def)
	r.AddRoute("model-a", "interactive", tracked)
	r.AddRoute("model-b", "interactive", tracked) // same *activityBackend-decorated backend

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("RoutingSummary() panicked with an activity-tracked backend shared by two models: %v", rec)
		}
	}()
	got := r.RoutingSummary()
	summaries, ok := got.([]RouteSummary)
	if !ok {
		t.Fatalf("RoutingSummary() returned %T, want []RouteSummary", got)
	}
	if len(summaries) != 1 {
		t.Fatalf("RoutingSummary() returned %d entries, want 1 (model-a/model-b grouped under the same activity-tracked backend); got %+v", len(summaries), summaries)
	}
	wantModels := []string{"model-a", "model-b"}
	sort.Strings(summaries[0].Models)
	if !reflect.DeepEqual(summaries[0].Models, wantModels) {
		t.Fatalf("grouped entry models = %v, want %v", summaries[0].Models, wantModels)
	}
}
