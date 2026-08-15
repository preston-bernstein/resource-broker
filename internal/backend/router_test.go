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
	"testing"

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
func (f *fakeBackend) Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error) {
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
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(body))
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
// verified directly against peekModel, which is the only place Router reads
// from the body before forwarding.
func TestRouterLargeBodyModelPeekNotFullyBuffered(t *testing.T) {
	// Build an ~8MB body: {"model":"vision-model","images":["<8MB of base64-ish filler>"]}
	filler := strings.Repeat("A", 8*1024*1024)
	body := `{"model":"vision-model","images":["` + filler + `"]}`

	// 1. Prove peekModel itself doesn't buffer the large payload: consumed
	// bytes should be a tiny fraction of the 8MB body, since "model" is the
	// first field and peekModel stops right after decoding it.
	model, consumed, err := peekModel(strings.NewReader(body))
	if err != nil {
		t.Fatalf("peekModel() error = %v", err)
	}
	if model != "vision-model" {
		t.Fatalf("peekModel() model = %q, want %q", model, "vision-model")
	}
	if len(consumed) > 4096 {
		t.Fatalf("peekModel() consumed %d bytes finding an early model field, want a small peek (not the full %d-byte body)", len(consumed), len(body))
	}

	// 2. Prove the full dispatch path still forwards a byte-identical body
	// to the resolved backend.
	hit := make(chan string, 1)
	bodies := make(chan []byte, 1)
	def := recordingBackend("default", hit, bodies)
	vision := recordingBackend("vision", hit, bodies)

	r := NewRouter(def)
	r.AddRoute("vision-model", "", vision)

	handler := r.ProxyForLane("interactive")
	req := newTestRequest(t, body)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := <-hit; got != "vision" {
		t.Fatalf("large vision request reached backend %q, want vision", got)
	}
	got := <-bodies
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("forwarded body not byte-identical: got %d bytes, want %d bytes", len(got), len(body))
	}

	// Sanity: the decoded model field really does round-trip byte-for-byte
	// through the forwarded body (FR-6 requirement stated explicitly).
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("forwarded body is not valid JSON: %v", err)
	}
	if decoded.Model != "vision-model" {
		t.Fatalf("forwarded body model = %q, want %q", decoded.Model, "vision-model")
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

	var grouped, solitary *RouteSummary
	for i := range summaries {
		s := &summaries[i]
		switch len(s.Models) {
		case 2:
			grouped = s
		case 1:
			solitary = s
		}
	}
	if grouped == nil {
		t.Fatalf("no 2-model grouped entry found in %+v", summaries)
	}
	if grouped.Lane != "interactive" {
		t.Fatalf("grouped entry lane = %q, want %q", grouped.Lane, "interactive")
	}
	wantModels := []string{"model-a", "model-b"}
	sort.Strings(grouped.Models)
	if !reflect.DeepEqual(grouped.Models, wantModels) {
		t.Fatalf("grouped entry models = %v, want %v", grouped.Models, wantModels)
	}

	if solitary == nil {
		t.Fatalf("no 1-model entry found in %+v", summaries)
	}
	if solitary.Models[0] != "model-c" || solitary.Lane != "batch" {
		t.Fatalf("solitary entry = %+v, want models=[model-c] lane=batch", solitary)
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
func (g *genBackend) Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error) {
	if g.generateErr != nil {
		return "", g.generateErr
	}
	return g.name + ":" + model, nil
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

	got, err := r.Generate(context.Background(), "qwen", "hi", nil, nil)
	if err != nil {
		t.Fatalf("Generate(routed model) error = %v", err)
	}
	if want := "alt:qwen"; got != want {
		t.Fatalf("Generate(routed model) = %q, want %q", got, want)
	}

	got, err = r.Generate(context.Background(), "bert", "hi", nil, nil)
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
