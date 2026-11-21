package backend

import (
	"context"
	"fmt"
	"net/http"

	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/inference"
	"github.com/preston-bernstein/resource-broker/internal/ollama"
	"github.com/preston-bernstein/resource-broker/internal/proxy"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// ollamaBackend is the Backend implementation for a real upstream Ollama
// server: a transparent reverse proxy for the Synchronous path plus an
// *ollama.Client for the durable Job path, /healthz, and VRAM unload on
// yield.
type ollamaBackend struct {
	proxy http.Handler
	c     *ollama.Client
}

// newOllamaBackend constructs the "ollama" backend.
func newOllamaBackend(cfg *config.Config) (Backend, error) {
	return &ollamaBackend{
		proxy: proxy.New(cfg.OllamaURL),
		c:     ollama.New(cfg.OllamaURL),
	}, nil
}

// Proxy returns the wrapped reverse proxy handler, unchanged.
func (b *ollamaBackend) Proxy() http.Handler {
	return b.proxy
}

// Generate mirrors cmd/broker/main.go's genAdapter.Generate: bridges the
// Ollama client's streaming Generate to the Job worker's Generator interface.
func (b *ollamaBackend) Generate(ctx context.Context, req inference.Request) (string, error) {
	out, _, err := b.c.Generate(ctx, ollama.GenerateRequest{Model: req.Model, Prompt: req.Prompt, Options: req.Options}, req.OnTokens)
	if err != nil {
		return "", fmt.Errorf("ollama generate: %w", err)
	}
	return out, nil
}

// Reachable mirrors main.go's healthCheck's Ollama-reachability check.
func (b *ollamaBackend) Reachable(ctx context.Context) error {
	if _, err := b.c.LoadedModels(ctx); err != nil {
		return fmt.Errorf("ollama reachable: %w", err)
	}
	return nil
}

// Unloader returns the *ollama.Client, which already implements
// yield.Unloader via its Unload(ctx) error method.
func (b *ollamaBackend) Unloader() yield.Unloader {
	return b.c
}
