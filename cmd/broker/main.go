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
	slog.SetDefault(newLogger())

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	be, err := backend.New(cfg)
	if err != nil {
		slog.Error("backend", "err", err)
		os.Exit(1)
	}

	sched := queue.New()
	sched.SetMaxWaiters(cfg.MaxWaiters)
	sched.SetMaxInflight(cfg.MaxInflight)
	sched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
	detector := detect.New(detect.ProcLister)
	if cfg.PlexToken != "" {
		detector.SetPlexChecker(plex.New(cfg.PlexURL, cfg.PlexToken))
		slog.Info("plex session corroboration enabled", "url", cfg.PlexURL)
	}
	ctrl := yield.NewWithConfirm(detector, be.Unloader(), cfg.DetectInterval, cfg.YieldConfirmPolls)
	// yieldingFn adapts ctrl.Yielding()'s (bool, string) signature to the
	// plain closure RunParkDrain expects (ADR-0009's Core redesign: the park
	// drain loop is a plain ticker poll, not a yield.Controller broadcast).
	// Shared by sched and embedSched below — both watch the same ctrl.
	yieldingFn := func() bool {
		y, _ := ctrl.Yielding()
		return y
	}
	reg := metrics.New()
	// Contention detection failing open (couldn't read /proc) must not be
	// silent — see internal/detect/detect.go's Detect() and the
	// broker_detect_errors_total counter (2026-08-01 audit fix).
	detector.SetErrorRecorder(reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.SetShutdownContext(ctx)
	go ctrl.Run(ctx)
	go sched.RunParkDrain(ctx, yieldingFn)

	// Tdarr cooperative GPU management: pause GPU transcoding when gaming/Plex
	// takes the GPU (via yield controller), and during the internal-scraper-service window.
	// Lowest priority consumer: gaming/Plex > Ollama inference > Tdarr.
	var tdarrClient *tdarr.Client
	var tdarrStatusFn admin.TdarrStatusFn
	if cfg.TdarrURL != "" && cfg.TdarrNodeID != "" {
		tdarrClient = tdarr.New(cfg.TdarrURL, cfg.TdarrNodeID, cfg.TdarrGPUWorkers)
		ctrl.SetGPUManager(tdarrClient)
		go runTdarrSchedule(ctx, tdarrClient)
		tdarrStatusFn = func() *admin.TdarrStatus {
			gpu, err := tdarrClient.WorkerLimits(context.Background())
			if err != nil {
				return &admin.TdarrStatus{Managed: true, GPUWorkers: -1}
			}
			return &admin.TdarrStatus{Managed: true, GPUWorkers: gpu}
		}
		slog.Info("tdarr integration enabled", "url", cfg.TdarrURL, "node", cfg.TdarrNodeID)
	}

	// Durable Job system (ADR-0006/0007): SQLite-backed queue + worker, sharing
	// the scheduler and yield gate with the synchronous proxy.
	store, err := job.OpenSQLite(cfg.JobDBPath)
	if err != nil {
		slog.Error("open job store", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := store.Close(); err != nil {
			slog.Warn("job store close", "err", err)
		}
	}()
	jobSvc := job.NewService(store, cfg.JobMaxAttempts)
	jobSvc.SetRecorder(reg)
	if err := jobSvc.Recover(ctx); err != nil {
		slog.Error("job recovery", "err", err)
		os.Exit(1)
	}
	worker := job.NewWorker(jobSvc, sched, ctrl, be, cfg.BatchQuantum, 0)
	go worker.Run(ctx)
	go jobSvc.RunPrune(ctx, cfg.JobPruneInterval, cfg.JobFetchedGrace, cfg.JobHardCap)

	jobCounts := func() job.Counts {
		c, err := jobSvc.Counts(context.Background())
		if err != nil {
			slog.Warn("job counts", "err", err)
		}
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

	jobStatus := func() any { return jobCounts() }

	// healthCheck backs /healthz with the three things "the process is up"
	// says nothing about (ADR-0010): can we reach the upstream, can we read
	// the durable job store, and is the contention-detection loop still
	// actually polling. Any one failing means the broker cannot do its job
	// even though systemd shows it active — the exact clamd-shaped defect the
	// 2026-08-01 audit flagged against the old hardcoded "ok".
	const detectStaleFactor = 3 // see ADR-0010: >3 missed polls is a stalled loop, not a slow one
	healthCheck := func(ctx context.Context) error {
		if err := be.Reachable(ctx); err != nil {
			return fmt.Errorf("upstream unreachable: %w", err)
		}
		if _, err := store.HasQueued(ctx); err != nil {
			return fmt.Errorf("job store unreadable: %w", err)
		}
		if maxAge := detectStaleFactor * cfg.DetectInterval; ctrl.PollAge() > maxAge {
			return fmt.Errorf("contention detector stalled: no poll in over %s (want under %s)",
				ctrl.PollAge().Round(time.Second), maxAge)
		}
		return nil
	}

	// batchServer fronts bulk/background traffic (LightRAG's embedding calls
	// among it) — exactly the workload that repeatedly hit a "server
	// disconnected without sending a response" failure during sustained
	// multi-minute runs (2026-07-15), even after removing every timer on the
	// broker's own inbound and outbound sides that could plausibly explain a
	// stale-connection race (see IdleTimeout comment above, IdleConnTimeout
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
	batchServer := newServer(cfg.BatchAddr, sched.Gate(queue.Batch, cfg.BatchWait, 0, ctrl, reg, be.Proxy()))
	batchServer.SetKeepAlivesEnabled(false)

	servers := []*http.Server{
		newServer(cfg.InteractiveAddr, sched.Gate(queue.Interactive, cfg.InteractiveWait, 0, ctrl, reg, be.Proxy())),
		batchServer,
		newServer(cfg.ControlAddr, admin.Mux(ctrl, sched, healthCheck, metricsHandler, jobSvc.Routes(), jobStatus, tdarrStatusFn, cfg.ControlToken)),
	}

	// Image-embedding lane (optional): fronts an Infinity SigLIP server on CPU
	// for the internal-scraper-service durable corpus. It shares the yield controller —
	// so it backs off the moment gaming/Plex is detected, exactly like Ollama —
	// but runs on its OWN scheduler: CPU embedding and GPU inference use
	// different hardware, so they must not contend for the single GPU slot.
	// Disabled (lane not started) when INFINITY_URL is unset.
	//
	// Gate's upstreamTimeout is set here (cfg.EmbedTimeout, ADR-0013) and
	// nowhere else: this lane's own MaxInflight is hardcoded to 1 (line
	// below), so one stuck Infinity call — no response, connection just
	// hangs — wedges the lane's single slot forever with no bound to free
	// it, unlike Ollama's interactive/batch lanes where a legitimate
	// generation can legitimately run for minutes and must NOT be cut off.
	if cfg.InfinityURL != nil {
		embedSched := queue.New()
		embedSched.SetMaxWaiters(cfg.MaxWaiters)
		embedSched.SetMaxInflight(1) // Infinity saturates all CPU cores per request
		embedSched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
		embedSched.SetShutdownContext(ctx)
		go embedSched.RunParkDrain(ctx, yieldingFn)
		// embed lane always fronts Infinity directly, never routed through
		// backend.New() — UPSTREAM_BACKEND has no effect here.
		embedUpstream := proxy.NewEmbed(cfg.InfinityURL)
		servers = append(servers, newServer(cfg.EmbedAddr,
			embedSched.Gate(queue.Batch, cfg.BatchWait, cfg.EmbedTimeout, ctrl, reg, embedUpstream)))
		slog.Info("embed lane enabled", "addr", cfg.EmbedAddr, "upstream", cfg.InfinityURL.String(), "embed_timeout", cfg.EmbedTimeout.String())
	}

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
	// cfg.OllamaURL is nil under UPSTREAM_BACKEND=openai (and cfg.UpstreamURL
	// is nil under the default "ollama" backend) — config.Load() only
	// populates the URL matching the active backend, so this log line must
	// pick whichever one is actually set rather than assuming OllamaURL.
	var upstreamURLStr string
	if cfg.UpstreamBackend == "ollama" {
		upstreamURLStr = cfg.OllamaURL.String()
	} else {
		upstreamURLStr = cfg.UpstreamURL.String()
	}
	slog.Info("broker up", "backend", cfg.UpstreamBackend, "upstream", upstreamURLStr, "detect_interval", cfg.DetectInterval.String(), "yield_confirm_polls", cfg.YieldConfirmPolls)

	// Shut down on either an OS signal or a fatal listener error — both paths
	// run the same graceful shutdown so nothing is hard-killed mid-flight.
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
			shouldPause := !schedule.SafeForBackgroundGPU(now)
			if shouldPause && !paused {
				if err := tc.PauseGPU(ctx); err == nil {
					paused = true
					slog.Info("tdarr schedule pause: internal-scraper-service window active")
				}
			} else if !shouldPause && paused {
				if err := tc.ResumeGPU(ctx); err == nil {
					paused = false
					slog.Info("tdarr schedule resume: internal-scraper-service window ended")
				}
			}
		}
	}
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
