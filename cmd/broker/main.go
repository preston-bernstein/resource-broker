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

	"github.com/preston-bernstein/ollama-resource-broker/internal/admin"
	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/detect"
	"github.com/preston-bernstein/ollama-resource-broker/internal/job"
	"github.com/preston-bernstein/ollama-resource-broker/internal/metrics"
	"github.com/preston-bernstein/ollama-resource-broker/internal/ollama"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
	"github.com/preston-bernstein/ollama-resource-broker/internal/schedule"
	"github.com/preston-bernstein/ollama-resource-broker/internal/tdarr"
	"github.com/preston-bernstein/ollama-resource-broker/internal/yield"
)

// newLogger builds the process-default structured logger, aligned to
// home-infra CONVENTIONS.md §18's canonical log-line shape: schema_version,
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
	return slog.New(h).With("schema_version", 1, "service", "ollama-resource-broker")
}

func main() {
	slog.SetDefault(newLogger())

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	upstream := proxy.New(cfg.OllamaURL)
	sched := queue.New()
	sched.SetMaxWaiters(cfg.MaxWaiters)
	sched.SetMaxInflight(cfg.MaxInflight)
	sched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
	detector := detect.New(detect.ProcLister)
	oc := ollama.New(cfg.OllamaURL)
	ctrl := yield.New(detector, oc, cfg.DetectInterval)
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
	// takes the GPU (via yield controller), and during the estate-scraper window.
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
	defer store.Close()
	jobSvc := job.NewService(store, cfg.JobMaxAttempts)
	jobSvc.SetRecorder(reg)
	if err := jobSvc.Recover(ctx); err != nil {
		slog.Error("job recovery", "err", err)
		os.Exit(1)
	}
	worker := job.NewWorker(jobSvc, sched, ctrl, genAdapter{oc}, cfg.BatchQuantum, 0)
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
	// says nothing about (ADR-0010): can we reach Ollama, can we read the
	// durable job store, and is the contention-detection loop still actually
	// polling. Any one failing means the broker cannot do its job even though
	// systemd shows it active — the exact clamd-shaped defect the
	// 2026-08-01 audit flagged against the old hardcoded "ok".
	const detectStaleFactor = 3 // see ADR-0010: >3 missed polls is a stalled loop, not a slow one
	healthCheck := func(ctx context.Context) error {
		if _, err := oc.LoadedModels(ctx); err != nil {
			return fmt.Errorf("ollama upstream unreachable: %w", err)
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

	servers := []*http.Server{
		newServer(cfg.InteractiveAddr, sched.Gate(queue.Interactive, cfg.InteractiveWait, ctrl, reg, upstream)),
		newServer(cfg.BatchAddr, sched.Gate(queue.Batch, cfg.BatchWait, ctrl, reg, upstream)),
		newServer(cfg.ControlAddr, admin.Mux(ctrl, sched, healthCheck, metricsHandler, jobSvc.Routes(), jobStatus, tdarrStatusFn, cfg.ControlToken)),
	}

	// Image-embedding lane (optional): fronts an Infinity SigLIP server on CPU
	// for the estate-scraper durable corpus. It shares the yield controller —
	// so it backs off the moment gaming/Plex is detected, exactly like Ollama —
	// but runs on its OWN scheduler: CPU embedding and GPU inference use
	// different hardware, so they must not contend for the single GPU slot.
	// Disabled (lane not started) when INFINITY_URL is unset.
	if cfg.InfinityURL != nil {
		embedSched := queue.New()
		embedSched.SetMaxWaiters(cfg.MaxWaiters)
		embedSched.SetMaxInflight(1) // Infinity saturates all CPU cores per request
		embedSched.SetParkConfig(cfg.ParkHold, cfg.ParkMaxQueue, cfg.ParkDrainBurst)
		embedSched.SetShutdownContext(ctx)
		go embedSched.RunParkDrain(ctx, yieldingFn)
		embedUpstream := proxy.NewEmbed(cfg.InfinityURL)
		servers = append(servers, newServer(cfg.EmbedAddr,
			embedSched.Gate(queue.Batch, cfg.BatchWait, ctrl, reg, embedUpstream)))
		slog.Info("embed lane enabled", "addr", cfg.EmbedAddr, "upstream", cfg.InfinityURL.String())
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
	slog.Info("broker up", "upstream", cfg.OllamaURL.String(), "detect_interval", cfg.DetectInterval.String())

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

// runTdarrSchedule pauses Tdarr GPU workers during the estate-scraper window
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
					slog.Info("tdarr schedule pause: estate-scraper window active")
				}
			} else if !shouldPause && paused {
				if err := tc.ResumeGPU(ctx); err == nil {
					paused = false
					slog.Info("tdarr schedule resume: estate-scraper window ended")
				}
			}
		}
	}
}

// genAdapter bridges the Ollama client to the Job worker's Generator interface.
type genAdapter struct{ c *ollama.Client }

func (g genAdapter) Generate(ctx context.Context, model, prompt string, opts map[string]any, onTokens func(int)) (string, error) {
	out, _, err := g.c.Generate(ctx, ollama.GenerateRequest{Model: model, Prompt: prompt, Options: opts}, onTokens)
	return out, err
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// Route net/http's own error lines (e.g. superfluous WriteHeader) into
		// the structured JSON stream instead of raw stderr.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		// No WriteTimeout: inference streams can run for minutes.
	}
}
