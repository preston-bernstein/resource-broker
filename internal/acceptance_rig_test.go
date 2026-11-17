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
// The suite covers all 24 acceptance criteria (AC-1 through AC-24), split by
// domain: synchronous requests (acceptance_sync_test.go), durable Jobs and
// preemption (acceptance_jobs_test.go), per-model routing
// (acceptance_routing_test.go), and the openai backend's wire contract
// (acceptance_openai_test.go). This file holds the rig they share.
package broker_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/admin"
	"github.com/preston-bernstein/resource-broker/internal/backend"
	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/detect"
	"github.com/preston-bernstein/resource-broker/internal/job"
	"github.com/preston-bernstein/resource-broker/internal/metrics"
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
	reg         *metrics.Registry
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
	// active is the backend the Job worker and health check use: the router
	// when routes are configured, else be.
	active backend.Backend
}

// newRig builds a rig from cfg, exactly the way cmd/broker/main.go's main()
// builds the Broker, and registers cleanup. It deliberately does NOT start
// ctrl.Run(ctx) (the contention-detection poll loop): the tests here don't
// exercise gaming/Plex detection, and a Controller that never runs its poll
// loop simply stays in its zero-value ModeAuto/not-yielding state, which is
// exactly what every test needs. The detector itself is still wired with a
// real (but no-op) process Lister so cfg-driven fields (DetectInterval,
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

	reg := metrics.New()
	detector := detect.New(func() ([]detect.Process, error) { return nil, nil })
	detector.SetErrorRecorder(reg)

	r := &rig{cfg: cfg, be: be, sched: sched, reg: reg, active: be}
	r.buildController(t, detector)

	ctx, cancel := context.WithCancel(context.Background())
	sched.SetShutdownContext(ctx)
	go sched.RunParkDrain(ctx, r.yielding)

	r.startJobs(ctx, t)
	r.startServers(ctx, t)
	// Registered last so it runs first: stop the background loops, then close
	// the servers, then the store.
	t.Cleanup(cancel)
	return r
}

func (r *rig) yielding() bool {
	y, _ := r.ctrl.Yielding()
	return y
}

// buildController mirrors cmd/broker/main.go's own router-construction gate
// exactly (docs/adr/0015-per-model-backend-routing.md): when cfg.Routes is
// empty (every test predating per-model routing), router stays nil and active
// is be — byte-for-byte the pre-feature rig. Only tests that explicitly set
// cfg.Routes exercise the routed path, keeping this rig representative of real
// production wiring either way — the whole reason this suite exists.
func (r *rig) buildController(t *testing.T, detector yield.Detector) {
	t.Helper()
	cfg := r.cfg
	if len(cfg.Routes) == 0 {
		r.ctrl = yield.NewWithConfirm(detector, r.be.Unloader(), cfg.DetectInterval, cfg.YieldConfirmPolls)
		return
	}
	routeBackends := make([]backend.Backend, len(cfg.Routes))
	unloaders := make([]yield.Unloader, len(cfg.Routes)+1)
	labels := make([]string, len(cfg.Routes)+1)
	unloaders[0] = r.be.Unloader()
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
	r.router = backend.NewRouter(r.be)
	for i, rb := range cfg.Routes {
		for _, model := range rb.Models {
			r.router.AddRoute(model, rb.Lane, routeBackends[i])
		}
	}
	r.active = r.router
	r.ctrl = yield.NewWithConfirmMulti(detector, unloaders, labels, yield.Polling{Interval: cfg.DetectInterval, ConfirmPolls: cfg.YieldConfirmPolls})
}

// startJobs opens the in-memory job store, recovers it, and starts the worker.
func (r *rig) startJobs(ctx context.Context, t *testing.T) {
	t.Helper()
	store, err := job.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open job store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r.jobSvc = job.NewService(store, r.cfg.JobMaxAttempts)
	r.jobSvc.SetRecorder(r.reg)
	if err := r.jobSvc.Recover(ctx); err != nil {
		t.Fatalf("job recover: %v", err)
	}
	worker := job.NewWorker(job.WorkerConfig{Service: r.jobSvc, Gate: r.sched, Yielder: r.ctrl, Generator: r.active, Quantum: r.cfg.BatchQuantum})
	go worker.Run(ctx)
}

func (r *rig) jobCounts(ctx context.Context) job.Counts {
	c, _ := r.jobSvc.Counts(ctx)
	return c
}

func (r *rig) metricsHandler() http.Handler {
	return r.reg.Handler(func(ctx context.Context) metrics.Gauges {
		st := r.sched.Stats()
		c := r.jobCounts(ctx)
		return metrics.Gauges{
			Yielding:     r.yielding(),
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
}

// healthCheck mirrors cmd/broker/main.go's healthCheck upstream check exactly
// (down to the "upstream unreachable: %w" wrapping): AC-9 requires /healthz's
// 503 body to name "upstream" as the failed dependency, and a bare
// Reachable(ctx) error (e.g. a raw "connection refused") doesn't guarantee that
// word appears. main.go's job-store and detector-staleness checks are
// deliberately omitted: no test here needs them wired.
func (r *rig) healthCheck(ctx context.Context) error {
	if err := r.active.Reachable(ctx); err != nil {
		return fmt.Errorf("upstream unreachable: %w", err)
	}
	return nil
}

// startServers starts the interactive, batch and control servers, plus the
// embed lane when cfg.InfinityURL is set, and closes them with the test.
func (r *rig) startServers(ctx context.Context, t *testing.T) {
	t.Helper()
	cfg := r.cfg
	interactiveProxy, batchProxy := r.be.Proxy(), r.be.Proxy()
	var routingStatus func() any
	if r.router != nil {
		interactiveProxy = r.router.ProxyForLane(queue.Interactive.String())
		batchProxy = r.router.ProxyForLane(queue.Batch.String())
		routingStatus = r.router.RoutingSummary
	}
	gate := func(class queue.Class, wait time.Duration, next http.Handler) *httptest.Server {
		return startServer(t, r.sched.Gate(queue.GateConfig{Class: class, Wait: wait, Admission: r.ctrl, Recorder: r.reg}, next))
	}
	r.interactive = gate(queue.Interactive, cfg.InteractiveWait, interactiveProxy)
	r.batch = gate(queue.Batch, cfg.BatchWait, batchProxy)
	r.control = startServer(t, admin.Mux(admin.Options{
		Controller:    r.ctrl,
		Stats:         r.sched,
		Health:        r.healthCheck,
		Metrics:       r.metricsHandler(),
		Jobs:          r.jobSvc.Routes(),
		JobStatus:     func(ctx context.Context) any { return r.jobCounts(ctx) },
		RoutingStatus: routingStatus,
		ControlToken:  cfg.ControlToken,
	}))
	if cfg.InfinityURL != nil {
		r.embed = r.startEmbedLane(ctx, t)
	}
}

// startEmbedLane starts the optional image-embedding lane (AC-12): mirrors
// cmd/broker/main.go's own "if cfg.InfinityURL != nil" block — its own
// scheduler (never r.sched), NewEmbed fronting InfinityURL directly,
// completely bypassing backend.New()/be.Proxy() — proving the embed lane is
// unaffected by cfg.UpstreamBackend.
func (r *rig) startEmbedLane(ctx context.Context, t *testing.T) *httptest.Server {
	t.Helper()
	cfg := r.cfg
	embedSched := queue.New()
	embedSched.SetMaxWaiters(cfg.MaxWaiters)
	embedSched.SetMaxInflight(1)
	embedSched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
	embedSched.SetShutdownContext(ctx)
	go embedSched.RunParkDrain(ctx, r.yielding)
	gateCfg := queue.GateConfig{Class: queue.Batch, Wait: cfg.BatchWait, UpstreamTimeout: cfg.EmbedTimeout, Admission: r.ctrl, Recorder: r.reg}
	return startServer(t, embedSched.Gate(gateCfg, proxy.NewEmbed(cfg.InfinityURL)))
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
	cfg := baseCfg()
	cfg.UpstreamBackend = "ollama"
	cfg.OllamaURL = mustParseURL(t, upstreamURL)
	return &cfg
}

func openaiCfg(t *testing.T, upstreamURL string) *config.Config {
	t.Helper()
	cfg := baseCfg()
	cfg.UpstreamBackend = "openai"
	cfg.UpstreamURL = mustParseURL(t, upstreamURL)
	return &cfg
}

// readLineWithin reads one line with a deadline, mirroring
// internal/openaicompat's helper of the same name: a buffered
// (non-incrementally-flushed) implementation would hang past the deadline
// instead of returning quickly, turning a would-be-silent bug into a clear
// test failure.
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
	resp := get(t, controlURL+"/jobs/"+jobID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/%s: status %d", jobID, resp.StatusCode)
	}
	var st job.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode job status: %v", err)
	}
	return st
}

// waitForJobState polls GET /jobs/<id> through the real HTTP surface until the
// Job reaches want or the deadline expires, returning the last-observed
// status either way (mirroring getJobStatus's "always hit the real endpoint"
// discipline).
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

// submitJobViaHTTP submits a Job for acceptance-model through the real POST
// /jobs surface, returning its ID.
func submitJobViaHTTP(t *testing.T, controlURL, idempotencyKey string) string {
	t.Helper()
	return submitJobBody(t, controlURL, idempotencyKey, `{"model":"acceptance-model","prompt":"hi"}`)
}

// submitJobBody posts body to POST /jobs with the Idempotency-Key header and
// returns the new Job's ID.
func submitJobBody(t *testing.T, controlURL, idempotencyKey, body string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, controlURL+"/jobs", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build submit request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
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
