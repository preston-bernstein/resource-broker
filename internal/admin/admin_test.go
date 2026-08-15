package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/queue"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

type fakeCtrl struct {
	mode yield.Mode
	set  bool
}

func (f *fakeCtrl) SetMode(m yield.Mode) { f.mode = m; f.set = true }
func (f *fakeCtrl) Snapshot() yield.State {
	return yield.State{Mode: f.mode.String(), Yielding: f.mode == yield.ModeForceYield}
}

type fakeStats struct{}

func (fakeStats) Stats() queue.Stats { return queue.Stats{Busy: true, Interactive: 1, Batch: 2} }

func newMux(c Controller, token string) http.Handler {
	return newMuxWithHealth(c, nil, token)
}

func newMuxWithHealth(c Controller, health HealthCheck, token string) http.Handler {
	return Mux(c, fakeStats{}, health, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "metrics")
	}), nil, nil, nil, nil, nil, token)
}

// loopbackReq is httptest.NewRequest with RemoteAddr overridden to loopback
// (the default, 192.0.2.1:1234, is not loopback) so tests of /control's
// business logic aren't tripped up by the ADR-0005 auth gate.
func loopbackReq(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(&fakeCtrl{}, "").ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

// --- ADR-0010: /healthz becomes a real readiness check ---

// TestHealthzWithCheckerHealthy proves a configured HealthCheck that returns
// nil still yields 200, with a body that says so.
func TestHealthzWithCheckerHealthy(t *testing.T) {
	rec := httptest.NewRecorder()
	health := func(context.Context) error { return nil }
	newMuxWithHealth(&fakeCtrl{}, health, "").ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz (healthy): %d %q", rec.Code, rec.Body.String())
	}
}

// TestHealthzWithCheckerUnhealthy is the defect this ADR fixes, pinned as a
// test: /healthz must be able to fail, and the body must name what's wrong —
// not report "ok" while the broker can't actually reach Ollama, the job
// store, or the detector loop (the clamd-shaped defect from the 2026-08-01
// audit).
func TestHealthzWithCheckerUnhealthy(t *testing.T) {
	rec := httptest.NewRecorder()
	health := func(context.Context) error {
		return errors.New("ollama upstream unreachable: dial tcp: connection refused")
	}
	newMuxWithHealth(&fakeCtrl{}, health, "").ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz (unhealthy): status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ollama upstream unreachable") {
		t.Fatalf("healthz (unhealthy) body does not name the failed dependency: %q", rec.Body.String())
	}
}

// TestHealthzTimeoutBound proves a HealthCheck that never returns still
// resolves the HTTP response — the probe must not hang forever just because
// one dependency did.
func TestHealthzTimeoutBound(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	health := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		newMuxWithHealth(&fakeCtrl{}, health, "").ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("healthz did not resolve within its own timeout bound")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz (timeout): status = %d, want 503", rec.Code)
	}
}

func TestControlPostValid(t *testing.T) {
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/control", strings.NewReader(`{"mode":"yield"}`))
	newMux(c, "").ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !c.set || c.mode != yield.ModeForceYield {
		t.Fatalf("SetMode not applied: set=%v mode=%v", c.set, c.mode)
	}
}

func TestControlPostInvalidMode(t *testing.T) {
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/control", strings.NewReader(`{"mode":"bogus"}`))
	newMux(c, "").ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if c.set {
		t.Fatal("SetMode should not be called for invalid mode")
	}
}

func TestControlPostBadJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/control", strings.NewReader(`{not json`))
	newMux(&fakeCtrl{}, "").ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

func TestControlMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/control", nil)
	newMux(&fakeCtrl{}, "").ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

// TestStatusBaselineFixture pins testdata/status_baseline.json as the
// pre-feature /status shape: newMux (see above) builds Mux with jobStatus,
// tdarrStatus, and routingStatus all nil — exactly the base case a future
// idleStatus parameter (added by a later task) will also default to when its
// config vars are unset. AC1 for the idle-unload feature is that /status is
// byte-for-byte unchanged in that unset case; this fixture is the "before"
// side of that diff.
//
// The "schedule" section is the one part of /status that is NOT deterministic
// across captures: schedule.TakeSnapshot(time.Now()) reflects the real wall
// clock against internal/schedule's weekly window table (see
// internal/schedule/schedule.go), so active_windows/safe_for_tdarr legitimately
// differ depending on when the test runs. A future byte-for-byte AC1
// comparison test must normalize or exclude "schedule" before diffing; this
// test only checks its shape, not its value, for that reason.
func TestStatusBaselineFixture(t *testing.T) {
	fixtureBytes, err := os.ReadFile("testdata/status_baseline.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}

	if _, ok := fixture["idle"]; ok {
		t.Fatal(`fixture must not contain an "idle" key — it must represent the pre-feature /status shape`)
	}
	for _, k := range []string{"jobs", "tdarr", "routing"} {
		if _, ok := fixture[k]; ok {
			t.Fatalf("fixture must not contain optional key %q — newMux's base case is jobStatus/tdarrStatus/routingStatus all nil", k)
		}
	}
	wantKeys := map[string]bool{"yield": true, "queue": true, "schedule": true}
	for k := range fixture {
		if !wantKeys[k] {
			t.Fatalf("fixture has unexpected top-level key %q", k)
		}
	}
	for k := range wantKeys {
		if _, ok := fixture[k]; !ok {
			t.Fatalf("fixture is missing expected top-level key %q", k)
		}
	}

	rec := httptest.NewRecorder()
	newMux(&fakeCtrl{}, "").ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var live map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("live /status body is not valid JSON: %v", err)
	}

	// "schedule" is time-dependent (see comment above); check shape only.
	sched, ok := live["schedule"].(map[string]any)
	if !ok {
		t.Fatal(`live "schedule" is not an object`)
	}
	if _, ok := sched["active_windows"]; !ok {
		t.Fatal(`live "schedule" missing "active_windows"`)
	}
	if _, ok := sched["safe_for_tdarr"]; !ok {
		t.Fatal(`live "schedule" missing "safe_for_tdarr"`)
	}
	delete(live, "schedule")
	delete(fixture, "schedule")

	liveNorm, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	fixtureNorm, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var liveCanon, fixtureCanon map[string]any
	json.Unmarshal(liveNorm, &liveCanon)
	json.Unmarshal(fixtureNorm, &fixtureCanon)
	liveCanonJSON, _ := json.Marshal(liveCanon)
	fixtureCanonJSON, _ := json.Marshal(fixtureCanon)
	if string(liveCanonJSON) != string(fixtureCanonJSON) {
		t.Fatalf("live /status (minus schedule) does not match testdata/status_baseline.json:\nlive:    %s\nfixture: %s", liveCanonJSON, fixtureCanonJSON)
	}
}

func TestStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(&fakeCtrl{}, "").ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"queue"`) || !strings.Contains(body, `"yield"`) {
		t.Fatalf("status: %d %q", rec.Code, body)
	}
}

// TestStatusWithIdleStatus verifies that idleStatus is wired into /status
// when provided.
func TestStatusWithIdleStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	idleStatus := func() any { return []string{"fake"} }
	mux := Mux(&fakeCtrl{}, fakeStats{}, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "metrics")
	}), nil, nil, nil, nil, idleStatus, "")
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var live map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("live /status body is not valid JSON: %v", err)
	}
	if _, ok := live["idle"]; !ok {
		t.Fatal(`live /status missing "idle" key when idleStatus is provided`)
	}
}

// TestStatusWithoutIdleStatus verifies that /status does not contain an
// "idle" key when idleStatus is nil.
func TestStatusWithoutIdleStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	mux := Mux(&fakeCtrl{}, fakeStats{}, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "metrics")
	}), nil, nil, nil, nil, nil, "")
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var live map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("live /status body is not valid JSON: %v", err)
	}
	if _, ok := live["idle"]; ok {
		t.Fatal(`live /status should not contain "idle" key when idleStatus is nil`)
	}
}

// --- ADR-0005: POST /control auth gate ---

func TestControlGetNeverRequiresAuth(t *testing.T) {
	// GET /control (state read) stays open even with a token configured and a
	// non-loopback, unauthenticated caller.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/control", nil)
	newMux(&fakeCtrl{}, "s3cret").ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /control status %d, want 200", rec.Code)
	}
}

func TestControlPostNoTokenNonLoopbackRejected(t *testing.T) {
	// No BROKER_CONTROL_TOKEN configured, request from a non-loopback address:
	// must be rejected (loopback-only fallback).
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/control", strings.NewReader(`{"mode":"yield"}`))
	newMux(c, "").ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if c.set {
		t.Fatal("SetMode must not be called for an unauthorized request")
	}
}

func TestControlPostNoTokenLoopbackAccepted(t *testing.T) {
	// No BROKER_CONTROL_TOKEN configured, request from loopback: accepted
	// (zero-config-safe default so SSH-local control keeps working).
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/control", strings.NewReader(`{"mode":"yield"}`))
	newMux(c, "").ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if !c.set {
		t.Fatal("SetMode should have been applied")
	}
}

func TestControlPostTokenConfiguredMissingHeaderRejected(t *testing.T) {
	// Token configured, no Authorization header at all, even from loopback:
	// once a token is configured it is required regardless of source.
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := loopbackReq("POST", "/control", strings.NewReader(`{"mode":"yield"}`))
	newMux(c, "s3cret").ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if c.set {
		t.Fatal("SetMode must not be called for an unauthorized request")
	}
}

func TestControlPostTokenMismatchRejected(t *testing.T) {
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/control", strings.NewReader(`{"mode":"yield"}`))
	req.Header.Set("Authorization", "Bearer wrong-token")
	newMux(c, "s3cret").ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if c.set {
		t.Fatal("SetMode must not be called for a mismatched token")
	}
}

func TestControlPostTokenMatchAccepted(t *testing.T) {
	// Correct token from a non-loopback address: accepted (token auth doesn't
	// require loopback).
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/control", strings.NewReader(`{"mode":"yield"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	newMux(c, "s3cret").ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if !c.set || c.mode != yield.ModeForceYield {
		t.Fatalf("SetMode not applied: set=%v mode=%v", c.set, c.mode)
	}
}
