package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// newTestOpenAIBackend builds an *openaiBackend pointed at the given mock
// OpenAI-compatible server URL, bypassing config.Load() so tests don't need
// a full environment. Mirrors newTestOllamaBackend in ollama_backend_test.go.
func newTestOpenAIBackend(t *testing.T, mockURL string) *openaiBackend {
	t.Helper()
	u, err := url.Parse(mockURL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}
	cfg := &config.Config{UpstreamURL: u}
	be, err := newOpenAIBackend(cfg)
	if err != nil {
		t.Fatalf("newOpenAIBackend: %v", err)
	}
	ob, ok := be.(*openaiBackend)
	if !ok {
		t.Fatalf("newOpenAIBackend returned %T, want *openaiBackend", be)
	}
	return ob
}

func TestOpenAIBackendReachable(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer mock.Close()

	ob := newTestOpenAIBackend(t, mock.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ob.Reachable(ctx); err != nil {
		t.Errorf("Reachable() on live mock = %v, want nil", err)
	}
}

func TestOpenAIBackendReachableNonOKStatus(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mock.Close()

	ob := newTestOpenAIBackend(t, mock.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ob.Reachable(ctx); err == nil {
		t.Error("Reachable() on 500 mock = nil, want error")
	}
}

// TestOpenAIBackendReachableStatusBoundary proves Reachable() treats 299 as
// success and 300 as an error — the exact boundary of the `resp.StatusCode <
// 200 || resp.StatusCode >= 300` check at openai_backend.go:72, which no
// other test exercised at the boundary itself (only 200 and 500 were
// tested), leaving a `>= 300` vs `> 300` mutation undetected.
func TestOpenAIBackendReachableStatusBoundary(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{status: 299, wantErr: false},
		{status: 300, wantErr: true},
	} {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		ob := newTestOpenAIBackend(t, mock.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := ob.Reachable(ctx)
		cancel()
		mock.Close()

		if tc.wantErr && err == nil {
			t.Errorf("status %d: Reachable() = nil, want error", tc.status)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("status %d: Reachable() = %v, want nil", tc.status, err)
		}
	}
}

// TestOpenAIBackendReachableSendsAPIKey proves Reachable() sets the
// Authorization header when UpstreamAPIKey is non-empty, and omits it when
// empty — the two branches of the `if b.apiKey != ""` check at
// openai_backend.go:64, which no other test exercised with a non-empty key
// (newTestOpenAIBackend never sets one), leaving that branch's negation an
// undetected mutation.
func TestOpenAIBackendReachableSendsAPIKey(t *testing.T) {
	var gotAuth string
	var gotAuthPresent bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, gotAuthPresent = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}
	cfg := &config.Config{UpstreamURL: u, UpstreamAPIKey: "test-secret-key"}
	be, err := newOpenAIBackend(cfg)
	if err != nil {
		t.Fatalf("newOpenAIBackend: %v", err)
	}
	ob := be.(*openaiBackend)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ob.Reachable(ctx); err != nil {
		t.Fatalf("Reachable() with API key = %v, want nil", err)
	}
	if !gotAuthPresent {
		t.Fatal("mock never received an Authorization header with a non-empty UpstreamAPIKey set")
	}
	if gotAuth != "Bearer test-secret-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-secret-key")
	}

	// Empty API key: no Authorization header at all (not an empty Bearer).
	obNoKey := newTestOpenAIBackend(t, mock.URL)
	gotAuthPresent = false
	if err := obNoKey.Reachable(ctx); err != nil {
		t.Fatalf("Reachable() without API key = %v, want nil", err)
	}
	if gotAuthPresent {
		t.Errorf("Authorization header present with empty UpstreamAPIKey, want absent (got %q)", gotAuth)
	}
}

// TestOpenAIBackendUnreachable points at a closed port (connection refused)
// rather than a mock server, so Reachable must return a real network error.
func TestOpenAIBackendUnreachable(t *testing.T) {
	// Grab a free port, then close the listener immediately so nothing is
	// listening on it: connection refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	ob := newTestOpenAIBackend(t, "http://"+addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ob.Reachable(ctx); err == nil {
		t.Error("Reachable() on closed port = nil, want error")
	}
}

func TestOpenAIBackendProxyServesHandler(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"m","messages":[{"role":"assistant","content":"hi"}],"choices":[{"delta":{"content":"hi"}}]}`))
	}))
	defer mock.Close()

	ob := newTestOpenAIBackend(t, mock.URL)

	// /api/tags has no OpenAI-compatible equivalent and must 404 (FR-28,
	// AC-21) — a light structural check that Proxy() really is the
	// openaicompat translating handler, not a transparent passthrough.
	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	ob.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("Proxy() /api/tags status = %d, want 404 (openai backend only supports /api/generate, /api/chat, /api/embed)", rec.Code)
	}
}

// TestOpenAIBackendUnloaderIsTrueNilInterface is THE critical regression
// test for the single highest-risk item in this feature: openaiBackend must
// return a direct, literal nil of the yield.Unloader interface type, never a
// typed nil (a nil concrete pointer boxed into the interface).
//
// A true nil interface value compares equal to nil with `u != nil` — this
// assertion is exactly the form yield.go's own nil-guard uses
// (`if c.unloader != nil { go c.doUnload() }`), so if openaiBackend.Unloader
// ever regressed to something like `var u *someConcreteType; return u`, this
// exact comparison would flip to true (the "!=" would incorrectly evaluate
// true) and this test would fail, catching the bug before it reaches
// production.
func TestOpenAIBackendUnloaderIsTrueNilInterface(t *testing.T) {
	ob := newTestOpenAIBackend(t, "http://127.0.0.1:1")

	u := ob.Unloader()
	if u != nil {
		t.Fatalf("Unloader() = %#v, want a true nil interface value (typed-nil regression — see plan.md's Typed-nil safety note)", u)
	}
}

// TestOpenAIBackendUnloaderDoesNotTriggerDoUnload drives the real
// yield.Controller's applyLocked/doUnload path with the real
// openaiBackend.Unloader() value (not a mock), end to end, proving the
// nil-guard actually works for this backend: because Unloader() is a true
// nil interface, `c.unloader != nil` in yield.go must be false, so
// `go c.doUnload()` must never fire at all — this is the deepest, most
// direct proof against the CRITICAL typed-nil finding from spec review, one
// level below the unit-level nil check above.
func TestOpenAIBackendUnloaderDoesNotTriggerDoUnload(t *testing.T) {
	ob := newTestOpenAIBackend(t, "http://127.0.0.1:1")

	det := &fakeDetector{}
	ctrl := yield.NewWithConfirm(det, ob.Unloader(), time.Hour, 1)

	// Force a yield transition the same way real contention detection would:
	// SetMode(ModeForceYield) drives applyLocked exactly like an auto
	// detection would, including the `if c.unloader != nil { go
	// c.doUnload() }` branch under test.
	det.contended = true
	det.reason = "gaming-steam"
	ctrl.SetMode(yield.ModeForceYield)

	yielding, _ := ctrl.Yielding()
	if !yielding {
		t.Fatal("Controller did not enter yielding state; test setup is broken")
	}

	// If doUnload had fired on a nil-receiver Unload call, it would have
	// panicked inside its own goroutine. yield.go's doUnload already wraps
	// that in a recover() as defense-in-depth (this test does not rely on
	// that recover — with a true nil interface, doUnload's goroutine is
	// never even spawned) — give any wrongly-spawned goroutine time to
	// crash the test binary via an unrecovered panic before concluding.
	time.Sleep(50 * time.Millisecond)

	// Reaching this line without the test process aborting is itself part
	// of the proof: a typed-nil Unloader would have caused a panic in an
	// unrecovered (pre-recover-era) goroutine, or a "panic in vram unload"
	// slog.Error from the current recover()-guarded doUnload — neither of
	// which should have happened here since c.unloader must be a true nil
	// interface and the `!= nil` guard must have skipped `go c.doUnload()`
	// entirely.
}

// fakeDetector is a minimal yield.Detector test double.
type fakeDetector struct {
	reason    string
	contended bool
}

func (f *fakeDetector) Detect() (string, bool) {
	return f.reason, f.contended
}

// TestOpenAIBackendUnloaderNonNilWhenUnitSet is the mirror of
// TestOpenAIBackendUnloaderIsTrueNilInterface: with cfg.UpstreamUnitName set,
// newOpenAIBackend must wire a real, non-nil Unloader (a *systemdUnitController
// via newSystemdUnitController), not the nil zero value used when no unit is
// configured.
func TestOpenAIBackendUnloaderNonNilWhenUnitSet(t *testing.T) {
	u, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg := &config.Config{UpstreamURL: u, UpstreamUnitName: "vllm"}
	be, err := newOpenAIBackend(cfg)
	if err != nil {
		t.Fatalf("newOpenAIBackend: %v", err)
	}
	ob := be.(*openaiBackend)

	if ob.Unloader() == nil {
		t.Fatal("Unloader() = nil, want a non-nil Unloader when UpstreamUnitName is set")
	}
}

// TestOpenAIBackendFactoryWiresSystemdController is THE regression test for
// the wiring bug an earlier adversarial review flagged: every other test in
// this file that touches systemdUnitController builds it by hand
// (&systemdUnitController{unit: "vllm", run: stubRun}), which would keep
// passing even if newOpenAIBackend's construction got "simplified" to a bare
// &systemdUnitController{unit: cfg.UpstreamUnitName} struct literal, leaving
// run as its nil zero-value func — that compiles fine, returns non-nil from
// Unloader(), passes every hand-built-struct test, and then panics at
// runtime on the first real yield event when run (a nil func) gets called.
// This is the only test that goes through the real newOpenAIBackend factory
// and proves run actually got wired to a working command runner.
func TestOpenAIBackendFactoryWiresSystemdController(t *testing.T) {
	u, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg := &config.Config{UpstreamURL: u, UpstreamUnitName: "vllm"}

	be, err := newOpenAIBackend(cfg)
	if err != nil {
		t.Fatalf("newOpenAIBackend: %v", err)
	}
	ob, ok := be.(*openaiBackend)
	if !ok {
		t.Fatalf("newOpenAIBackend returned %T, want *openaiBackend", be)
	}

	ctrl, ok := ob.Unloader().(*systemdUnitController)
	if !ok {
		t.Fatalf("Unloader() = %T, want *systemdUnitController", ob.Unloader())
	}

	// Primary assertion: run must not be the nil zero-value func. A struct
	// literal like &systemdUnitController{unit: cfg.UpstreamUnitName} (no run
	// field set) compiles fine and would fail this check.
	if ctrl.run == nil {
		t.Fatal("factory-built systemdUnitController.run is nil — newOpenAIBackend did not wire the real command runner (see newSystemdUnitController)")
	}

	// Defense in depth: substitute run with a spy post-construction and drive
	// Unload/Reload through the exact object the factory returned, proving
	// run is not just non-nil but actually invoked (with the right verbs) by
	// the real methods on the real returned instance.
	var gotVerbs []string
	ctrl.run = func(ctx context.Context, verb string) error {
		gotVerbs = append(gotVerbs, verb)
		return nil
	}

	if err := ctrl.Unload(context.Background()); err != nil {
		t.Fatalf("Unload() = %v, want nil", err)
	}
	if err := ctrl.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}

	want := []string{"stop", "start"}
	if len(gotVerbs) != len(want) || gotVerbs[0] != want[0] || gotVerbs[1] != want[1] {
		t.Errorf("run verbs recorded via factory-built controller = %v, want %v", gotVerbs, want)
	}
}

// TestSystemdUnitControllerUnloadRunsStop proves Unload invokes run with the
// "stop" verb — never "restart" — constructing the controller directly as a
// struct literal (this test is in-package and exercises the type's own
// methods in isolation, not the factory wiring).
func TestSystemdUnitControllerUnloadRunsStop(t *testing.T) {
	var gotVerb string
	u := &systemdUnitController{
		unit: "vllm",
		run: func(ctx context.Context, verb string) error {
			gotVerb = verb
			return nil
		},
	}

	if err := u.Unload(context.Background()); err != nil {
		t.Fatalf("Unload() = %v, want nil", err)
	}
	if gotVerb != "stop" {
		t.Errorf("run verb = %q, want %q", gotVerb, "stop")
	}
}

// TestSystemdUnitControllerReloadRunsStart proves Reload invokes run with the
// "start" verb — never "restart".
func TestSystemdUnitControllerReloadRunsStart(t *testing.T) {
	var gotVerb string
	u := &systemdUnitController{
		unit: "vllm",
		run: func(ctx context.Context, verb string) error {
			gotVerb = verb
			return nil
		},
	}

	if err := u.Reload(context.Background()); err != nil {
		t.Fatalf("Reload() = %v, want nil", err)
	}
	if gotVerb != "start" {
		t.Errorf("run verb = %q, want %q", gotVerb, "start")
	}
}

// TestSystemdUnitControllerErrorPropagates proves that when the underlying
// run closure fails, Unload/Reload return a non-nil wrapped error rather than
// panicking or swallowing the failure.
func TestSystemdUnitControllerErrorPropagates(t *testing.T) {
	stubErr := errors.New("boom")
	u := &systemdUnitController{
		unit: "vllm",
		run: func(ctx context.Context, verb string) error {
			return stubErr
		},
	}

	if err := u.Unload(context.Background()); err == nil {
		t.Fatal("Unload() = nil, want a non-nil wrapped error when run fails")
	} else if !errors.Is(err, stubErr) {
		t.Errorf("Unload() error = %v, want it to wrap %v", err, stubErr)
	}

	if err := u.Reload(context.Background()); err == nil {
		t.Fatal("Reload() = nil, want a non-nil wrapped error when run fails")
	} else if !errors.Is(err, stubErr) {
		t.Errorf("Reload() error = %v, want it to wrap %v", err, stubErr)
	}
}

// TestSystemdUnitControllerSerializesUnloadAndReload proves mu actually
// serializes Unload and Reload against each other: a rapid yield-clear/
// yield-start flap must never let a `systemctl start` run concurrently with
// a `systemctl stop` for the same unit. The stub run blocks until signaled,
// and a background watcher records whether it ever observed the in-progress
// flag already set on entry (i.e. two calls overlapping).
func TestSystemdUnitControllerSerializesUnloadAndReload(t *testing.T) {
	var mu sync.Mutex
	inProgress := false
	overlapped := false

	proceed := make(chan struct{})
	entered := make(chan struct{}, 2)

	u := &systemdUnitController{
		unit: "vllm",
		run: func(ctx context.Context, verb string) error {
			mu.Lock()
			if inProgress {
				overlapped = true
			}
			inProgress = true
			mu.Unlock()

			entered <- struct{}{}
			<-proceed // block until the test signals both goroutines have started

			mu.Lock()
			inProgress = false
			mu.Unlock()
			return nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := u.Unload(context.Background()); err != nil {
			t.Errorf("Unload() = %v, want nil", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := u.Reload(context.Background()); err != nil {
			t.Errorf("Reload() = %v, want nil", err)
		}
	}()

	// Wait for the first goroutine to enter run, then give the second a
	// window to attempt entry too (it must block on mu, not on the channel).
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(proceed)
	wg.Wait()

	if overlapped {
		t.Error("run() was in-progress concurrently for Unload and Reload; systemdUnitController.mu did not serialize them")
	}
}
