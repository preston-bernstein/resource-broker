// Package broker_test is a top-level, end-to-end acceptance suite. Unlike
// the unit/integration tests scattered through internal/*, every test here
// stands up the REAL pieces wired together the way cmd/broker/main.go wires
// them — a real backend.Backend, a real queue.Scheduler (both Gates), a real
// yield.Controller, a real durable job.Service+Worker backed by an in-memory
// SQLite store, and the real admin.Mux control plane — fronting a real
// in-process mock upstream (Ollama-shaped or OpenAI-compatible-shaped,
// depending on the test). It proves the acceptance criteria in
// docs/openai-compatible-upstream-backend/requirements.md hold when
// everything is assembled as it would be in production, not just that each
// piece works in isolation (that is what the rest of the repo's unit/
// integration tests already cover).
//
// This file covers all 24 acceptance criteria (AC-1 through AC-24), built up
// across four passes: AC-1..AC-6 (part 1), AC-7..AC-9 (part 2), AC-10..AC-12
// (part 3), and AC-13..AC-24 (part 4, final) below.
package broker_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/admin"
	"github.com/preston-bernstein/resource-broker/internal/backend"
	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/detect"
	"github.com/preston-bernstein/resource-broker/internal/job"
	"github.com/preston-bernstein/resource-broker/internal/metrics"
	"github.com/preston-bernstein/resource-broker/internal/openaicompat"
	"github.com/preston-bernstein/resource-broker/internal/proxy"
	"github.com/preston-bernstein/resource-broker/internal/queue"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// --- rig: a full in-process Broker, mirroring cmd/broker/main.go's wiring ---

// rig is a full in-process Broker instance a test can drive directly: real
// backend, real scheduler/gates, real yield controller, real durable job
// service+worker, real admin control plane — everything main.go assembles,
// minus main()'s signal handling and os.Exit calls. interactive/batch/control
// are httptest.Servers fronting the same Gate/Mux handlers main.go binds to
// BROKER_INTERACTIVE_ADDR/BROKER_BATCH_ADDR/BROKER_CONTROL_ADDR.
type rig struct {
	cfg         *config.Config
	be          backend.Backend
	sched       *queue.Scheduler
	ctrl        *yield.Controller
	jobSvc      *job.Service
	interactive *httptest.Server
	batch       *httptest.Server
	control     *httptest.Server
	// embed is the optional image-embedding lane fronting cfg.InfinityURL,
	// wired only when cfg.InfinityURL is non-nil (see newRig) — mirroring
	// cmd/broker/main.go's own "if cfg.InfinityURL != nil" gate. nil when the
	// lane is disabled, exactly like main.go never starting that server.
	embed *httptest.Server
	// router is non-nil only when cfg.Routes is non-empty, mirroring
	// cmd/broker/main.go's own router-construction gate (docs/adr/
	// 0015-per-model-backend-routing.md) — nil is the pre-feature, zero-route
	// state where be alone fronts both Gates exactly as before this feature.
	router *backend.Router
}

// newRig builds a rig from cfg, exactly the way cmd/broker/main.go's main()
// builds the Broker, and registers cleanup. It deliberately does NOT start
// ctrl.Run(ctx) (the contention-detection poll loop): AC-1 through AC-6 (this
// file) don't exercise gaming/Plex preemption — that's AC-7/AC-8, a later
// task's file — and a Controller that never runs its poll loop simply stays
// in its zero-value ModeAuto/not-yielding state, which is exactly what every
// test here needs. The detector itself is still wired with a real (but
// no-op) process Lister so cfg-driven fields (DetectInterval,
// YieldConfirmPolls) flow through unchanged from what main.go would build.
func newRig(t *testing.T, cfg *config.Config) *rig {
	t.Helper()

	be, err := backend.New(cfg)
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}

	sched := queue.New()
	sched.SetMaxWaiters(cfg.MaxWaiters)
	sched.SetMaxInflight(cfg.MaxInflight)
	sched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)

	detector := detect.New(func() ([]detect.Process, error) { return nil, nil })
	reg := metrics.New()
	detector.SetErrorRecorder(reg)

	// Mirrors cmd/broker/main.go's own router-construction gate exactly
	// (docs/adr/0015-per-model-backend-routing.md): when cfg.Routes is empty
	// (every AC-1..AC-24 test predating per-model routing), router stays nil
	// and activeBackend is be — byte-for-byte the pre-feature rig. Only tests
	// that explicitly set cfg.Routes exercise the routed path, keeping this
	// rig representative of real production wiring either way — the whole
	// reason this file exists (see the package doc comment above).
	var router *backend.Router
	var activeBackend backend.Backend = be
	var ctrl *yield.Controller
	if len(cfg.Routes) == 0 {
		ctrl = yield.NewWithConfirm(detector, be.Unloader(), cfg.DetectInterval, cfg.YieldConfirmPolls)
	} else {
		routeBackends := make([]backend.Backend, len(cfg.Routes))
		unloaders := make([]yield.Unloader, len(cfg.Routes)+1)
		labels := make([]string, len(cfg.Routes)+1)
		unloaders[0] = be.Unloader()
		labels[0] = "default"
		for i, rb := range cfg.Routes {
			rbBackend, err := backend.NewInstance(rb.Backend, rb.URL, rb.APIKey, rb.UnitName)
			if err != nil {
				t.Fatalf("backend.NewInstance route %d: %v", i, err)
			}
			routeBackends[i] = rbBackend
			unloaders[i+1] = rbBackend.Unloader()
			labels[i+1] = rb.URL.String()
		}
		r := backend.NewRouter(be)
		for i, rb := range cfg.Routes {
			for _, model := range rb.Models {
				r.AddRoute(model, rb.Lane, routeBackends[i])
			}
		}
		router = r
		activeBackend = router
		ctrl = yield.NewWithConfirmMulti(detector, unloaders, labels, cfg.DetectInterval, cfg.YieldConfirmPolls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sched.SetShutdownContext(ctx)
	yieldingFn := func() bool {
		y, _ := ctrl.Yielding()
		return y
	}
	go sched.RunParkDrain(ctx, yieldingFn)

	store, err := job.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("open job store: %v", err)
	}
	jobSvc := job.NewService(store, cfg.JobMaxAttempts)
	jobSvc.SetRecorder(reg)
	if err := jobSvc.Recover(ctx); err != nil {
		t.Fatalf("job recover: %v", err)
	}
	worker := job.NewWorker(jobSvc, sched, ctrl, activeBackend, cfg.BatchQuantum, 0)
	go worker.Run(ctx)

	jobCounts := func() job.Counts {
		c, _ := jobSvc.Counts(context.Background())
		return c
	}
	metricsHandler := reg.Handler(func() metrics.Gauges {
		st := sched.Stats()
		yielding, _ := ctrl.Yielding()
		c := jobCounts()
		return metrics.Gauges{
			Yielding:     yielding,
			Busy:         st.Busy,
			Inflight:     st.Inflight,
			MaxInflight:  st.MaxInflight,
			Interactive:  st.Interactive,
			Batch:        st.Batch,
			Parked:       st.Parked,
			JobQueued:    c.Queued,
			JobRunning:   c.Running,
			JobSucceeded: c.Succeeded,
			JobFailed:    c.Failed,
			JobCanceled:  c.Canceled,
		}
	})
	// Mirrors cmd/broker/main.go's healthCheck upstream check exactly (down to
	// the "upstream unreachable: %w" wrapping): AC-9 requires /healthz's 503
	// body to name "upstream" as the failed dependency, and a bare
	// be.Reachable(ctx) error (e.g. a raw "connection refused") doesn't
	// guarantee that word appears. main.go's job-store and detector-staleness
	// checks are deliberately omitted here — see this function's doc comment on
	// why this rig doesn't need them wired for AC-1..AC-9's scope.
	healthCheck := func(ctx context.Context) error {
		if err := activeBackend.Reachable(ctx); err != nil {
			return fmt.Errorf("upstream unreachable: %w", err)
		}
		return nil
	}

	var interactiveProxy, batchProxy http.Handler
	if router != nil {
		interactiveProxy = router.ProxyForLane(queue.Interactive.String())
		batchProxy = router.ProxyForLane(queue.Batch.String())
	} else {
		interactiveProxy = be.Proxy()
		batchProxy = be.Proxy()
	}
	var routingStatus func() any
	if router != nil {
		routingStatus = router.RoutingSummary
	}

	interactive := httptest.NewServer(sched.Gate(queue.Interactive, cfg.InteractiveWait, 0, ctrl, reg, interactiveProxy))
	batchSrv := httptest.NewServer(sched.Gate(queue.Batch, cfg.BatchWait, 0, ctrl, reg, batchProxy))
	control := httptest.NewServer(admin.Mux(ctrl, sched, healthCheck, metricsHandler, jobSvc.Routes(), func() any { return jobCounts() }, nil, routingStatus, nil, cfg.ControlToken))

	// Optional image-embedding lane, wired only when cfg.InfinityURL is set
	// (AC-12): mirrors cmd/broker/main.go's own "if cfg.InfinityURL != nil"
	// block — its own scheduler (never sched above), NewEmbed fronting
	// InfinityURL directly, completely bypassing backend.New()/be.Proxy() —
	// proving the embed lane is unaffected by cfg.UpstreamBackend.
	var embedSrv *httptest.Server
	if cfg.InfinityURL != nil {
		embedSched := queue.New()
		embedSched.SetMaxWaiters(cfg.MaxWaiters)
		embedSched.SetMaxInflight(1)
		embedSched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
		embedSched.SetShutdownContext(ctx)
		go embedSched.RunParkDrain(ctx, yieldingFn)
		embedUpstream := proxy.NewEmbed(cfg.InfinityURL)
		embedSrv = httptest.NewServer(embedSched.Gate(queue.Batch, cfg.BatchWait, cfg.EmbedTimeout, ctrl, reg, embedUpstream))
	}

	t.Cleanup(func() {
		cancel()
		interactive.Close()
		batchSrv.Close()
		control.Close()
		if embedSrv != nil {
			embedSrv.Close()
		}
		store.Close()
	})

	return &rig{
		cfg: cfg, be: be, sched: sched, ctrl: ctrl, jobSvc: jobSvc,
		interactive: interactive, batch: batchSrv, control: control, embed: embedSrv,
		router: router,
	}
}

// baseCfg fills in the scheduling/queue fields every rig needs regardless of
// backend, using the same defaults config.Load() would (see
// internal/config/config.go), so a rig built directly from a struct literal
// (bypassing env-var driven Load(), same pattern internal/job/worker_test.go's
// openaiBlockGen uses) still behaves like a production Broker.
func baseCfg() config.Config {
	return config.Config{
		InteractiveWait:   5 * time.Second,
		BatchWait:         5 * time.Second,
		MaxWaiters:        64,
		MaxInflight:       1,
		BatchQuantum:      10 * time.Second,
		JobMaxAttempts:    3,
		DetectInterval:    time.Second,
		YieldConfirmPolls: 1,
	}
}

func ollamaCfg(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse ollama upstream url: %v", err)
	}
	cfg := baseCfg()
	cfg.UpstreamBackend = "ollama"
	cfg.OllamaURL = u
	return &cfg
}

func openaiCfg(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse openai upstream url: %v", err)
	}
	cfg := baseCfg()
	cfg.UpstreamBackend = "openai"
	cfg.UpstreamURL = u
	return &cfg
}

// readLineWithin reads one line with a deadline, mirroring
// internal/openaicompat/handler_test.go's helper of the same name: a
// buffered (non-incrementally-flushed) implementation would hang past the
// deadline instead of returning quickly, turning a would-be-silent bug into
// a clear test failure.
func readLineWithin(br *bufio.Reader, d time.Duration) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := br.ReadString('\n')
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(d):
		return "", fmt.Errorf("timed out after %s waiting for a line", d)
	}
}

// getJobStatus fetches a Job's canonical status through the real /jobs/{id}
// HTTP surface (never by reaching into the Service directly), matching how a
// real consumer observes Job progress.
func getJobStatus(t *testing.T, controlURL, jobID string) job.Status {
	t.Helper()
	resp, err := http.Get(controlURL + "/jobs/" + jobID)
	if err != nil {
		t.Fatalf("GET /jobs/%s: %v", jobID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%s: status %d", jobID, resp.StatusCode)
	}
	var st job.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode job status: %v", err)
	}
	return st
}

// --- AC-1: default (UPSTREAM_BACKEND unset) behavior is unchanged ---

// TestAcceptance_AC1_OllamaBackendSanity is a light sanity check that this
// acceptance harness itself is wired correctly for the default "ollama"
// backend — it does NOT re-run the rest of the repo's existing test suite
// (that's `go test ./...`, run separately). It proves a Synchronous request
// through the full stack (interactive Gate -> ollama backend's real
// httputil.ReverseProxy -> a mock Ollama upstream) still passes through
// unchanged, and carries the broker's own response headers.
func TestAcceptance_AC1_OllamaBackendSanity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "m", "response": "hi", "done": true})
	}))
	defer upstream.Close()

	r := newRig(t, ollamaCfg(t, upstream.URL))

	resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"m","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Broker-Request-Id"); got == "" {
		t.Error("X-Broker-Request-Id header missing: request did not go through the Gate")
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["response"] != "hi" {
		t.Errorf("response = %v, want pass-through %q", body["response"], "hi")
	}
}

// --- AC-2/AC-3: config.Load() validation ---

func TestAcceptance_AC2_InvalidUpstreamBackendRejected(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "bogus")

	cfg, err := config.Load()
	if err == nil {
		t.Fatal("config.Load() err = nil, want a non-nil error naming the invalid value")
	}
	if cfg != nil {
		t.Errorf("config.Load() cfg = %+v, want nil on error", cfg)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name the invalid value %q", err.Error(), "bogus")
	}
}

func TestAcceptance_AC3_OpenAIBackendRequiresUpstreamURL(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "openai")
	t.Setenv("UPSTREAM_URL", "")

	cfg, err := config.Load()
	if err == nil {
		t.Fatal("config.Load() err = nil, want a non-nil error: UPSTREAM_BACKEND=openai with no UPSTREAM_URL")
	}
	if cfg != nil {
		t.Errorf("config.Load() cfg = %+v, want nil on error", cfg)
	}
}

// --- AC-4: streaming chat/generate end to end ---

// TestAcceptance_AC4_StreamingChatEndToEnd proves a Synchronous stream:true
// request through the full stack (interactive Gate -> openai backend's real
// translating handler -> a mock OpenAI-compatible upstream) is relayed to the
// client as incrementally flushed Ollama-shaped NDJSON, not buffered until
// the upstream finishes. The mock upstream withholds its second chunk until
// this test has actually read the first NDJSON line off the live HTTP
// response — if the full stack buffered anywhere along the way, reading line
// 1 would hang and readLineWithin's deadline would fail the test.
func TestAcceptance_AC4_StreamingChatEndToEnd(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		fl.Flush()
		<-release // withhold the final chunk until the client has read line 1
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", got)
	}
	if got := resp.Header.Get("X-Broker-Request-Id"); got == "" {
		t.Error("X-Broker-Request-Id header missing: request did not go through the Gate")
	}

	br := bufio.NewReader(resp.Body)
	line1, err := readLineWithin(br, 3*time.Second)
	if err != nil {
		t.Fatalf("reading NDJSON line 1 (full stack likely buffered): %v", err)
	}
	var chunk1 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line1)), &chunk1); err != nil {
		t.Fatalf("line1 %q is not valid JSON: %v", line1, err)
	}
	if done, _ := chunk1["done"].(bool); done {
		t.Fatalf("line1 done = true, want false (an intermediate flush)")
	}
	// line1 carries the actual delta text as it arrives (matching real
	// Ollama's own progressive-streaming convention) rather than an empty
	// placeholder — a client rendering tokens live sees "Hello" as soon as
	// this line flushes.
	if chunk1["response"] != "Hello" {
		t.Errorf("line1 response = %v, want delta text %q relayed live", chunk1["response"], "Hello")
	}

	close(release) // now allow the final chunk

	line2, err := readLineWithin(br, 3*time.Second)
	if err != nil {
		t.Fatalf("reading NDJSON line 2: %v", err)
	}
	var chunk2 map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line2)), &chunk2); err != nil {
		t.Fatalf("line2 %q is not valid JSON: %v", line2, err)
	}
	if done, _ := chunk2["done"].(bool); !done {
		t.Fatalf("line2 done = %v, want true (the final line)", chunk2["done"])
	}
	// line2 (the final, done:true line) carries an empty response: "Hello"
	// was already relayed on line1, so repeating it here would double the
	// text for any client that concatenates every line's "response" field.
	if chunk2["response"] != "" {
		t.Errorf("line2 response = %v, want empty (full text already sent on line1)", chunk2["response"])
	}
}

// --- AC-5: embeddings end to end ---

// TestAcceptance_AC5_EmbedEndToEnd proves a Synchronous /api/embed request
// through the full stack (batch Gate -> openai backend's real translating
// handler -> a mock OpenAI-compatible /v1/embeddings upstream) produces an
// Ollama-shaped embeddings response.
func TestAcceptance_AC5_EmbedEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model": "acceptance-model",
			"data": []map[string]any{
				{"embedding": []float64{0.1, 0.2}, "index": 0},
				{"embedding": []float64{0.3, 0.4}, "index": 1},
			},
		})
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	resp, err := http.Post(r.batch.URL+"/api/embed", "application/json", strings.NewReader(`{"model":"acceptance-model","input":["a","b"]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out openaicompat.EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode embed response: %v", err)
	}
	if len(out.Embeddings) != 2 {
		t.Fatalf("len(embeddings) = %d, want 2", len(out.Embeddings))
	}
	if len(out.Embeddings[0]) != 2 || out.Embeddings[0][0] != 0.1 || out.Embeddings[0][1] != 0.2 {
		t.Errorf("embeddings[0] = %v, want [0.1 0.2]", out.Embeddings[0])
	}
	if len(out.Embeddings[1]) != 2 || out.Embeddings[1][0] != 0.3 || out.Embeddings[1][1] != 0.4 {
		t.Errorf("embeddings[1] = %v, want [0.3 0.4]", out.Embeddings[1])
	}
}

// --- AC-6: durable Job lifecycle end to end ---

// TestAcceptance_AC6_JobLifecycleEndToEnd proves a Job submitted via POST
// /jobs through the full stack (durable Service+Worker -> openai backend's
// real Client.Generate -> a mock OpenAI-compatible upstream) runs to
// SUCCEEDED, and that GET /jobs/<id> shows an increasing token count while
// RUNNING. The mock upstream paces its chunks so this test's poll loop has
// time to observe more than one distinct in-progress token count.
func TestAcceptance_AC6_JobLifecycleEndToEnd(t *testing.T) {
	const chunks = 6
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(60 * time.Millisecond)
		}
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	body := `{"model":"acceptance-model","prompt":"hi"}`
	req, err := http.NewRequest(http.MethodPost, r.control.URL+"/jobs", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build submit request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "acceptance-ac6")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	var sub struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
	}
	if sub.JobID == "" {
		t.Fatal("submit response carried no job_id")
	}

	deadline := time.Now().Add(10 * time.Second)
	maxTokensSeen := 0
	sawIncrease := false
	finalState := job.State("")
	for time.Now().Before(deadline) {
		st := getJobStatus(t, r.control.URL, sub.JobID)
		if st.Progress != nil && st.Progress.Tokens > maxTokensSeen {
			if maxTokensSeen > 0 {
				sawIncrease = true
			}
			maxTokensSeen = st.Progress.Tokens
		}
		if st.State.Terminal() {
			finalState = st.State
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if finalState != job.StateSucceeded {
		t.Fatalf("job final state = %q, want %q (last max tokens seen: %d)", finalState, job.StateSucceeded, maxTokensSeen)
	}
	if !sawIncrease {
		t.Errorf("token count progress never increased across polls (max seen: %d) — GET /jobs/<id> should show growth during execution", maxTokensSeen)
	}
	if maxTokensSeen == 0 {
		t.Error("token count progress never observed above 0")
	}
}

// waitForJobState polls GET /jobs/<id> through the real HTTP surface until the
// Job reaches want or the deadline expires, returning the last-observed
// status either way (mirroring getJobStatus's "always hit the real endpoint"
// discipline, used by both AC-7 and AC-8 below).
func waitForJobState(t *testing.T, controlURL, jobID string, want job.State) job.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var st job.Status
	for time.Now().Before(deadline) {
		st = getJobStatus(t, controlURL, jobID)
		if st.State == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach state %s (last=%+v)", jobID, want, st)
	return st
}

// submitJobViaHTTP submits a Job through the real POST /jobs surface, exactly
// as AC-6 does, returning its ID.
func submitJobViaHTTP(t *testing.T, controlURL, idempotencyKey string) string {
	t.Helper()
	body := `{"model":"acceptance-model","prompt":"hi"}`
	req, err := http.NewRequest(http.MethodPost, controlURL+"/jobs", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build submit request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	defer resp.Body.Close()
	var sub struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
	}
	if sub.JobID == "" {
		t.Fatal("submit response carried no job_id")
	}
	return sub.JobID
}

// --- AC-7: gaming/Plex Yield preempts an in-flight Job end to end ---

// TestAcceptance_AC7_YieldPreemptsJobEndToEnd proves that forcing Yield
// through the real control plane (POST /control {"mode":"yield"} — the same
// HTTP surface an operator or a future gaming-detector integration uses,
// exercising *yield.Controller's real SetMode/applyLocked transition rather
// than a fake) while a durable openai-backend Job is RUNNING cancels that
// Job's in-flight upstream call and requeues it to the front of the Queue:
// Position 1, State QUEUED. internal/job/worker_test.go's
// TestWorkerGamingPreempt already pins this at the Worker-unit level (with a
// fakeYield); this proves the same outcome through the full wired stack (real
// HTTP control endpoint -> real Controller -> real Worker -> real openai
// backend/client -> mock upstream).
func TestAcceptance_AC7_YieldPreemptsJobEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		fl.Flush()
		<-r.Context().Done() // held open until Yield cancels the Job's upstream call
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac7")
	waitForJobState(t, r.control.URL, jobID, job.StateRunning)

	ctrlResp, err := http.Post(r.control.URL+"/control", "application/json", strings.NewReader(`{"mode":"yield"}`))
	if err != nil {
		t.Fatalf("POST /control: %v", err)
	}
	ctrlResp.Body.Close()
	if ctrlResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /control status = %d, want 200", ctrlResp.StatusCode)
	}

	st := waitForJobState(t, r.control.URL, jobID, job.StateQueued)
	if st.Position != 1 {
		t.Fatalf("position after gaming preempt = %d, want 1 (front of queue)", st.Position)
	}
	if st.Attempts != 0 {
		t.Fatalf("gaming preempt burned an attempt: %d", st.Attempts)
	}
}

// --- AC-8: interactive request preempts a batch Job past BROKER_BATCH_QUANTUM ---

// TestAcceptance_AC8_InteractivePreemptsBatchJobPastQuantumEndToEnd proves
// that a real interactive HTTP request, parked behind a running batch Job on
// a MaxInflight=1 gate, does NOT preempt the Job before a short test-friendly
// BROKER_BATCH_QUANTUM has elapsed, and DOES preempt it once the quantum has
// elapsed — through the full wired stack (real interactive Gate -> real
// Worker's monitor/shouldPreempt -> real openai backend -> mock upstream),
// complementing internal/job/worker_test.go's
// TestWorkerInteractivePreemptPastQuantum (Worker-unit level, direct
// sched.Acquire) with the real end-to-end HTTP path.
func TestAcceptance_AC8_InteractivePreemptsBatchJobPastQuantumEndToEnd(t *testing.T) {
	const jobModel = "batch-job-model"
	var jobCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var reqBody struct {
			Model string `json:"model"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &reqBody)

		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		fl.Flush()
		// Only the batch Job's FIRST connection blocks (long enough for the
		// interactive request to trigger quantum-based preemption). Once
		// preempted, the Worker immediately re-claims the freed slot and
		// re-runs the same Job (nothing else is queued) — its second
		// connection must complete normally so the Job converges to a
		// deterministic terminal state instead of blocking forever on a
		// connection nothing will ever cancel again, which would otherwise
		// hang this test's `defer upstream.Close()` at cleanup.
		if reqBody.Model == jobModel && jobCalls.Add(1) == 1 {
			<-r.Context().Done() // held open until quantum-based preemption cancels it
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	cfg := openaiCfg(t, upstream.URL)
	quantum := 400 * time.Millisecond // test-friendly, short BROKER_BATCH_QUANTUM
	cfg.BatchQuantum = quantum
	r := newRig(t, cfg)

	body := fmt.Sprintf(`{"model":%q,"prompt":"hi"}`, jobModel)
	req, err := http.NewRequest(http.MethodPost, r.control.URL+"/jobs", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build submit request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "acceptance-ac8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	var sub struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sub); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
	}

	waitForJobState(t, r.control.URL, sub.JobID, job.StateRunning)
	start := time.Now()

	// A real interactive request, parked on the busy MaxInflight=1 gate behind
	// the running batch Job, drives w.gate.Stats().Interactive > 0 the same
	// way a live Consumer request would.
	interactiveDone := make(chan struct{})
	go func() {
		defer close(interactiveDone)
		iresp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"interactive-model","prompt":"hi","stream":true}`))
		if err != nil {
			t.Error("interactive request: " + err.Error())
			return
		}
		io.Copy(io.Discard, iresp.Body)
		iresp.Body.Close()
	}()

	// Must NOT preempt before the quantum elapses (AC-8's "not before").
	// Checked at a third of the quantum, well short of both the quantum
	// itself and the Worker's 200ms re-check ticker.
	select {
	case <-interactiveDone:
		t.Fatalf("interactive request completed after only %v, before quantum %v elapsed (preempted too early)", time.Since(start), quantum)
	case <-time.After(quantum / 3):
	}

	// Past the quantum the Worker must preempt so the interactive request's
	// slot frees up and it completes.
	select {
	case <-interactiveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("interactive request never completed: batch Job was never preempted")
	}
	const tolerance = 150 * time.Millisecond // absorbs the 200ms monitor ticker + HTTP round trips
	if elapsed := time.Since(start); elapsed < quantum-tolerance {
		t.Fatalf("interactive request completed after %v, want >= ~quantum %v (preempted before its trigger time)", elapsed, quantum)
	}

	// The batch Job was requeued (resume-first) rather than failed: the
	// Worker immediately re-claims the now-free slot and re-runs it to
	// SUCCEEDED (its second connection, per the mock above) with no attempt
	// burned by the preempt itself (AC-8). Waiting for the terminal state,
	// rather than trying to catch the transient QUEUED window between the
	// preempt and the automatic re-run, is what makes this assertion
	// deterministic instead of racing the Worker's own re-acquire.
	st := waitForJobState(t, r.control.URL, sub.JobID, job.StateSucceeded)
	if st.Attempts != 0 {
		t.Fatalf("interactive preempt burned an attempt: %d", st.Attempts)
	}
}

// --- AC-9: /healthz reflects openai backend upstream reachability ---

// TestAcceptance_AC9_HealthzReflectsUpstreamReachabilityEndToEnd proves GET
// /healthz through the real admin control-plane handler returns
// 200 {"status":"ok"} while the mock OpenAI-compatible upstream is reachable,
// and 503 naming "upstream" as the failed dependency once the upstream goes
// away (its httptest.Server closed, producing a real connection-refused
// error) — exercising the real health.Reachable probe this rig wires
// identically to cmd/broker/main.go's healthCheck (see newRig).
func TestAcceptance_AC9_HealthzReflectsUpstreamReachabilityEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "acceptance-model"}}})
	}))

	r := newRig(t, openaiCfg(t, upstream.URL))

	resp, err := http.Get(r.control.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz (upstream reachable): %v", err)
	}
	var okBody map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&okBody); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200 (upstream reachable)", resp.StatusCode)
	}
	if okBody["status"] != "ok" {
		t.Errorf(`healthz body status = %q, want "ok"`, okBody["status"])
	}

	upstream.Close() // upstream now unreachable: connection refused

	resp2, err := http.Get(r.control.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz (upstream unreachable): %v", err)
	}
	var badBody map[string]string
	if err := json.NewDecoder(resp2.Body).Decode(&badBody); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz status = %d, want 503 (upstream unreachable)", resp2.StatusCode)
	}
	if !strings.Contains(badBody["error"], "upstream") {
		t.Errorf("healthz error %q does not name the failed dependency (want it to mention %q)", badBody["error"], "upstream")
	}
}

// --- AC-10/AC-11/AC-12 (part 3 of 4): observability end to end ---

// TestAcceptance_AC10_BrokerHeadersAndTrailerEndToEnd is a light confirmation
// that a Synchronous streamed chat/generate request through the FULL rig
// (real HTTP client, openai backend, real Gate) still carries
// X-Broker-Request-Id and X-Broker-Wait-Ms as response headers, and
// X-Broker-Status as both a header and (once the stream completes) a
// trailer with the "served" outcome. internal/openaicompat/handler_test.go
// already proves this contract in depth at the Gate+handler level (Task 8);
// this test only re-derives it through the real end-to-end HTTP path, so it
// stays intentionally short.
func TestAcceptance_AC10_BrokerHeadersAndTrailerEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fl.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Broker-Request-Id"); got == "" {
		t.Error("X-Broker-Request-Id header missing")
	}
	if got := resp.Header.Get("X-Broker-Wait-Ms"); got == "" {
		t.Error("X-Broker-Wait-Ms header missing")
	}
	if got := resp.Header.Get("X-Broker-Status"); got != "served" {
		t.Errorf("X-Broker-Status header = %q, want %q", got, "served")
	}

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain streamed body (to reach the trailer): %v", err)
	}
	if got := resp.Trailer.Get("X-Broker-Status"); got != "served" {
		t.Errorf("X-Broker-Status trailer = %q, want %q", got, "served")
	}
}

// TestAcceptance_AC11_MetricsEndToEnd scrapes /metrics through the full rig
// after a served Synchronous request and a completed Job, and confirms
// broker_requests_total{class,outcome} and broker_job_outcomes_total{outcome}
// carry the expected label values, and that a couple of well-known
// pre-existing metric names (from internal/metrics/metrics.go, unrelated to
// this feature) are still present unrenamed — a spot check, not an
// exhaustive one, that this feature didn't rename/remove anything.
func TestAcceptance_AC11_MetricsEndToEnd(t *testing.T) {
	// The Job path always speaks SSE to the upstream regardless of the
	// inbound request's stream field (internal/openaicompat/client.go's
	// openChatStream always sends stream:true), so the mock must respond in
	// proper SSE shape for both the interactive request below and the Job.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	// One served Synchronous request on the interactive class.
	resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// One completed Job.
	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac11")
	waitForJobState(t, r.control.URL, jobID, job.StateSucceeded)

	mresp, err := http.Get(r.control.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", mresp.StatusCode)
	}
	raw, err := io.ReadAll(mresp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `broker_requests_total{class="interactive",outcome="served"} 1`) {
		t.Errorf("/metrics missing served interactive request count:\n%s", body)
	}
	if !strings.Contains(body, `broker_job_outcomes_total{outcome="succeeded"} 1`) {
		t.Errorf("/metrics missing succeeded job outcome count:\n%s", body)
	}

	// Spot-check: pre-existing metric names (unrelated to this feature) must
	// not have been renamed or removed.
	for _, want := range []string{"broker_yielding ", "broker_wait_seconds_count "} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing pre-existing metric %q (renamed or removed?):\n%s", want, body)
		}
	}
}

// TestAcceptance_RoutingContentionBlocksAllConfiguredInstances proves
// docs/per-model-backend-routing/requirements.md's own AC-12 (distinct from
// this file's pre-existing AC-12 above, which predates per-model routing):
// during active gaming/Plex contention, no request — regardless of which
// model or backend instance it targets — is admitted to any configured
// instance. Two real upstreams are wired (default ollama-shaped, one routed
// openai-shaped instance for "routed-model"); both track whether they were
// ever actually called. Forcing yield via the real POST /control path (the
// same mechanism TestAcceptance_AC7 uses) and then sending one request for
// the routed model and one for an unrouted model must defer both with 503,
// and neither upstream may ever see a request — proving the yield gate
// blocks admission before Router ever dispatches, for every instance alike.
func TestAcceptance_RoutingContentionBlocksAllConfiguredInstances(t *testing.T) {
	var defaultCalls, routedCalls atomic.Int32
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "m", "response": "hi", "done": true})
	}))
	defer defaultUpstream.Close()
	routedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routedCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "x", "object": "chat.completion", "choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "hi"}}}})
	}))
	defer routedUpstream.Close()

	cfg := ollamaCfg(t, defaultUpstream.URL)
	routedURL, err := url.Parse(routedUpstream.URL)
	if err != nil {
		t.Fatalf("parse routed upstream url: %v", err)
	}
	cfg.Routes = []config.RouteBackend{{Models: []string{"routed-model"}, Backend: "openai", URL: routedURL}}

	r := newRig(t, cfg)
	if r.router == nil {
		t.Fatal("newRig did not construct a Router despite cfg.Routes being set")
	}

	ctrlResp, err := http.Post(r.control.URL+"/control", "application/json", strings.NewReader(`{"mode":"yield"}`))
	if err != nil {
		t.Fatalf("POST /control: %v", err)
	}
	ctrlResp.Body.Close()
	if ctrlResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /control status = %d, want 200", ctrlResp.StatusCode)
	}

	routedResp, err := http.Post(r.interactive.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"routed-model","messages":[]}`))
	if err != nil {
		t.Fatalf("post routed model: %v", err)
	}
	routedResp.Body.Close()
	if routedResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("routed-model request during yield: status = %d, want 503", routedResp.StatusCode)
	}

	defaultResp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"unrouted-model","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("post unrouted model: %v", err)
	}
	defaultResp.Body.Close()
	if defaultResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unrouted-model request during yield: status = %d, want 503", defaultResp.StatusCode)
	}

	// routedCalls must be exactly 0: the route has no _UNIT_NAME configured,
	// so its Unloader() is nil (no unload call is ever made to it), and the
	// routed inference request itself was correctly deferred with 503 above
	// — nothing legitimate should ever reach this upstream during yield.
	if n := routedCalls.Load(); n != 0 {
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
	var defaultCalls, routedCalls atomic.Int32
	defaultUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "unrouted-model", "response": "from-default", "done": true})
	}))
	defer defaultUpstream.Close()
	routedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routedCalls.Add(1)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock routed upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-routed\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer routedUpstream.Close()

	cfg := ollamaCfg(t, defaultUpstream.URL)
	routedURL, err := url.Parse(routedUpstream.URL)
	if err != nil {
		t.Fatalf("parse routed upstream url: %v", err)
	}
	cfg.Routes = []config.RouteBackend{{Models: []string{"routed-model"}, Backend: "openai", URL: routedURL}}

	r := newRig(t, cfg)
	if r.router == nil {
		t.Fatal("newRig did not construct a Router despite cfg.Routes being set")
	}

	for _, lane := range []struct {
		name string
		addr string
	}{
		{"interactive", r.interactive.URL},
		{"batch", r.batch.URL},
	} {
		t.Run(lane.name+"/routed-model", func(t *testing.T) {
			defaultCalls.Store(0)
			routedCalls.Store(0)
			resp, err := http.Post(lane.addr+"/api/chat", "application/json", strings.NewReader(`{"model":"routed-model","messages":[]}`))
			if err != nil {
				t.Fatalf("post routed model on %s: %v", lane.name, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("routed-model on %s: status = %d, want 200, body=%s", lane.name, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "from-routed") {
				t.Errorf("routed-model on %s: body = %s, want it to contain the routed upstream's response", lane.name, body)
			}
			if n := routedCalls.Load(); n != 1 {
				t.Errorf("routed-model on %s: routed upstream got %d calls, want 1", lane.name, n)
			}
			if n := defaultCalls.Load(); n != 0 {
				t.Errorf("routed-model on %s: default upstream got %d calls, want 0 — request leaked to the wrong backend", lane.name, n)
			}
		})

		t.Run(lane.name+"/unrouted-model", func(t *testing.T) {
			defaultCalls.Store(0)
			routedCalls.Store(0)
			resp, err := http.Post(lane.addr+"/api/generate", "application/json", strings.NewReader(`{"model":"unrouted-model","prompt":"hi"}`))
			if err != nil {
				t.Fatalf("post unrouted model on %s: %v", lane.name, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("unrouted-model on %s: status = %d, want 200, body=%s", lane.name, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "from-default") {
				t.Errorf("unrouted-model on %s: body = %s, want it to contain the default upstream's response", lane.name, body)
			}
			if n := defaultCalls.Load(); n != 1 {
				t.Errorf("unrouted-model on %s: default upstream got %d calls, want 1", lane.name, n)
			}
			if n := routedCalls.Load(); n != 0 {
				t.Errorf("unrouted-model on %s: routed upstream got %d calls, want 0 — request leaked to the wrong backend", lane.name, n)
			}
		})
	}
}

// TestAcceptance_AC12_EmbedLaneUnaffectedByUpstreamBackendEndToEnd proves a
// request to the image-embedding embed lane (INFINITY_URL/BROKER_EMBED_ADDR)
// reaches a mock Infinity server through the full rig, identically regardless
// of UPSTREAM_BACKEND's value — the embed lane bypasses backend.New()/
// be.Proxy() entirely (see newRig, mirroring cmd/broker/main.go's own
// InfinityURL-gated block) and always fronts Infinity directly.
func TestAcceptance_AC12_EmbedLaneUnaffectedByUpstreamBackendEndToEnd(t *testing.T) {
	for _, backendKind := range []string{"ollama", "openai"} {
		t.Run(backendKind, func(t *testing.T) {
			var gotPath, gotBody string
			infinity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				raw, _ := io.ReadAll(r.Body)
				gotBody = string(raw)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float64{0.5}}}})
			}))
			defer infinity.Close()

			// The Ollama/openai upstream is deliberately never reached by this
			// test (nothing here should call it); a closed server proves that
			// if the embed lane were mistakenly routed through backend.New(),
			// the request would fail loudly instead of silently succeeding.
			deadUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("the ollama/openai upstream must never be reached by an embed-lane request")
			}))
			deadUpstream.Close()

			var cfg *config.Config
			if backendKind == "ollama" {
				cfg = ollamaCfg(t, deadUpstream.URL)
			} else {
				cfg = openaiCfg(t, deadUpstream.URL)
			}
			infinityURL, err := url.Parse(infinity.URL)
			if err != nil {
				t.Fatalf("parse infinity url: %v", err)
			}
			cfg.InfinityURL = infinityURL

			r := newRig(t, cfg)
			if r.embed == nil {
				t.Fatal("rig embed lane not wired despite cfg.InfinityURL set")
			}

			resp, err := http.Post(r.embed.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"input":["a picture"]}`))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if gotPath != "/embeddings_image" {
				t.Errorf("mock Infinity server saw path %q, want %q (embed lane's own route rewrite)", gotPath, "/embeddings_image")
			}
			if gotBody != `{"input":["a picture"]}` {
				t.Errorf("mock Infinity server saw body %q, want pass-through of the request body", gotBody)
			}
		})
	}
}

// --- AC-13..AC-24 (part 4 of 4, FINAL): error mapping, isolation, and the
// post-spec-challenge requirements ---
//
// Most of what follows has substantial unit-level coverage already
// (internal/openaicompat/{handler,client,embed}_test.go,
// internal/backend/*_test.go, internal/job/worker_test.go,
// internal/config/config_test.go) — these tests stay focused on proving the
// full wiring through the real rig rather than re-deriving unit-level
// translation logic. See this task's final report for which AC got a new
// end-to-end test here vs. a light confirmation vs. "already proven
// elsewhere, not duplicated."

// TestAcceptance_AC13_JobFailsAfterExhaustingRetriesOnUpstreamErrorEndToEnd
// proves that with UPSTREAM_BACKEND=openai, a Job whose mock upstream always
// returns a non-2xx status exhausts BROKER_JOB_MAX_ATTEMPTS and lands FAILED
// — through the real durable Service+Worker+openai-backend wiring, not a
// generic fakeGen (internal/job/worker_test.go's TestWorkerRetryThenFail
// already pins the generic retry-then-fail mechanics; this proves the same
// outcome specifically for an openai-backend upstream error, end to end).
func TestAcceptance_AC13_JobFailsAfterExhaustingRetriesOnUpstreamErrorEndToEnd(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := openaiCfg(t, upstream.URL)
	cfg.JobMaxAttempts = 2
	r := newRig(t, cfg)

	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac13-job")
	st := waitForJobState(t, r.control.URL, jobID, job.StateFailed)

	if st.Attempts != 2 {
		t.Errorf("job attempts = %d, want 2 (BROKER_JOB_MAX_ATTEMPTS)", st.Attempts)
	}
	if st.Error == "" {
		t.Error("failed job carries no error message")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("mock upstream called %d times, want 2 (one per attempt)", got)
	}
}

// TestAcceptance_AC13_SynchronousUpstreamErrorEndToEnd proves that with
// UPSTREAM_BACKEND=openai, a Synchronous request whose mock upstream returns
// a non-2xx status before any response bytes are sent produces a non-2xx
// error response through the real interactive Gate — the same error-response
// contract the ollama backend's errorHandler produces today (both route
// through proxy.WriteUpstreamError/its 502 fallback; see
// internal/openaicompat/handler_test.go's TestServeChat_
// PreResponseUpstream500_Returns502 for the exhaustive handler-level proof
// this only re-derives through the full stack).
func TestAcceptance_AC13_SynchronousUpstreamErrorEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a non-2xx error response consistent with the ollama backend's upstream-error handling", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response body: %v", err)
	}
	if body["error"] == "" {
		t.Error("error response body carries no error message")
	}
}

// TestAcceptance_AC14_NoRealNetworkCallAudit is a regression guard for a
// manual audit already performed for this task: every mock upstream this
// feature's test suite (internal/openaicompat, internal/backend, internal/
// job, and this file) talks to is an in-process httptest.Server bound to a
// loopback address, generated at runtime — grepping every _test.go source
// file in those packages for a literal http(s):// URL turns up nothing but
// 127.0.0.1/localhost. This test pins that fact so a future test can't
// silently reintroduce a hardcoded external host; it does not re-run or
// rebuild the audit itself.
func TestAcceptance_AC14_NoRealNetworkCallAudit(t *testing.T) {
	// Relative to internal/ (this file's own directory, the go test cwd).
	dirs := []string{".", "openaicompat", "backend", "job"}
	localOnly := regexp.MustCompile(`^https?://(127\.0\.0\.1|localhost)(:[0-9]+)?(/|$)`)
	urlRe := regexp.MustCompile(`https?://[^\s"'` + "`" + `]+`)

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := dir + "/" + e.Name()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			for _, m := range urlRe.FindAllString(string(raw), -1) {
				if !localOnly.MatchString(m) {
					t.Errorf("%s: literal non-local URL %q — every mock upstream in this feature's test suite must be an in-process httptest.Server (AC-14)", path, m)
				}
			}
		}
	}
}

// AC-15 (structural parity: UPSTREAM_BACKEND=ollama constructs the exact
// pre-feature httputil.ReverseProxy, no dispatch/translation layer
// interposed) is already proven by
// internal/backend/parity_test.go's TestOllamaBackendProxyStructuralParity —
// confirmed present and passing (see this task's final report); not
// duplicated here since it is not an end-to-end HTTP-surface concern, it's a
// construction-time type assertion the acceptance rig has no way to observe
// (newRig only ever sees the backend.Backend interface, never the concrete
// ollamaBackend/ReverseProxy types that test asserts on).

// TestAcceptance_AC16_ConfigLoadBackendURLRequirementEndToEnd proves, through
// config.Load() (not a hand-built config.Config literal like ollamaCfg/
// openaiCfg use) followed by actually starting the full rig, that
// UPSTREAM_BACKEND=openai with OLLAMA_URL unset both loads successfully and
// serves a real request — the end-to-end value internal/config/config_test.
// go's TestLoadUpstreamBackendOpenAIWithValidUpstreamURL can't provide on its
// own (it never starts a rig). The reverse case (UPSTREAM_BACKEND=ollama with
// OLLAMA_URL invalid fails Load()) has no rig to start, so it's asserted
// directly against config.Load(), matching config_test.go's existing
// TestLoadUpstreamBackendOllamaWithoutOllamaURL — included here only for a
// single coherent AC-16 proof, not because it adds new coverage.
func TestAcceptance_AC16_ConfigLoadBackendURLRequirementEndToEnd(t *testing.T) {
	t.Run("openai backend starts and serves without OLLAMA_URL", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fl, ok := w.(http.Flusher)
			if !ok {
				t.Error("mock upstream ResponseWriter is not a Flusher")
				return
			}
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			fl.Flush()
		}))
		defer upstream.Close()

		t.Setenv("UPSTREAM_BACKEND", "openai")
		t.Setenv("UPSTREAM_URL", upstream.URL)
		t.Setenv("OLLAMA_URL", "") // explicitly unset

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load() with UPSTREAM_BACKEND=openai and OLLAMA_URL unset: %v", err)
		}
		if cfg.OllamaURL != nil {
			t.Errorf("cfg.OllamaURL = %v, want nil when UPSTREAM_BACKEND=openai", cfg.OllamaURL)
		}

		r := newRig(t, cfg) // must not fail: proves the full production wiring path (config.Load -> backend.New -> Gate) tolerates a nil OllamaURL
		resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi","stream":false}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("ollama backend fails Load() without a valid OLLAMA_URL", func(t *testing.T) {
		t.Setenv("UPSTREAM_BACKEND", "ollama")
		t.Setenv("OLLAMA_URL", "not-a-url")
		if _, err := config.Load(); err == nil {
			t.Fatal("config.Load() err = nil, want a non-nil error: UPSTREAM_BACKEND=ollama with an invalid OLLAMA_URL")
		}
	})
}

// TestAcceptance_AC17_APIKeyHeaderAndLogSafetyEndToEnd proves, through the
// real rig, that UPSTREAM_API_KEY reaches the mock upstream as an
// Authorization: Bearer <value> header when non-empty and is never sent when
// empty, and that the raw key never appears in a captured slog stream during
// a real request — the CR/LF-rejected-at-Load() half of AC-17 is already
// pinned by internal/config/config_test.go's
// TestLoadUpstreamAPIKeyControlCharRejected and not duplicated here.
func TestAcceptance_AC17_APIKeyHeaderAndLogSafetyEndToEnd(t *testing.T) {
	const secretKey = "sk-acceptance-9f3c7b2a-secret"

	chatSSE := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}

	t.Run("non-empty key sent as Authorization header and never logged", func(t *testing.T) {
		var gotAuth atomic.Value
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth.Store(r.Header.Get("Authorization"))
			chatSSE(w)
		}))
		defer upstream.Close()

		cfg := openaiCfg(t, upstream.URL)
		cfg.UpstreamAPIKey = secretKey
		r := newRig(t, cfg)

		var logBuf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
		defer slog.SetDefault(prevLogger)

		resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if got, _ := gotAuth.Load().(string); got != "Bearer "+secretKey {
			t.Errorf("mock upstream saw Authorization = %q, want %q", got, "Bearer "+secretKey)
		}
		if strings.Contains(logBuf.String(), secretKey) {
			t.Errorf("raw UPSTREAM_API_KEY leaked into logs:\n%s", logBuf.String())
		}
	})

	t.Run("empty key sends no Authorization header", func(t *testing.T) {
		var gotAuth atomic.Value
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth.Store(r.Header.Get("Authorization"))
			chatSSE(w)
		}))
		defer upstream.Close()

		r := newRig(t, openaiCfg(t, upstream.URL)) // cfg.UpstreamAPIKey left at its zero value ""

		resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi"}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if got, _ := gotAuth.Load().(string); got != "" {
			t.Errorf("mock upstream saw Authorization = %q, want no header sent", got)
		}
	})
}

// TestAcceptance_AC18_StreamFalseSingleBufferedResponseEndToEnd proves that
// with UPSTREAM_BACKEND=openai, stream:false through the full rig produces a
// single buffered application/json response body — not NDJSON — complementing
// AC-4's stream:true proof above.
func TestAcceptance_AC18_StreamFalseSingleBufferedResponseEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":1}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"acceptance-model","prompt":"hi","stream":false}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not NDJSON)", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if n := strings.Count(strings.TrimSpace(string(raw)), "\n"); n != 0 {
		t.Errorf("body contains %d newline(s), want a single buffered JSON object (no NDJSON framing): %s", n, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body %q is not valid JSON: %v", raw, err)
	}
	if body["done"] != true {
		t.Errorf("done = %v, want true", body["done"])
	}
	if body["response"] != "Hello" {
		t.Errorf("response = %v, want %q", body["response"], "Hello")
	}
}

// TestAcceptance_AC19_UsageOmittedFallbackTokenCountEndToEnd proves that when
// the mock upstream omits the usage field on its final SSE chunk (replicating
// vLLM's default behavior), a Job's token-count progress still increases and
// reaches the correct final count, derived from the per-chunk fallback
// counter (internal/openaicompat/stream.go's parseSSEStream) — and that the
// outbound request to the mock upstream included stream_options:
// {include_usage:true} regardless.
func TestAcceptance_AC19_UsageOmittedFallbackTokenCountEndToEnd(t *testing.T) {
	const chunks = 4
	var gotBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		gotBody.Store(string(raw))
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(60 * time.Millisecond)
		}
		// Final chunk carries finish_reason but deliberately omits "usage",
		// replicating vLLM's default streaming behavior (FR-26/AC-19).
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))
	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac19")

	deadline := time.Now().Add(10 * time.Second)
	maxTokensSeen := 0
	finalState := job.State("")
	for time.Now().Before(deadline) {
		st := getJobStatus(t, r.control.URL, jobID)
		if st.Progress != nil && st.Progress.Tokens > maxTokensSeen {
			maxTokensSeen = st.Progress.Tokens
		}
		if st.State.Terminal() {
			finalState = st.State
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if finalState != job.StateSucceeded {
		t.Fatalf("job final state = %q, want SUCCEEDED (max tokens seen: %d)", finalState, maxTokensSeen)
	}
	if maxTokensSeen != chunks {
		t.Errorf("max tokens observed while RUNNING = %d, want %d (fallback per-chunk counter, upstream omitted usage)", maxTokensSeen, chunks)
	}

	raw, _ := gotBody.Load().(string)
	var outbound map[string]any
	if err := json.Unmarshal([]byte(raw), &outbound); err != nil {
		t.Fatalf("decode captured outbound request body %q: %v", raw, err)
	}
	so, ok := outbound["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("outbound request body missing stream_options: %s", raw)
	}
	if so["include_usage"] != true {
		t.Errorf("outbound stream_options.include_usage = %v, want true", so["include_usage"])
	}
}

// TestAcceptance_AC20_ImagesFieldRejected400EndToEnd proves that with
// UPSTREAM_BACKEND=openai, a /api/chat request whose message carries an
// "images" field is rejected 400 through the full rig, and that the mock
// upstream is never contacted.
func TestAcceptance_AC20_ImagesFieldRejected400EndToEnd(t *testing.T) {
	var upstreamCalled atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	body := `{"model":"acceptance-model","messages":[{"role":"user","content":"describe this","images":["base64data"]}]}`
	resp, err := http.Post(r.interactive.URL+"/api/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if upstreamCalled.Load() {
		t.Error("mock upstream was contacted despite the unsupported images field")
	}
}

// TestAcceptance_AC21_NonChatEndpointsEndToEnd proves that with
// UPSTREAM_BACKEND=openai, /api/tags, /api/show, /api/ps, and /api/pull each
// receive 404 through the full rig without ever reaching the mock upstream,
// while the same requests pass through unchanged under UPSTREAM_BACKEND=
// ollama (which this rig supports directly via ollamaCfg — no rig extension
// needed).
func TestAcceptance_AC21_NonChatEndpointsEndToEnd(t *testing.T) {
	paths := []string{"/api/tags", "/api/show", "/api/ps", "/api/pull"}

	t.Run("openai backend returns 404 without contacting upstream", func(t *testing.T) {
		var upstreamCalled atomic.Bool
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalled.Store(true)
			w.WriteHeader(http.StatusOK)
		}))
		defer upstream.Close()

		r := newRig(t, openaiCfg(t, upstream.URL))
		for _, p := range paths {
			resp, err := http.Get(r.interactive.URL + p)
			if err != nil {
				t.Fatalf("GET %s: %v", p, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("openai backend GET %s status = %d, want 404", p, resp.StatusCode)
			}
		}
		if upstreamCalled.Load() {
			t.Error("mock upstream was contacted for a non-chat/embed endpoint under the openai backend")
		}
	})

	t.Run("ollama backend passes through unchanged", func(t *testing.T) {
		var mu sync.Mutex
		seen := map[string]bool{}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[r.URL.Path] = true
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		}))
		defer upstream.Close()

		r := newRig(t, ollamaCfg(t, upstream.URL))
		for _, p := range paths {
			resp, err := http.Get(r.interactive.URL + p)
			if err != nil {
				t.Fatalf("GET %s: %v", p, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("ollama backend GET %s status = %d, want 200 (pass-through)", p, resp.StatusCode)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range paths {
			if !seen[p] {
				t.Errorf("ollama backend never forwarded %s to the upstream", p)
			}
		}
	})
}

// AC-22 (an /api/embed request with multiple inputs returns embeddings in the
// same order) is already fully proven end-to-end by AC-5 above
// (TestAcceptance_AC5_EmbedEndToEnd), which uses two inputs ("a","b") mapped
// to distinguishable mock vectors ([0.1 0.2] and [0.3 0.4]) and asserts each
// lands at its corresponding output index — not duplicated here. Unit-level
// order preservation is additionally pinned by internal/openaicompat/
// embed_test.go's TestClientEmbed_PreservesInputOrder.

// TestAcceptance_AC23_AC24_GenerateFieldHandlingAndModelPassthroughEndToEnd
// proves, in one full-rig /api/generate request, that: the context field is
// accepted without error and has no effect on the outbound request; the
// system field becomes a prepended system-role message; the template field is
// ignored without error; and the model value reaches the mock upstream byte-
// identical to what the Consumer supplied (AC-24) — combined into a single
// wiring proof since all four assertions come from inspecting one captured
// outbound request body. Unit-level coverage of each field individually
// already exists in internal/openaicompat/handler_test.go's
// TestServeGenerate_ContextFieldAccepted/_SystemFieldMappedToSystemMessage/
// _TemplateFieldIgnored.
func TestAcceptance_AC23_AC24_GenerateFieldHandlingAndModelPassthroughEndToEnd(t *testing.T) {
	const wantModel = "acceptance/exact-model-name:v1.2-rc3"
	var gotBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		gotBody.Store(string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	r := newRig(t, openaiCfg(t, upstream.URL))

	reqBody := fmt.Sprintf(`{"model":%q,"prompt":"hi","stream":false,"system":"be terse","template":"{{ .System }}{{ .Prompt }}","context":[1,2,3]}`, wantModel)
	resp, err := http.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (context/template must be accepted without error, AC-23)", resp.StatusCode)
	}

	raw, _ := gotBody.Load().(string)
	var outbound struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &outbound); err != nil {
		t.Fatalf("decode captured outbound request body %q: %v", raw, err)
	}

	if outbound.Model != wantModel {
		t.Errorf("outbound model = %q, want byte-identical %q (AC-24)", outbound.Model, wantModel)
	}
	if len(outbound.Messages) < 2 {
		t.Fatalf("outbound messages = %v, want a system-role message prepended before the prompt-derived user message (AC-23)", outbound.Messages)
	}
	if outbound.Messages[0]["role"] != "system" || outbound.Messages[0]["content"] != "be terse" {
		t.Errorf("outbound messages[0] = %v, want {role:system content:%q} (AC-23 system field mapping)", outbound.Messages[0], "be terse")
	}
	if strings.Contains(raw, "template") || strings.Contains(raw, `"context"`) {
		t.Errorf("outbound request body leaked the ignored template/context fields (AC-23: neither has an OpenAI-compatible equivalent and must never be forwarded): %s", raw)
	}
}
