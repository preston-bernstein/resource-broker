package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// countingFakeBackend is fakeBackend (see router_test.go) plus a call
// counter on Proxy(), so tests can assert WithActivityTracking calls the
// underlying backend's Proxy() exactly once at wrap time — never per
// activityBackend.Proxy() call.
type countingFakeBackend struct {
	mu         sync.Mutex
	proxyCalls int
	handler    http.HandlerFunc
	unloader   yield.Unloader
}

func (f *countingFakeBackend) Proxy() http.Handler {
	f.mu.Lock()
	f.proxyCalls++
	f.mu.Unlock()
	return f.handler
}
func (f *countingFakeBackend) Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error) {
	return "", nil
}
func (f *countingFakeBackend) Reachable(ctx context.Context) error { return nil }
func (f *countingFakeBackend) Unloader() yield.Unloader            { return f.unloader }

func (f *countingFakeBackend) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.proxyCalls
}

// fakeIdleUnloader is a minimal yield.Unloader stub for constructing a real
// *yield.Controller with a configured Idle timeout.
type fakeIdleUnloader struct{}

func (fakeIdleUnloader) Unload(context.Context) error { return nil }
func (fakeIdleUnloader) Reload(context.Context) error { return nil }

// fakeIdleDetector is a minimal yield.Detector stub — never contends, so it
// doesn't interfere with idle bookkeeping under test.
type fakeIdleDetector struct{}

func (fakeIdleDetector) Detect() (string, bool) { return "", false }

// newTestIdleController builds a real *yield.Controller with one configured
// instance (a non-nil Unloader at orig-index 0) and an idle timeout on that
// instance, mirroring the pattern internal/yield's own tests (and
// router_test.go's fakeBackend) use to construct test fixtures.
func newTestIdleController(t *testing.T, idleTimeout time.Duration) *yield.Controller {
	t.Helper()
	ctrl := yield.NewWithConfirmMulti(fakeIdleDetector{}, []yield.Unloader{fakeIdleUnloader{}}, nil, time.Hour, 1)
	ctrl.ConfigureIdle([]time.Duration{idleTimeout})
	return ctrl
}

// TestWithActivityTrackingDispatchUpdatesController proves a request routed
// through WithActivityTracking's returned Backend.Proxy() actually reaches
// the wrapped Controller's idle bookkeeping: after dispatch, IdleSummary
// reports a near-zero since_last_dispatch for the tracked instance.
func TestWithActivityTrackingDispatchUpdatesController(t *testing.T) {
	fb := &countingFakeBackend{
		handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}
	ctrl := newTestIdleController(t, 5*time.Minute)

	tracked := WithActivityTracking(fb, ctrl, 0)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	tracked.Proxy().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", w.Code, http.StatusOK)
	}

	summaryAny := ctrl.IdleSummary()
	summary, ok := summaryAny.([]yield.IdleSummaryEntry)
	if !ok || len(summary) != 1 {
		t.Fatalf("IdleSummary() = %#v, want a single-entry []yield.IdleSummaryEntry", summaryAny)
	}
	if summary[0].IdleUnloaded {
		t.Fatalf("IdleSummary()[0].IdleUnloaded = true after a fresh dispatch, want false")
	}
	// since_last_dispatch should be a small duration string (e.g. "0s" or a
	// few hundred microseconds), not the multi-hour value it would show if
	// the request never reached the Controller's bookkeeping at all.
	if d, err := time.ParseDuration(summary[0].SinceLastDispatch); err != nil {
		t.Fatalf("SinceLastDispatch %q did not parse as a duration: %v", summary[0].SinceLastDispatch, err)
	} else if d > 5*time.Second {
		t.Fatalf("SinceLastDispatch = %v immediately after dispatch, want near-zero", d)
	}
}

// TestWithActivityTrackingProxyIsCachedNotRewrapped proves WithActivityTracking
// calls the underlying backend's Proxy() exactly once, at wrap time — never
// again on subsequent activityBackend.Proxy() calls — and that every call to
// activityBackend.Proxy() returns the exact same handler instance.
func TestWithActivityTrackingProxyIsCachedNotRewrapped(t *testing.T) {
	fb := &countingFakeBackend{
		handler: func(w http.ResponseWriter, r *http.Request) {},
	}
	ctrl := newTestIdleController(t, time.Minute)

	tracked := WithActivityTracking(fb, ctrl, 0)

	h1 := tracked.Proxy()
	h2 := tracked.Proxy()

	if reflect.ValueOf(h1).Pointer() != reflect.ValueOf(h2).Pointer() {
		t.Fatalf("Proxy() returned different handler instances across calls")
	}
	if got := fb.calls(); got != 1 {
		t.Fatalf("underlying backend Proxy() called %d times, want exactly 1 (wrap-time only)", got)
	}
}

// TestWithActivityTrackingIsUsableAsMapKey directly guards the
// pointer-not-value requirement documented on WithActivityTracking: if it
// ever returned a bare activityBackend value instead of a pointer, using the
// result as (part of) a map key would panic with "comparing uncomparable
// type" the moment two decorated backends landed in the same map — exactly
// what Router.RoutingSummary does on every call.
func TestWithActivityTrackingIsUsableAsMapKey(t *testing.T) {
	fb1 := &countingFakeBackend{handler: func(w http.ResponseWriter, r *http.Request) {}}
	fb2 := &countingFakeBackend{handler: func(w http.ResponseWriter, r *http.Request) {}}
	ctrl := newTestIdleController(t, time.Minute)

	tracked1 := WithActivityTracking(fb1, ctrl, 0)
	tracked2 := WithActivityTracking(fb2, ctrl, 0)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("using decorated backends as map keys panicked: %v", r)
		}
	}()

	m := make(map[Backend]string)
	m[tracked1] = "one"
	m[tracked2] = "two"

	if len(m) != 2 {
		t.Fatalf("map has %d entries, want 2", len(m))
	}
}
