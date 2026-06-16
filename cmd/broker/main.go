// Command broker is the Ollama resource broker: an HTTP-fronting proxy that
// arbitrates a single GPU between gaming/Plex and inference consumers.
//
// M1: transparent streaming passthrough to Ollama (no queue/yield yet).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	handler := proxy.New(cfg.OllamaURL)

	// M1 runs a single listener. Later milestones add the second class port.
	srv := &http.Server{
		Addr:              cfg.InteractiveAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: inference streams can run for minutes.
	}

	go func() {
		log.Printf("broker M1 passthrough listening on %s -> %s", cfg.InteractiveAddr, cfg.OllamaURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
