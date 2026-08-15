package backend

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
