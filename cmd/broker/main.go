// Command broker is the Ollama resource broker: an HTTP-fronting proxy that
// arbitrates a single GPU between gaming/Plex and inference consumers.
//
// M3: process-detection + binary yield + manual override on a control plane.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/preston-bernstein/ollama-resource-broker/internal/admin"
	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/detect"
	"github.com/preston-bernstein/ollama-resource-broker/internal/ollama"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
	"github.com/preston-bernstein/ollama-resource-broker/internal/yield"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	upstream := proxy.New(cfg.OllamaURL)
	sched := queue.New()
	detector := detect.New(detect.ProcLister)
	unloader := ollama.New(cfg.OllamaURL)
	ctrl := yield.New(detector, unloader, cfg.DetectInterval)

	// The detection loop is tied to the process lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctrl.Run(ctx)

	servers := []*http.Server{
		newServer(cfg.InteractiveAddr, sched.Gate(queue.Interactive, ctrl, upstream)),
		newServer(cfg.BatchAddr, sched.Gate(queue.Batch, ctrl, upstream)),
		newServer(cfg.ControlAddr, admin.Mux(ctrl)),
	}

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			log.Printf("listening on %s", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("listen %s: %v", srv.Addr, err)
			}
		}(srv)
	}
	log.Printf("upstream ollama: %s | detect every %s", cfg.OllamaURL, cfg.DetectInterval)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Print("shutting down")
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("shutdown %s: %v", srv.Addr, err)
		}
	}
	wg.Wait()
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: inference streams can run for minutes.
	}
}
