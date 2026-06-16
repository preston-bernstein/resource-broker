// Command broker is the Ollama resource broker: an HTTP-fronting proxy that
// arbitrates a single GPU between gaming/Plex and inference consumers.
//
// M2: two class ports (interactive/batch) sharing one concurrency-1 priority
// scheduler in front of a streaming passthrough to Ollama.
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

	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
	"github.com/preston-bernstein/ollama-resource-broker/internal/queue"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	upstream := proxy.New(cfg.OllamaURL)
	sched := queue.New()

	servers := []*http.Server{
		newServer(cfg.InteractiveAddr, sched.Gate(queue.Interactive, upstream)),
		newServer(cfg.BatchAddr, sched.Gate(queue.Batch, upstream)),
	}

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			log.Printf("listening on %s -> %s", srv.Addr, cfg.OllamaURL)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("listen %s: %v", srv.Addr, err)
			}
		}(srv)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
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
