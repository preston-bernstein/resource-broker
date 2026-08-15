// Package backend defines the upstream-agnostic Backend abstraction the
// broker fronts: either a real Ollama server (the "ollama" backend) or an
// OpenAI-compatible server such as vLLM (the "openai" backend). New(cfg)
// is the single factory that picks the concrete implementation based on
// cfg.UpstreamBackend (see docs/openai-compatible-upstream-backend/plan.md,
// "Architecture").
package backend

import (
	"context"
	"fmt"
	"net/http"

	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// Backend is the upstream-agnostic surface cmd/broker/main.go wires into the
// Gate (Proxy), the durable Job worker (Generate), /healthz (Reachable), and
// the yield Controller (Unloader).
type Backend interface {
	// Proxy returns the http.Handler that fronts the upstream for the
	// Synchronous path (wired into both the Interactive and Batch Gates).
	Proxy() http.Handler
	// Generate runs a single non-streamed-to-the-caller generation for the
	// durable Job path, invoking onTokens as tokens are produced. Signature
	// mirrors the existing Generator interface job.NewWorker accepts.
	Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error)
	// Reachable reports whether the upstream is reachable, for /healthz.
	Reachable(ctx context.Context) error
	// Unloader returns the yield.Unloader this backend uses to free upstream
	// VRAM on a yield transition, or nil if the backend has no VRAM control
	// (e.g. the openai backend — see plan.md's "Typed-nil safety" note: any
	// implementation MUST return a direct, literal nil of this interface
	// type, never a typed-nil concrete pointer boxed into it).
	Unloader() yield.Unloader
}

// New returns the concrete Backend selected by cfg.UpstreamBackend ("ollama"
// or "openai"). cfg.UpstreamBackend is validated by config.Load(), so any
// other value here indicates a caller-constructed Config that skipped that
// validation.
func New(cfg *config.Config) (Backend, error) {
	switch cfg.UpstreamBackend {
	case "ollama":
		return newOllamaBackend(cfg)
	case "openai":
		return newOpenAIBackend(cfg)
	default:
		return nil, fmt.Errorf("backend: unknown UpstreamBackend %q", cfg.UpstreamBackend)
	}
}
