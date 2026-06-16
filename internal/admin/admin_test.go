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

func newMux(c Controller) http.Handler {
	return Mux(c, fakeStats{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "metrics")
	}))
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(&fakeCtrl{}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

func TestControlPostValid(t *testing.T) {
	c := &fakeCtrl{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/control", strings.NewReader(`{"mode":"yield"}`))
	newMux(c).ServeHTTP(rec, req)
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
	req := httptest.NewRequest("POST", "/control", strings.NewReader(`{"mode":"bogus"}`))
	newMux(c).ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if c.set {
		t.Fatal("SetMode should not be called for invalid mode")
	}
}

func TestControlPostBadJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/control", strings.NewReader(`{not json`))
	newMux(&fakeCtrl{}).ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400", rec.Code)
	}
}

func TestControlMethodNotAllowed(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/control", nil)
	newMux(&fakeCtrl{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(&fakeCtrl{}).ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"queue"`) || !strings.Contains(body, `"yield"`) {
		t.Fatalf("status: %d %q", rec.Code, body)
	}
}
