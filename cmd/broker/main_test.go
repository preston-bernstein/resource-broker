package main

// TestMainStartsWithoutPanic builds the real binary and runs it briefly under
// each UPSTREAM_BACKEND value, asserting clean startup (a "broker up" log
// line, no panic/crash). This exists because every other test in this repo
// exercises a package in isolation — nothing exercised main() itself, which
// is exactly how a nil-pointer panic on startup under the default backend
// slipped past `go build`, `go vet`, and a green `go test ./... -race` (see
// the composition-root gap found during this feature's integration
// validation pass). A package-level unit test can't easily drive main()
// itself (it blocks on ListenAndServe/signal handling), so this builds and
// runs the actual binary as a subprocess instead.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/backend"
	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// freePort asks the OS for an ephemeral port, then releases it immediately so
// the broker subprocess can bind it. Racy in the same way every "pick a free
// port for a subprocess" test helper is — acceptable for a single serialized
// test in this package.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func buildBrokerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ollama-broker-test")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runBrokerBriefly(t *testing.T, bin string, env []string) (stdout string, err error) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if startErr := cmd.Start(); startErr != nil {
		t.Fatalf("start broker: %v", startErr)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Poll for the startup log line rather than sleeping a fixed window —
	// under -race, startup is measurably slower due to instrumentation
	// overhead, and a fixed window flakes exactly the way the composition-
	// root gap this test exists to catch would: silently, only sometimes.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), `"msg":"broker up"`) || strings.Contains(buf.String(), "panic:") {
			break
		}
		select {
		case werr := <-done:
			return buf.String(), werr
		case <-time.After(20 * time.Millisecond):
		}
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	return buf.String(), nil
}

// syncBuffer is a concurrency-safe bytes.Buffer: cmd.Stdout/Stderr are
// written from the subprocess-reading goroutines exec.Cmd spawns, while the
// poll loop above reads it concurrently from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMainStartsWithoutPanic(t *testing.T) {
	bin := buildBrokerBinary(t)

	cases := []struct {
		name string
		env  []string
	}{
		{
			name: "default (UPSTREAM_BACKEND unset, ollama)",
			env: []string{
				"OLLAMA_URL=http://127.0.0.1:19999",
				"BROKER_CONTROL_ADDR=:0",
				"BROKER_INTERACTIVE_ADDR=:0",
				"BROKER_BATCH_ADDR=:0",
			},
		},
		{
			name: "explicit ollama",
			env: []string{
				"UPSTREAM_BACKEND=ollama",
				"OLLAMA_URL=http://127.0.0.1:19999",
				"BROKER_CONTROL_ADDR=:0",
				"BROKER_INTERACTIVE_ADDR=:0",
				"BROKER_BATCH_ADDR=:0",
			},
		},
		{
			name: "openai backend",
			env: []string{
				"UPSTREAM_BACKEND=openai",
				"UPSTREAM_URL=http://127.0.0.1:19999",
				"BROKER_CONTROL_ADDR=:0",
				"BROKER_INTERACTIVE_ADDR=:0",
				"BROKER_BATCH_ADDR=:0",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runBrokerBriefly(t, bin, tc.env)
			if strings.Contains(out, "panic:") {
				t.Fatalf("broker panicked on startup:\n%s", out)
			}
			if err != nil {
				t.Fatalf("broker exited unexpectedly: %v\noutput:\n%s", err, out)
			}
			if !strings.Contains(out, `"msg":"broker up"`) {
				t.Fatalf("broker did not log a clean startup:\n%s", out)
			}
		})
	}
}

// stubDetector never reports contention — buildBroker's ctrl.Run is never
// started by this test (no goroutine polls it), so all this needs to do is
// satisfy yield.Detector's interface.
type stubDetector struct{}

func (stubDetector) Detect() (reason string, contended bool) { return "", false }

// TestBuildBrokerWiresActivityTrackingIntoRouter is the regression test for
// the ordering bug this task fixes: backend.NewRouter/AddRoute must only
// ever capture an ALREADY idle-activity-decorated backend. If buildBroker's
// route branch ever regresses to building the Router before constructing
// ctrl and decorating routeBackends[i] with backend.WithActivityTracking,
// this test fails — the Router would have captured the undecorated route
// backend, so dispatching a request through it would never touch
// ctrl.TrackActivity's bookkeeping, and ctrl.IdleSummary() below would show
// no activity (or, if the ordering bug also caused a panic/mismatch
// upstream, the test would fail earlier).
//
// This calls buildBroker directly, as a unit test — no subprocess, no real
// network I/O. backend.NewInstance/buildRoutes only build HTTP clients
// pointed at a URL; they never dial out at construction time, only when a
// handler actually proxies a request (which this test does, but against a
// URL with nothing listening — that's fine, TrackActivity's bookkeeping
// runs regardless of what the wrapped handler ultimately does or returns).
func TestBuildBrokerWiresActivityTrackingIntoRouter(t *testing.T) {
	defaultURL, err := url.Parse("http://127.0.0.1:19999")
	if err != nil {
		t.Fatalf("parse default url: %v", err)
	}
	routeURL, err := url.Parse("http://127.0.0.1:19998")
	if err != nil {
		t.Fatalf("parse route url: %v", err)
	}

	cfg := &config.Config{
		DetectInterval:      time.Minute,
		YieldConfirmPolls:   1,
		UpstreamIdleTimeout: 0, // default backend has no unit name below, so this must stay 0
		Routes: []config.RouteBackend{
			{
				Models:      []string{"test-model"},
				Backend:     "openai",
				URL:         routeURL,
				UnitName:    "test-vllm.service",
				IdleTimeout: 5 * time.Minute,
			},
		},
	}

	be, err := backend.NewInstance("openai", defaultURL, "", "")
	if err != nil {
		t.Fatalf("build default backend: %v", err)
	}

	router, activeBackend, ctrl, idleStatus, err := buildBroker(cfg, stubDetector{}, be)
	if err != nil {
		t.Fatalf("buildBroker: %v", err)
	}
	if router == nil {
		t.Fatal("buildBroker: router is nil, want a configured Router")
	}
	if activeBackend == nil {
		t.Fatal("buildBroker: activeBackend is nil")
	}
	if ctrl == nil {
		t.Fatal("buildBroker: ctrl is nil")
	}
	if idleStatus == nil {
		t.Fatal("buildBroker: idleStatus is nil, want ctrl.IdleSummary (a route has a nonzero IdleTimeout)")
	}

	// Dispatch a request through the REAL Router returned by buildBroker,
	// for the model routed to the idle-tracked route backend. This is the
	// part that proves the wiring, not just the construction: if the Router
	// captured the undecorated backend (the ordering bug), this request
	// would never reach ctrl.TrackActivity's bookkeeping.
	handler := router.ProxyForLane("interactive")
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(`{"model":"test-model"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	summaryAny := idleStatus()
	summary, ok := summaryAny.([]yield.IdleSummaryEntry)
	if !ok {
		t.Fatalf("idleStatus(): got %T, want []yield.IdleSummaryEntry", summaryAny)
	}
	if len(summary) != 1 {
		t.Fatalf("idleStatus(): got %d entries, want 1 (only the route has IdleTimeout configured): %+v", len(summary), summary)
	}
	entry := summary[0]
	if entry.Label != routeURL.String() {
		t.Fatalf("idleStatus(): entry label = %q, want %q", entry.Label, routeURL.String())
	}
	since, err := time.ParseDuration(entry.SinceLastDispatch)
	if err != nil {
		t.Fatalf("parse since_last_dispatch %q: %v", entry.SinceLastDispatch, err)
	}
	if since > 5*time.Second {
		t.Fatalf("since_last_dispatch = %s, want near-zero — the dispatched request through router.ProxyForLane should have updated the route's activity tracking; a stale/large value means the Router captured an undecorated backend (the ordering bug this test guards against)", since)
	}
}

// TestMainInteractiveProxyTracksActivityOnNoRoutesPath is the regression test
// for a second occurrence of the same class of bug
// TestBuildBrokerWiresActivityTrackingIntoRouter guards against — this time
// in main() itself, on the no-routes path, discovered only by actually
// running the real binary and driving a real HTTP request through it (a live
// verification pass), not by any unit test or two prior code-review passes.
//
// buildBroker's no-routes branch correctly decorates its own be parameter
// with backend.WithActivityTracking and returns the decorated value as
// activeBackend — that part is unit-tested and correct. The bug lived one
// level up, in main() itself: main() constructs be via backend.New(cfg)
// *before* calling buildBroker(cfg, detector, be), then used that same
// pre-buildBroker be (not the decorated activeBackend buildBroker returned)
// to build interactiveProxy/batchProxy via be.Proxy(). Since be is a plain Go
// interface value passed to buildBroker by copy, buildBroker reassigning its
// own be parameter internally never changes main()'s be — only activeBackend
// carries the decoration back out. The result: every real interactive/batch
// request silently bypassed ctrl.TrackActivity's bookkeeping on the no-routes
// path, so an idle-unloaded default backend would never show its idle timer
// reset by real traffic and would never wake back up on demand — while
// go test ./... and two parallel-review passes stayed green, because nothing
// exercised main()'s actual interactiveProxy/batchProxy construction against
// a live request.
//
// This test builds and runs the real binary (no routes configured — the
// buggy path), against a real in-process fake OpenAI-compatible upstream,
// with idle-unload enabled and a very short timeout so a real idle-unload
// fires before the test's own request does. It then sends one real HTTP
// request through the broker's actual interactive listener and asserts, via
// a real GET to /status, that activity tracking moved — this is exactly the
// live check that caught the bug, now automated.
func TestMainInteractiveProxyTracksActivityOnNoRoutesPath(t *testing.T) {
	bin := buildBrokerBinary(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"fake","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()

	interactivePort := freePort(t)
	batchPort := freePort(t)
	controlPort := freePort(t)

	env := []string{
		"UPSTREAM_BACKEND=openai",
		"UPSTREAM_URL=" + upstream.URL,
		"UPSTREAM_UNIT_NAME=main-test-fake-unit.service",
		"UPSTREAM_IDLE_TIMEOUT=200ms",
		"BROKER_DETECT_INTERVAL=50ms",
		fmt.Sprintf("BROKER_INTERACTIVE_ADDR=127.0.0.1:%d", interactivePort),
		fmt.Sprintf("BROKER_BATCH_ADDR=127.0.0.1:%d", batchPort),
		fmt.Sprintf("BROKER_CONTROL_ADDR=127.0.0.1:%d", controlPort),
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	// Wait for real startup.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), `"msg":"broker up"`) {
		if strings.Contains(buf.String(), "panic:") {
			t.Fatalf("broker panicked on startup:\n%s", buf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), `"msg":"broker up"`) {
		t.Fatalf("broker did not log a clean startup within deadline:\n%s", buf.String())
	}

	statusURL := fmt.Sprintf("http://127.0.0.1:%d/status", controlPort)
	fetchStatus := func() map[string]any {
		t.Helper()
		resp, err := http.Get(statusURL)
		if err != nil {
			t.Fatalf("GET /status: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode /status: %v", err)
		}
		return out
	}

	// Wait for a real idle-unload to fire (200ms configured timeout, 50ms
	// detect interval — should fire well within 3s). systemctl doesn't exist
	// on every CI/dev host, so the underlying Unload call may itself fail —
	// that's fine and expected (WARN-logged, not fatal, per ADR-0014's
	// documented failure mode); what matters here is idle_unloaded flips to
	// true, proving checkIdleLocked genuinely fired.
	idleDeadline := time.Now().Add(3 * time.Second)
	var sawIdleUnloaded bool
	for time.Now().Before(idleDeadline) {
		st := fetchStatus()
		idle, _ := st["idle"].([]any)
		if len(idle) == 1 {
			entry, _ := idle[0].(map[string]any)
			if b, _ := entry["idle_unloaded"].(bool); b {
				sawIdleUnloaded = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawIdleUnloaded {
		t.Fatalf("instance never reported idle_unloaded=true within 3s; last /status:\n%+v\nbroker output:\n%s", fetchStatus(), buf.String())
	}

	// Widen the gap deliberately: wait well past the 200ms idle timeout
	// before dispatching the test's own request, so "since_last_dispatch
	// never reset" (the bug) and "since_last_dispatch reset to ~0 just now"
	// (correct) are unambiguous by a wide margin — a loose comparison here
	// (e.g. "< 2s") can pass in BOTH the buggy and fixed cases if the whole
	// test runs faster than the threshold, which is exactly the false-
	// negative shape this comment exists to rule out (caught in this
	// feature's own verify pass: an earlier draft of this test used a loose
	// 2s bound and passed unchanged with the bug reintroduced).
	time.Sleep(1200 * time.Millisecond)

	// The regression check: dispatch one real request through the broker's
	// actual interactive listener (main()'s real interactiveProxy, not a
	// handler built directly by a unit test), then confirm /status shows
	// activity tracking actually moved. Before the fix, this next request
	// would never touch ctrl.TrackActivity — since_last_dispatch would keep
	// climbing from the original idle-unload time (now >1.4s ago) instead of
	// resetting to near-zero.
	reqURL := fmt.Sprintf("http://127.0.0.1:%d/api/generate", interactivePort)
	resp, err := http.Post(reqURL, "application/json", strings.NewReader(`{"model":"fake-model","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("POST /api/generate: %v", err)
	}
	resp.Body.Close()

	st := fetchStatus()
	idle, _ := st["idle"].([]any)
	if len(idle) != 1 {
		t.Fatalf("/status \"idle\" section: got %d entries, want 1: %+v", len(idle), st)
	}
	entry, _ := idle[0].(map[string]any)
	sinceStr, _ := entry["since_last_dispatch"].(string)
	since, err := time.ParseDuration(sinceStr)
	if err != nil {
		t.Fatalf("parse since_last_dispatch %q: %v", sinceStr, err)
	}
	// Tight bound: a correctly-tracked request resets since_last_dispatch to
	// a few milliseconds. The pre-request sleep above guarantees an untracked
	// request would show >=1.2s instead — 500ms leaves comfortable margin on
	// both sides without being loose enough to mask the bug.
	if since > 500*time.Millisecond {
		t.Fatalf("since_last_dispatch = %s after a real request through the interactive listener (following a deliberate "+
			"1.2s pre-request wait), want near-zero — this means interactiveProxy is not the activity-tracked backend "+
			"(the bug: main() used the pre-buildBroker be instead of the returned activeBackend). Full /status: %+v\nbroker output:\n%s",
			since, st, buf.String())
	}
}
