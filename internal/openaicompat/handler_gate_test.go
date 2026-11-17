package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/queue"
)

// alwaysServeAdm is a minimal queue.Admission that never yields — the happy
// path for TestGateOpenAIHandler_SuccessStreaming_HeadersAndTrailer, mirroring
// internal/queue's own unexported alwaysServe test type (which this package
// cannot reach directly since it lives in queue's _test.go files).
type alwaysServeAdm struct{}

func (alwaysServeAdm) Yielding() (bool, string)      { return false, "" }
func (alwaysServeAdm) ServeContext() context.Context { return context.Background() }

// alwaysYieldAdm is a minimal queue.Admission that always reports yielding,
// forcing queue.Gate's deferral path (Interactive class never parks, so this
// always resolves via deferRequest — FR-10) for
// TestGateOpenAIHandler_DeferredYield_Headers.
type alwaysYieldAdm struct{}

func (alwaysYieldAdm) Yielding() (bool, string)      { return true, "test forced yield" }
func (alwaysYieldAdm) ServeContext() context.Context { return context.Background() }

// gatedFront runs a real queue.Gate around the openai handler in front of
// upstream, exactly as cmd/broker/main.go composes it in production.
func gatedFront(t *testing.T, upstream *httptest.Server, adm queue.Admission) *httptest.Server {
	t.Helper()
	s := queue.New()
	cfg := queue.GateConfig{Class: queue.Interactive, Wait: 2 * time.Second, Admission: adm}
	return startServer(t, s.Gate(cfg, NewHandler(mustParseURL(t, upstream.URL), "")))
}

// TestGateOpenAIHandler_SuccessStreaming_HeadersAndTrailer is the FR-15/AC-10
// integration proof that a real queue.Gate, wired around the openai backend's
// NewHandler as `next` (exactly as cmd/broker/main.go composes it in
// production), still carries the X-Broker-Request-Id/Wait-Ms/Status contract
// correctly for a streamed request — both as headers (set optimistically
// before next runs, per gate.go) and, since this is a chunked NDJSON
// response, as the authoritative final-outcome trailer. Gate's trailer logic
// reads context/serve state, not what next wrote, so this proves the
// composition, not new behavior in either package (steps.md's "no new
// production code expected" premise for this task).
func TestGateOpenAIHandler_SuccessStreaming_HeadersAndTrailer(t *testing.T) {
	front := gatedFront(t, sseUpstream(t, sseDelta("Hi"), sseStop(1), sseDone), alwaysServeAdm{})

	resp := postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if reqID := resp.Header.Get("X-Broker-Request-Id"); reqID == "" {
		t.Fatal("missing X-Broker-Request-Id header")
	}
	waitMs := resp.Header.Get("X-Broker-Wait-Ms")
	if waitMs == "" {
		t.Fatal("missing X-Broker-Wait-Ms header")
	}
	if n, err := strconv.Atoi(waitMs); err != nil || n < 0 {
		t.Fatalf("X-Broker-Wait-Ms = %q, want a valid non-negative number", waitMs)
	}
	if got := resp.Header.Get("X-Broker-Status"); got != "served" {
		t.Fatalf("X-Broker-Status header = %q, want served", got)
	}

	// The trailer carries the authoritative final outcome and is only
	// populated on resp.Trailer once the chunked body has been fully read.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain body: %v", err)
	}
	if got := resp.Trailer.Get("X-Broker-Status"); got != "served" {
		t.Fatalf("X-Broker-Status trailer = %q, want served", got)
	}
}

// TestGateOpenAIHandler_DeferredYield_Headers is the deferred-path half of
// the FR-15/AC-10 integration proof: with an Admission that always reports
// yielding, queue.Gate defers the request (Interactive never parks, FR-10)
// before ever invoking the openai handler's next.ServeHTTP — proven by the
// upstream call counter staying at zero — and the response still carries
// X-Broker-Request-Id and X-Broker-Status: deferred, matching the same
// deferral shape internal/proxy's ollama-backed Gate composition uses (see
// internal/proxy/proxy_test.go's TestErrorHandlerDeadlineExceededMatches503Shape
// for the sibling contract on the other backend).
func TestGateOpenAIHandler_DeferredYield_Headers(t *testing.T) {
	upstream, calls := countingUpstream(t)
	front := gatedFront(t, upstream, alwaysYieldAdm{})

	resp := postChat(t, front, "/api/generate", `{"model":"test-model","prompt":"hi","stream":true}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if reqID := resp.Header.Get("X-Broker-Request-Id"); reqID == "" {
		t.Fatal("missing X-Broker-Request-Id header")
	}
	if got := resp.Header.Get("X-Broker-Status"); got != "deferred" {
		t.Fatalf("X-Broker-Status = %q, want deferred", got)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	if n := calls.Load(); n != 0 {
		t.Fatalf("upstream called %d times, want 0 (Gate must defer before reaching the openai handler)", n)
	}
}
