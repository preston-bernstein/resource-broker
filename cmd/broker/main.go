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
	"github.com/preston-bernstein/ollama-resource-broker/internal/metrics"
	"github.com/preston-bernstein/ollama-resource-broker/internal/ollama"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
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
	detector := detect.New(detect.ProcLister)
	unloader := ollama.New(cfg.OllamaURL)
	ctrl := yield.New(detector, unloader, cfg.DetectInterval)
	reg := metrics.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctrl.Run(ctx)

	metricsHandler := reg.Handler(func() metrics.Gauges {
		st := sched.Stats()
		yielding, _ := ctrl.Yielding()
		return metrics.Gauges{
			Yielding:    yielding,
			Busy:        st.Busy,
			Interactive: st.Interactive,
			Batch:       st.Batch,
		}
	})

	servers := []*http.Server{
		newServer(cfg.InteractiveAddr, sched.Gate(queue.Interactive, cfg.InteractiveWait, ctrl, reg, upstream)),
		newServer(cfg.BatchAddr, sched.Gate(queue.Batch, cfg.BatchWait, ctrl, reg, upstream)),
		newServer(cfg.ControlAddr, admin.Mux(ctrl, sched, metricsHandler)),
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
