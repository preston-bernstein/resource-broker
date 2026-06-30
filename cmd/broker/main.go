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

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	upstream := proxy.New(cfg.OllamaURL)
	sched := queue.New()
	sched.SetMaxWaiters(cfg.MaxWaiters)
	sched.SetMaxInflight(cfg.MaxInflight)
	detector := detect.New(detect.ProcLister)
	oc := ollama.New(cfg.OllamaURL)
	ctrl := yield.New(detector, oc, cfg.DetectInterval)
	reg := metrics.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctrl.Run(ctx)

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
			JobQueued:    c.Queued,
			JobRunning:   c.Running,
			JobSucceeded: c.Succeeded,
			JobFailed:    c.Failed,
			JobCanceled:  c.Canceled,
		}
	})

	jobStatus := func() any { return jobCounts() }

	servers := []*http.Server{
		newServer(cfg.InteractiveAddr, sched.Gate(queue.Interactive, cfg.InteractiveWait, ctrl, reg, upstream)),
		newServer(cfg.BatchAddr, sched.Gate(queue.Batch, cfg.BatchWait, ctrl, reg, upstream)),
		newServer(cfg.ControlAddr, admin.Mux(ctrl, sched, metricsHandler, jobSvc.Routes(), jobStatus, tdarrStatusFn)),
	}

	// Image-embedding lane (optional): fronts an Infinity SigLIP server on CPU
	// for the internal-scraper-service durable corpus. It shares the yield controller —
	// so it backs off the moment gaming/Plex is detected, exactly like Ollama —
	// but runs on its OWN scheduler: CPU embedding and GPU inference use
	// different hardware, so they must not contend for the single GPU slot.
	// Disabled (lane not started) when INFINITY_URL is unset.
	if cfg.InfinityURL != nil {
		embedSched := queue.New()
		embedSched.SetMaxWaiters(cfg.MaxWaiters)
		embedSched.SetMaxInflight(1) // Infinity saturates all CPU cores per request
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
