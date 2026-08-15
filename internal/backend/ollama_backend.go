package backend

import (
	"context"
	"net/http"

	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/ollama"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
	"github.com/preston-bernstein/ollama-resource-broker/internal/yield"
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
func (b *ollamaBackend) Generate(ctx context.Context, model, prompt string, opts map[string]any, onTokens func(int)) (string, error) {
	out, _, err := b.c.Generate(ctx, ollama.GenerateRequest{Model: model, Prompt: prompt, Options: opts}, onTokens)
	return out, err
}

// Reachable mirrors main.go's healthCheck's Ollama-reachability check.
func (b *ollamaBackend) Reachable(ctx context.Context) error {
	_, err := b.c.LoadedModels(ctx)
	return err
}

// Unloader returns the *ollama.Client, which already implements
// yield.Unloader via its Unload(ctx) error method.
func (b *ollamaBackend) Unloader() yield.Unloader {
	return b.c
}
