package broker_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/preston-bernstein/resource-broker/internal/config"
)

// routedRig is a rig with a default ollama-shaped upstream and one routed
// openai-shaped instance for "routed-model"; both count the calls they get.
type routedRig struct {
	*rig
	defaultCalls, routedCalls *atomic.Int32
}

// newRoutedRig wires the two upstreams. defaultReply is the Ollama-shaped JSON
// the default upstream answers with; routedEvents are the SSE events the routed
// instance answers with (nil makes it answer with a plain JSON chat completion).
func newRoutedRig(t *testing.T, defaultReply map[string]any, routedEvents ...string) *routedRig {
	t.Helper()
	var defaultCalls, routedCalls atomic.Int32
	defaultUpstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalls.Add(1)
		writeJSON(w, defaultReply)
	}))
	routedUpstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routedCalls.Add(1)
		if routedEvents == nil {
			writeJSON(w, map[string]any{"id": "x", "object": "chat.completion", "choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "hi"}}}})
			return
		}
		writeSSE(w, routedEvents...)
	}))

	cfg := ollamaCfg(t, defaultUpstream.URL)
	cfg.Routes = []config.RouteBackend{{Models: []string{"routed-model"}, Backend: "openai", URL: mustParseURL(t, routedUpstream.URL)}}
	r := newRig(t, cfg)
	if r.router == nil {
		t.Fatal("newRig did not construct a Router despite cfg.Routes being set")
	}
	return &routedRig{rig: r, defaultCalls: &defaultCalls, routedCalls: &routedCalls}
}

// TestAcceptance_RoutingContentionBlocksAllConfiguredInstances proves
// docs/per-model-backend-routing/requirements.md's own AC-12 (distinct from
// this suite's pre-existing AC-12, which predates per-model routing): during
// active gaming/Plex contention, no request — regardless of which model or
// backend instance it targets — is admitted to any configured instance. Two
// real upstreams are wired (default ollama-shaped, one routed openai-shaped
// instance for "routed-model"); both track whether they were ever actually
// called. Forcing yield via the real POST /control path (the same mechanism
// TestAcceptance_AC7 uses) and then sending one request for the routed model
// and one for an unrouted model must defer both with 503, and neither
// upstream may ever see a request — proving the yield gate blocks admission
// before Router ever dispatches, for every instance alike.
func TestAcceptance_RoutingContentionBlocksAllConfiguredInstances(t *testing.T) {
	r := newRoutedRig(t, map[string]any{"model": "m", "response": "hi", "done": true})

	requireStatus(t, post(t, r.control.URL+"/control", `{"mode":"yield"}`), http.StatusOK)

	routedStatus, _ := postDrained(t, r.interactive.URL+"/api/chat", `{"model":"routed-model","messages":[]}`)
	if routedStatus != http.StatusServiceUnavailable {
		t.Errorf("routed-model request during yield: status = %d, want 503", routedStatus)
	}
	defaultStatus, _ := postDrained(t, r.interactive.URL+"/api/generate", `{"model":"unrouted-model","prompt":"hi"}`)
	if defaultStatus != http.StatusServiceUnavailable {
		t.Errorf("unrouted-model request during yield: status = %d, want 503", defaultStatus)
	}

	// routedCalls must be exactly 0: the route has no _UNIT_NAME configured,
	// so its Unloader() is nil (no unload call is ever made to it), and the
	// routed inference request itself was correctly deferred with 503 above
	// — nothing legitimate should ever reach this upstream during yield.
	if n := r.routedCalls.Load(); n != 0 {
		t.Errorf("routed upstream received %d calls during yield, want 0 — the routed instance was not blocked by contention", n)
	}
	// defaultCalls is NOT asserted to be 0: ollama.Client.Unload (ADR-0003's
	// hard-yield VRAM-unload mechanism) legitimately calls this same upstream
	// with keep_alive=0 as part of yield-start itself — that is correct,
	// expected traffic, not a leaked inference admission. The two 503
	// responses above are what actually prove admission was blocked for both
	// instances; a raw call count on the default upstream can't distinguish
	// "an inference request got through" from "the unload call ran," since
	// both hit the same mock handler.
}

// TestAcceptance_OneRouteBothLanesReachCorrectBackend proves docs/
// per-model-backend-routing/steps.md's own Task 9 acceptance check outside
// of a contention scenario: with exactly one route configured, a request for
// the routed model reaches the routed upstream and a request for an
// unrouted model reaches the default upstream, on both the Interactive and
// the Batch lane — through the real rig (real queue, real Router, real
// backend.NewInstance-constructed openai backend), not a bare Router
// constructed directly by a unit test (see internal/backend/router_test.go
// for that coverage).
func TestAcceptance_OneRouteBothLanesReachCorrectBackend(t *testing.T) {
	r := newRoutedRig(t,
		map[string]any{"model": "unrouted-model", "response": "from-default", "done": true},
		sseDelta("from-routed"), sseStopWithUsage(1), sseDone)

	lanes := map[string]*httptest.Server{"interactive": r.interactive, "batch": r.batch}
	for name, srv := range lanes {
		t.Run(name+"/routed-model", func(t *testing.T) {
			r.requireReaches(t, routeCase{
				lane: name, url: srv.URL + "/api/chat", body: `{"model":"routed-model","messages":[]}`,
				wantText: "from-routed", reached: r.routedCalls, other: r.defaultCalls,
			})
		})
		t.Run(name+"/unrouted-model", func(t *testing.T) {
			r.requireReaches(t, routeCase{
				lane: name, url: srv.URL + "/api/generate", body: `{"model":"unrouted-model","prompt":"hi"}`,
				wantText: "from-default", reached: r.defaultCalls, other: r.routedCalls,
			})
		})
	}
}

// routeCase is one request and the upstream it must reach.
type routeCase struct {
	lane, url, body, wantText string
	reached, other            *atomic.Int32
}

// requireReaches posts the request and checks it succeeded with a response
// containing wantText, reached the expected upstream exactly once and never
// the other one (a request that leaked to the wrong backend).
func (r *routedRig) requireReaches(t *testing.T, c routeCase) {
	t.Helper()
	r.defaultCalls.Store(0)
	r.routedCalls.Store(0)
	status, got := postDrained(t, c.url, c.body)
	if status != http.StatusOK {
		t.Fatalf("%s on %s: status = %d, want 200, body=%s", c.wantText, c.lane, status, got)
	}
	if !strings.Contains(got, c.wantText) {
		t.Errorf("%s on %s: body = %s, want it to contain the upstream's response", c.wantText, c.lane, got)
	}
	if n := c.reached.Load(); n != 1 {
		t.Errorf("%s on %s: expected upstream got %d calls, want 1", c.wantText, c.lane, n)
	}
	if n := c.other.Load(); n != 0 {
		t.Errorf("%s on %s: other upstream got %d calls, want 0 — request leaked to the wrong backend", c.wantText, c.lane, n)
	}
}
