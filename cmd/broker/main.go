// Command broker is the Ollama resource broker: an HTTP-fronting proxy that
// arbitrates a single GPU between gaming/Plex and inference consumers.
//
// M6: structured JSON logs, Prometheus /metrics, response headers, /status.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/admin"
	"github.com/preston-bernstein/resource-broker/internal/backend"
	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/detect"
	"github.com/preston-bernstein/resource-broker/internal/job"
	"github.com/preston-bernstein/resource-broker/internal/metrics"
	"github.com/preston-bernstein/resource-broker/internal/plex"
	"github.com/preston-bernstein/resource-broker/internal/proxy"
	"github.com/preston-bernstein/resource-broker/internal/queue"
	"github.com/preston-bernstein/resource-broker/internal/schedule"
	"github.com/preston-bernstein/resource-broker/internal/tdarr"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// newLogger builds the process-default structured logger, aligned to
// internal-infra CONVENTIONS.md §18's canonical log-line shape: schema_version,
// ts (RFC3339 UTC — slog's own "time" key renamed, not left as-is), a
// lowercase level string, and a service identity field so this repo's JSON
// lines join the same Loki convention as the rest of the fleet. This does
// NOT retrofit the contract's "event" field onto the ~44 existing slog call
// sites in this repo — that is a larger, separate follow-up; see the
// 2026-08-01 fleet observability audit.
func newLogger() *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				a.Key = "ts"
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			case slog.LevelKey:
				a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
			}
			return a
		},
	})
	return slog.New(h).With("schema_version", 1, "service", "resource-broker")
}

func main() {
	os.Exit(run())
}

// broker is the wired-up process: everything run needs after construction.
//
// Per-model backend routing (docs/adr/0015-per-model-backend-routing.md):
// when no routes are configured (the default, and today's exact
// pre-feature behavior), router stays nil and every call site uses
// the backend directly — zero Router construction, zero body-peeking, byte-for-
// byte identical to before this feature existed. When one or more routes
// are configured, router fronts both Gates via ProxyForLane (never via
// its own Proxy(), which has no lane context) and active becomes
// router for the Job worker and healthCheck, since Router.Generate/
// Reachable already dispatch/check correctly on its own.
type broker struct {
	cfg        *config.Config
	sched      *queue.Scheduler
	ctrl       *yield.Controller
	reg        *metrics.Registry
	router     *backend.Router
	active     backend.Backend
	idleStatus func() any
}

// startupFailure logs why the process cannot start and returns its exit code.
func startupFailure(stage string, err error) int {
	slog.Error(stage, "err", err)
	return 1
}

func run() int {
	slog.SetDefault(newLogger())

	cfg, err := config.Load()
	if err != nil {
		return startupFailure("config", err)
	}

	be, err := backend.New(cfg)
	if err != nil {
		return startupFailure("backend", err)
	}

	detector := newDetector(cfg)
	router, activeBackend, ctrl, idleStatus, err := buildBroker(cfg, detector, be)
	if err != nil {
		return startupFailure("build broker", err)
	}
	b := &broker{cfg: cfg, sched: newScheduler(cfg), ctrl: ctrl, reg: metrics.New(),
		router: router, active: activeBackend, idleStatus: idleStatus}
	// Contention detection failing open (couldn't read /proc) must not be
	// silent — see internal/detect/detect.go's Detect() and the
	// broker_detect_errors_total counter (2026-08-01 audit fix).
	detector.SetErrorRecorder(b.reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.startControlLoops(ctx)
	tdarrStatus := b.startTdarr(ctx)

	store, err := job.OpenSQLite(ctx, cfg.JobDBPath)
	if err != nil {
		return startupFailure("open job store", err)
	}
	defer closeJobStore(store)
	jobSvc, err := b.startJobs(ctx, store)
	if err != nil {
		return startupFailure("job recovery", err)
	}

	servers := b.servers(ctx, jobSvc, store, tdarrStatus)
	return b.serve(cancel, servers)
}

// startJobs starts the durable Job system (ADR-0006/0007) on store: a
// SQLite-backed queue and worker that share the scheduler and yield gate with
// the synchronous proxy. It recovers Jobs a crash left running, then starts
// the worker.
func (b *broker) startJobs(ctx context.Context, store *job.SQLiteStore) (*job.Service, error) {
	cfg := b.cfg
	jobSvc := job.NewService(store, cfg.JobMaxAttempts)
	jobSvc.SetRecorder(b.reg)
	if err := jobSvc.Recover(ctx); err != nil {
		return nil, fmt.Errorf("startJobs: recover jobs: %w", err)
	}
	worker := job.NewWorker(job.WorkerConfig{Service: jobSvc, Gate: b.sched, Yielder: b.ctrl, Generator: b.active, Quantum: cfg.BatchQuantum})
	go worker.Run(ctx)
	go jobSvc.RunPrune(ctx, cfg.JobPruneInterval, cfg.JobFetchedGrace, cfg.JobHardCap)
	return jobSvc, nil
}

func closeJobStore(store *job.SQLiteStore) {
	if err := store.Close(); err != nil {
		slog.Warn("job store close", "err", err)
	}
}

func newScheduler(cfg *config.Config) *queue.Scheduler {
	sched := queue.New()
	sched.SetMaxWaiters(cfg.MaxWaiters)
	sched.SetMaxInflight(cfg.MaxInflight)
	sched.SetCPUModels(cfg.CPUModels) // ADR-0018: CPU-only models skip the GPU slot
	sched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
	return sched
}

func newDetector(cfg *config.Config) *detect.Detector {
	detector := detect.New(detect.ProcLister)
	if cfg.PlexToken != "" {
		detector.SetPlexChecker(plex.New(cfg.PlexURL, cfg.PlexToken))
		slog.Info("plex session corroboration enabled", "url", cfg.PlexURL)
	}
	return detector
}

// yielding adapts ctrl.Yielding()'s (bool, string) signature to the plain
// closure RunParkDrain expects (ADR-0009's Core redesign: the park drain loop
// is a plain ticker poll, not a yield.Controller broadcast). Shared by the main
// scheduler and the embed lane's — both watch the same ctrl.
func (b *broker) yielding() bool {
	y, _ := b.ctrl.Yielding()
	return y
}

func (b *broker) startControlLoops(ctx context.Context) {
	b.sched.SetShutdownContext(ctx)
	go b.ctrl.Run(ctx)
	go b.sched.RunParkDrain(ctx, b.yielding)
}

// startTdarr wires Tdarr cooperative GPU management: pause GPU transcoding
// when gaming/Plex takes the GPU (via yield controller), and during the
// internal-scraper-service window. Lowest priority consumer: gaming/Plex >
// Ollama inference > Tdarr. It returns nil when Tdarr is not configured.
func (b *broker) startTdarr(ctx context.Context) admin.TdarrStatusFn {
	cfg := b.cfg
	if cfg.TdarrURL == "" || cfg.TdarrNodeID == "" {
		return nil
	}
	tdarrClient := tdarr.New(cfg.TdarrURL, cfg.TdarrNodeID, cfg.TdarrGPUWorkers)
	b.ctrl.SetGPUManager(tdarrClient)
	go runTdarrSchedule(ctx, tdarrClient)
	slog.Info("tdarr integration enabled", "url", cfg.TdarrURL, "node", cfg.TdarrNodeID)
	return func(ctx context.Context) *admin.TdarrStatus {
		gpu, err := tdarrClient.WorkerLimits(ctx)
		if err != nil {
			return &admin.TdarrStatus{Managed: true, GPUWorkers: -1}
		}
		return &admin.TdarrStatus{Managed: true, GPUWorkers: gpu}
	}
}

// jobCountsFn returns the Job counts, logging (not failing) a store error.
func jobCountsFn(jobSvc *job.Service) func(context.Context) job.Counts {
	return func(ctx context.Context) job.Counts {
		c, err := jobSvc.Counts(ctx)
		if err != nil {
			slog.Warn("job counts", "err", err)
		}
		return c
	}
}

func (b *broker) metricsHandler(jobCounts func(context.Context) job.Counts) http.Handler {
	return b.reg.Handler(func(ctx context.Context) metrics.Gauges {
		st := b.sched.Stats()
		yielding, _ := b.ctrl.Yielding()
		c := jobCounts(ctx)
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
}

// detectStaleFactor: see ADR-0010: >3 missed polls is a stalled loop, not a slow one.
const detectStaleFactor = 3

// healthCheck backs /healthz with the three things "the process is up"
// says nothing about (ADR-0010): can we reach the upstream, can we read
// the durable job store, and is the contention-detection loop still
// actually polling. Any one failing means the broker cannot do its job
// even though systemd shows it active — the exact clamd-shaped defect the
// 2026-08-01 audit flagged against the old hardcoded "ok".
func (b *broker) healthCheck(store *job.SQLiteStore) admin.HealthCheck {
	return func(ctx context.Context) error {
		if err := b.active.Reachable(ctx); err != nil {
			return fmt.Errorf("upstream unreachable: %w", err)
		}
		if _, err := store.HasQueued(ctx); err != nil {
			return fmt.Errorf("job store unreadable: %w", err)
		}
		if maxAge := detectStaleFactor * b.cfg.DetectInterval; b.ctrl.PollAge() > maxAge {
			return fmt.Errorf("contention detector stalled: no poll in over %s (want under %s)",
				b.ctrl.PollAge().Round(time.Second), maxAge)
		}
		return nil
	}
}

// laneProxies picks the handlers behind the interactive and batch gates.
// router.ProxyForLane's own doc comment notes Proxy() (no lane context) is not
// what production wiring should call — this is that production wiring, always
// passing a real lane (queue.Class.String()) when routes are configured. When
// router is nil (zero routes), activeBackend.Proxy() is the exact pre-feature
// code path (no Router construction, no body-peeking) EXCEPT that activeBackend
// — not the pre-buildBroker local be — must be used here: buildBroker's
// no-routes branch may have reassigned its own be parameter to a
// backend.WithActivityTracking-decorated wrapper (idle-unload enabled)
// and returned that decorated value as activeBackend. be is a plain Go
// interface value passed to buildBroker by copy, so buildBroker
// reassigning its own be parameter never changes this be — only
// activeBackend carries the decoration back out. Using be.Proxy() here
// silently drops idle-activity tracking for every interactive/batch
// request on the no-routes path (verified live: /status kept reporting
// idle_unloaded=true and since_last_dispatch kept climbing across a real
// served request, with no wake-triggered reload log line, until this was
// fixed to reference activeBackend instead).
func (b *broker) laneProxies() (interactive, batch http.Handler) {
	if b.router != nil {
		return b.router.ProxyForLane(queue.Interactive.String()), b.router.ProxyForLane(queue.Batch.String())
	}
	return b.active.Proxy(), b.active.Proxy()
}

// servers builds every listener: interactive, batch, control, and the optional
// embed lane.
func (b *broker) servers(ctx context.Context, jobSvc *job.Service, store *job.SQLiteStore, tdarrStatus admin.TdarrStatusFn) []*http.Server {
	cfg := b.cfg
	jobCounts := jobCountsFn(jobSvc)
	interactiveProxy, batchProxy := b.laneProxies()

	var routingStatus func() any
	if b.router != nil {
		routingStatus = b.router.RoutingSummary
	}

	servers := []*http.Server{
		newServer(cfg.InteractiveAddr, b.sched.Gate(queue.GateConfig{Class: queue.Interactive, Wait: cfg.InteractiveWait, Admission: b.ctrl, Recorder: b.reg}, interactiveProxy)),
		b.batchServer(batchProxy),
		newServer(cfg.ControlAddr, admin.Mux(admin.Options{
			Controller:    b.ctrl,
			Stats:         b.sched,
			Health:        b.healthCheck(store),
			Metrics:       b.metricsHandler(jobCounts),
			Jobs:          jobSvc.Routes(),
			JobStatus:     func(ctx context.Context) any { return jobCounts(ctx) },
			TdarrStatus:   tdarrStatus,
			RoutingStatus: routingStatus,
			IdleStatus:    b.idleStatus,
			ControlToken:  cfg.ControlToken,
		})),
	}
	if cfg.InfinityURL != nil {
		servers = append(servers, b.embedServer(ctx))
	}
	return servers
}

// batchServer fronts bulk/background traffic (LightRAG's embedding calls
// among it) — exactly the workload that repeatedly hit a "server
// disconnected without sending a response" failure during sustained
// multi-minute runs (2026-07-15), even after removing every timer on the
// broker's own inbound and outbound sides that could plausibly explain a
// stale-connection race (see newServer's IdleTimeout comment, IdleConnTimeout
// and retryTransport in internal/proxy/proxy.go). The broker's own access
// log showed every request "served" cleanly on the outbound (Ollama) leg
// right up to the crash, and the retry layer never activated — narrowing
// the failure to the final hop, writing the response back over an
// already-established connection that something (the client's own pool,
// or the network path) killed without either endpoint logging why.
// SetKeepAlivesEnabled(false) removes connection reuse entirely on this
// lane: every request gets a fresh TCP connection, so there is no pooled
// connection left for either side to race on reusing. Scoped to the
// batch lane only (not interactive) since the extra per-request TCP
// handshake is negligible on a LAN but this trades a little latency for
// eliminating an entire failure class — not worth paying on the
// low-latency interactive lane where the race hasn't been observed.
func (b *broker) batchServer(proxy http.Handler) *http.Server {
	srv := newServer(b.cfg.BatchAddr, b.sched.Gate(queue.GateConfig{Class: queue.Batch, Wait: b.cfg.BatchWait, Admission: b.ctrl, Recorder: b.reg}, proxy))
	srv.SetKeepAlivesEnabled(false)
	return srv
}

// embedServer builds the image-embedding lane: it fronts an Infinity SigLIP
// server on CPU for the internal-scraper-service durable corpus. It shares the
// yield controller — so it backs off the moment gaming/Plex is detected,
// exactly like Ollama — but runs on its OWN scheduler: CPU embedding and GPU
// inference use different hardware, so they must not contend for the single
// GPU slot. The lane is not started when INFINITY_URL is unset.
//
// Gate's UpstreamTimeout is set here (cfg.EmbedTimeout, ADR-0013) and
// nowhere else: this lane's own MaxInflight is hardcoded to 1 (line
// below), so one stuck Infinity call — no response, connection just
// hangs — wedges the lane's single slot forever with no bound to free
// it, unlike Ollama's interactive/batch lanes where a legitimate
// generation can legitimately run for minutes and must NOT be cut off.
func (b *broker) embedServer(ctx context.Context) *http.Server {
	cfg := b.cfg
	embedSched := queue.New()
	embedSched.SetMaxWaiters(cfg.MaxWaiters)
	embedSched.SetMaxInflight(1) // Infinity saturates all CPU cores per request
	embedSched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
	embedSched.SetShutdownContext(ctx)
	go embedSched.RunParkDrain(ctx, b.yielding)
	// embed lane always fronts Infinity directly, never routed through
	// backend.New() — UPSTREAM_BACKEND has no effect here.
	embedUpstream := proxy.NewEmbed(cfg.InfinityURL)
	srv := newServer(cfg.EmbedAddr,
		embedSched.Gate(queue.GateConfig{Class: queue.Batch, Wait: cfg.BatchWait, UpstreamTimeout: cfg.EmbedTimeout, Admission: b.ctrl, Recorder: b.reg}, embedUpstream))
	slog.Info("embed lane enabled", "addr", cfg.EmbedAddr, "upstream", cfg.InfinityURL.String(), "embed_timeout", cfg.EmbedTimeout.String())
	return srv
}

// upstreamURL returns the URL of the active backend. cfg.OllamaURL is nil
// under UPSTREAM_BACKEND=openai (and cfg.UpstreamURL is nil under the default
// "ollama" backend) — config.Load() only populates the URL matching the active
// backend, so the log line must pick whichever one is actually set.
func upstreamURL(cfg *config.Config) string {
	if cfg.UpstreamBackend == "ollama" {
		return cfg.OllamaURL.String()
	}
	return cfg.UpstreamURL.String()
}

// serve runs every server until an OS signal or a fatal listener error, then
// shuts them down gracefully. Both paths run the same shutdown so nothing is
// hard-killed mid-flight. It returns the process exit code.
func (b *broker) serve(cancel context.CancelFunc, servers []*http.Server) int {
	cfg := b.cfg
	var wg sync.WaitGroup
	errCh := make(chan error, len(servers))
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			slog.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listen %s: %w", srv.Addr, err)
			}
		}(srv)
	}
	slog.Info("broker up", "backend", cfg.UpstreamBackend, "upstream", upstreamURL(cfg), "detect_interval", cfg.DetectInterval.String(), "yield_confirm_polls", cfg.YieldConfirmPolls)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
		slog.Info("shutting down")
	case err := <-errCh:
		slog.Error("server failed, shutting down", "err", err)
	}
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutCtx); err != nil {
			slog.Error("shutdown", "addr", srv.Addr, "err", err)
		}
	}
	wg.Wait()
	return 0
}

// newUnitUnloader builds the Unloader for one BROKER_YIELD_UNITS entry. A
// variable so tests can record stop/start without running systemctl.
var newUnitUnloader = backend.NewUnitUnloader

// routeUnloader returns route rb's Unloader. A variable for the same reason as
// newUnitUnloader: ConfigureDisplace stops a displacing route at construction.
var routeUnloader = func(_ config.RouteBackend, b backend.Backend) yield.Unloader { return b.Unloader() }

// yieldUnits are the BROKER_YIELD_UNITS (ADR-0019) Unloaders and their labels.
// They go after the default backend and every route, so the orig indexes
// 0..len(cfg.Routes) keep their meaning. They never idle-unload: their idle
// timeout is always 0.
type yieldUnits struct {
	unloaders []yield.Unloader
	labels    []string
}

func newYieldUnits(cfg *config.Config) yieldUnits {
	var units yieldUnits
	for _, u := range cfg.YieldUnits {
		units.unloaders = append(units.unloaders, newUnitUnloader(u))
		units.labels = append(units.labels, "unit:"+u)
	}
	return units
}

// buildBroker performs the pure (no network I/O, no os.Exit) wiring that
// used to live inline in main(): constructing the yield.Controller, the
// optional per-model Router, and the idle-unload decoration of whichever
// backend(s) are actually in play. It exists so this logic has a real test
// seam — run() itself blocks on ListenAndServe/signal handling and can't
// easily be driven by a unit test.
//
// THE ORDERING BUG THIS FIXES: it is tempting to build the Router first
// (AddRoute captures each routeBackends[i] and be BY VALUE into the
// Router's routing table at that moment) and only afterward construct ctrl
// and decorate be/routeBackends[i] with backend.WithActivityTracking — ctrl
// doesn't exist until yield.NewWithConfirmMulti runs, so building it last
// feels natural. But reassigning the local be/routeBackends[i] variables
// AFTER the Router has already captured the undecorated values does nothing
// to what's stored inside the Router — idle-tracking would silently never
// fire for any routed instance, despite compiling cleanly and passing any
// test that constructs Router directly. The fix is strict ordering: ctrl
// must exist, and every backend must be decorated, BEFORE backend.NewRouter
// and the AddRoute loop ever run.
func buildBroker(cfg *config.Config, detector yield.Detector, be backend.Backend) (router *backend.Router, activeBackend backend.Backend, ctrl *yield.Controller, idleStatus func() any, err error) {
	units := newYieldUnits(cfg)
	switch {
	case len(cfg.Routes) > 0:
		router, ctrl, err = buildRouted(cfg, detector, be, units)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		activeBackend = router
	case len(units.unloaders) > 0:
		ctrl, activeBackend = buildUnitsOnly(cfg, detector, be, units)
	default:
		ctrl, activeBackend = buildSingle(cfg, detector, be)
	}
	if hasIdleTimeout(cfg) {
		idleStatus = ctrl.IdleSummary
	}
	return router, activeBackend, ctrl, idleStatus, nil
}

func hasIdleTimeout(cfg *config.Config) bool {
	if cfg.UpstreamIdleTimeout != 0 {
		return true
	}
	for _, rb := range cfg.Routes {
		if rb.IdleTimeout != 0 {
			return true
		}
	}
	return false
}

// buildUnitsOnly wires the default backend plus BROKER_YIELD_UNITS, no routes.
func buildUnitsOnly(cfg *config.Config, detector yield.Detector, be backend.Backend, units yieldUnits) (*yield.Controller, backend.Backend) {
	unloaders := append([]yield.Unloader{be.Unloader()}, units.unloaders...)
	labels := append([]string{"default"}, units.labels...)
	ctrl := yield.NewWithConfirmMulti(detector, unloaders, labels, yield.Polling{Interval: cfg.DetectInterval, ConfirmPolls: cfg.YieldConfirmPolls})
	idleTimeouts := make([]time.Duration, len(unloaders))
	idleTimeouts[0] = cfg.UpstreamIdleTimeout
	ctrl.ConfigureIdle(idleTimeouts)
	if cfg.UpstreamIdleTimeout != 0 {
		be = backend.WithActivityTracking(be, ctrl, 0)
	}
	return ctrl, be
}

// buildSingle wires the default backend alone: no routes, no yield units.
func buildSingle(cfg *config.Config, detector yield.Detector, be backend.Backend) (*yield.Controller, backend.Backend) {
	// unl may be a true nil yield.Unloader (e.g. an openai backend with no
	// configured unit name) — NewWithConfirm then passes NewWithConfirmMulti
	// a zero-length unloaders slice (it does not keep a nil placeholder for
	// a nil single unloader), so ctrl.origToFiltered ends up length 0 in
	// that case. Calling ConfigureIdle would then panic on a length
	// mismatch against a 1-element idleTimeouts slice. config.Load()
	// already guarantees UpstreamIdleTimeout is 0 whenever there is no
	// Unloader to act on, so skipping ConfigureIdle/decoration entirely
	// when unl is nil loses nothing.
	unl := be.Unloader()
	ctrl := yield.NewWithConfirm(detector, unl, cfg.DetectInterval, cfg.YieldConfirmPolls)
	if unl == nil {
		return ctrl, be
	}
	ctrl.ConfigureIdle([]time.Duration{cfg.UpstreamIdleTimeout})
	if cfg.UpstreamIdleTimeout != 0 {
		be = backend.WithActivityTracking(be, ctrl, 0)
	}
	return ctrl, be
}

// buildRouted wires the default backend, every route and the yield units
// behind a Router.
func buildRouted(cfg *config.Config, detector yield.Detector, be backend.Backend, units yieldUnits) (*backend.Router, *yield.Controller, error) {
	routeBackends, routeUnloaders, routeLabels, err := buildRoutes(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("route construction: %w", err)
	}
	// unloaders/labels are computed, and ctrl constructed, BEFORE
	// backend.NewRouter/AddRoute run below — see buildBroker's doc
	// comment for why the order matters.
	unloaders := append([]yield.Unloader{be.Unloader()}, routeUnloaders...)
	unloaders = append(unloaders, units.unloaders...)
	labels := append([]string{"default"}, routeLabels...)
	labels = append(labels, units.labels...)
	ctrl := yield.NewWithConfirmMulti(detector, unloaders, labels, yield.Polling{Interval: cfg.DetectInterval, ConfirmPolls: cfg.YieldConfirmPolls})
	configureRoutedController(cfg, ctrl, len(unloaders))

	// Decorate in place — orig index 0 is the default backend, i+1 is
	// route i — BEFORE NewRouter/AddRoute below capture these values.
	if cfg.UpstreamIdleTimeout != 0 {
		be = backend.WithActivityTracking(be, ctrl, 0)
	}
	for i := range cfg.Routes {
		if cfg.Routes[i].IdleTimeout != 0 {
			routeBackends[i] = backend.WithActivityTracking(routeBackends[i], ctrl, i+1)
		}
	}

	r := backend.NewRouter(be)
	for i, rb := range cfg.Routes {
		for _, model := range rb.Models {
			r.AddRoute(model, rb.Lane, routeBackends[i])
		}
	}
	var routeSummary []string
	for _, rb := range cfg.Routes {
		routeSummary = append(routeSummary, fmt.Sprintf("%v->%s(%s)", rb.Models, rb.Backend, rb.URL.String()))
	}
	slog.Info("routing configured", "routes", routeSummary)
	return r, ctrl, nil
}

// configureRoutedController sets idle timeouts, displacement and the unit
// resume holdoff on ctrl. unloaderCount is the length of the unloaders slice
// ctrl was built from.
func configureRoutedController(cfg *config.Config, ctrl *yield.Controller, unloaderCount int) {
	// idleTimeouts is index-aligned with unloaders/labels: orig index
	// 0 is the default backend, orig index i+1 is cfg.Routes[i], and the
	// BROKER_YIELD_UNITS entries follow with a 0 timeout. Unlike the
	// single-unloader branch, this slice's length always matches
	// ctrl.origToFiltered's length regardless of any individual nil
	// Unloader, since the unloaders slice is always exactly
	// 1+len(cfg.Routes)+len(cfg.YieldUnits) long (append never drops a
	// nil element by position).
	idleTimeouts := make([]time.Duration, unloaderCount)
	idleTimeouts[0] = cfg.UpstreamIdleTimeout
	for i, rb := range cfg.Routes {
		idleTimeouts[i+1] = rb.IdleTimeout
	}
	ctrl.ConfigureIdle(idleTimeouts)

	// BROKER_ROUTE_<N>_DISPLACES (ADR-0020), in the same orig index space:
	// route i is i+1, yield unit k is 1+len(cfg.Routes)+k.
	unitOrig := map[string]int{}
	for k, u := range cfg.YieldUnits {
		unitOrig[u] = 1 + len(cfg.Routes) + k
	}
	displace := map[int][]int{}
	for i, rb := range cfg.Routes {
		for _, u := range rb.Displaces {
			displace[i+1] = append(displace[i+1], unitOrig[u])
		}
	}
	ctrl.ConfigureDisplace(displace)
	ctrl.SetUnitResumeHoldoff(cfg.UnitResumeHoldoff)
}

// buildRoutes constructs one Backend per cfg.Routes[i] via backend.NewInstance
// and returns it alongside that instance's own yield.Unloader() and a log
// label, all index-aligned by construction in a single pass — not three
// independently-built slices trusted to correlate by convention, since a
// silent mismatch would route request bodies through one instance while
// unloading a different one (docs/adr/0015-per-model-backend-routing.md).
func buildRoutes(cfg *config.Config) (backends []backend.Backend, unloaders []yield.Unloader, labels []string, err error) {
	backends = make([]backend.Backend, len(cfg.Routes))
	unloaders = make([]yield.Unloader, len(cfg.Routes))
	labels = make([]string, len(cfg.Routes))
	for i, rb := range cfg.Routes {
		b, err := backend.NewInstance(rb.Backend, rb.URL, rb.APIKey, rb.UnitName)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("route %d (%v): %w", i, rb.Models, err)
		}
		backends[i] = b
		unloaders[i] = routeUnloader(rb, b)
		labels[i] = rb.URL.String()
	}
	return backends, unloaders, labels, nil
}

// runTdarrSchedule pauses Tdarr GPU workers during the internal-scraper-service window
// (Fri 02:00–07:00) and resumes them outside it. This is the schedule-based
// coordination path; the yield-controller path handles gaming/Plex contention.
func runTdarrSchedule(ctx context.Context, tc *tdarr.Client) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var paused bool
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			paused = reconcileTdarr(ctx, tc, paused, !schedule.SafeForBackgroundGPU(now))
		}
	}
}

// reconcileTdarr moves Tdarr toward shouldPause and returns the new paused
// state. A failed call leaves the state unchanged so the next tick retries.
func reconcileTdarr(ctx context.Context, tc *tdarr.Client, paused, shouldPause bool) bool {
	switch {
	case shouldPause && !paused:
		if err := tc.PauseGPU(ctx); err == nil {
			slog.Info("tdarr schedule pause: internal-scraper-service window active")
			return true
		}
	case !shouldPause && paused:
		if err := tc.ResumeGPU(ctx); err == nil {
			slog.Info("tdarr schedule resume: internal-scraper-service window ended")
			return false
		}
	}
	return paused
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// No IdleTimeout: an inbound client (LightRAG's httpx client, in
		// particular) legitimately goes idle for minutes at a time between
		// requests — e.g. its own CPU-bound entity/relationship merge work
		// between embedding bursts — while still holding this connection open
		// in its own pool as "good." A 60s IdleTimeout was tried on
		// 2026-07-15 on the theory that proactively closing idle connections
		// server-side would prevent a client from reusing one that had gone
		// stale for some external reason; in practice this made the broker
		// itself the thing closing connections out from under a client that
		// still considered them valid — confirmed 2026-07-15 by a LightRAG
		// bulk-embedding crash ("server disconnected without sending a
		// response") landing after a 3+ minute gap with zero broker-side
		// activity, comfortably past that 60s timeout, on every observed
		// crash. A server unilaterally closing idle keep-alives shorter than
		// a real client's idle pattern IS the stale-connection race, not a
		// defense against it — removed rather than tuned, since there's no
		// value here we can pick that's safely longer than every legitimate
		// client gap. See Development/Research/
		// lightrag-ollama-embedding-batch-instability.md on the vault.
		// Route net/http's own error lines (e.g. superfluous WriteHeader) into
		// the structured JSON stream instead of raw stderr.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		// No WriteTimeout: inference streams can run for minutes.
	}
}
