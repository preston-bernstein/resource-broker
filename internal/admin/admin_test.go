package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
	"github.com/preston-bernstein/ollama-resource-broker/internal/yield"
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
	return Mux(c, fakeStats{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "metrics")
	}), nil, nil, nil, token)
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

func TestStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(&fakeCtrl{}, "").ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"queue"`) || !strings.Contains(body, `"yield"`) {
		t.Fatalf("status: %d %q", rec.Code, body)
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
