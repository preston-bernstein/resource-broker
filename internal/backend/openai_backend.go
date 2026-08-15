package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/openaicompat"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
	"github.com/preston-bernstein/ollama-resource-broker/internal/yield"
)

// openaiBackend is the Backend implementation for an OpenAI-compatible
// upstream server (e.g. vLLM): a hand-written translating handler for the
// Synchronous path plus an *openaicompat.Client for the durable Job path.
// It has no VRAM control of its own (see Unloader below).
type openaiBackend struct {
	proxy       http.Handler
	c           *openaicompat.Client
	upstreamURL *url.URL
	apiKey      string
	http        *http.Client
}

// newOpenAIBackend constructs the "openai" backend.
func newOpenAIBackend(cfg *config.Config) (Backend, error) {
	return &openaiBackend{
		proxy:       openaicompat.NewHandler(cfg.UpstreamURL, cfg.UpstreamAPIKey),
		c:           openaicompat.New(cfg.UpstreamURL, cfg.UpstreamAPIKey),
		upstreamURL: cfg.UpstreamURL,
		apiKey:      cfg.UpstreamAPIKey,
		// Reuse internal/proxy's connection-retry transport, same as
		// openaicompat.Client and openaicompat.NewHandler do internally, for
		// the same reasons documented there (Design decision #5).
		http: &http.Client{Transport: proxy.Transport},
	}, nil
}

// Proxy returns the wrapped OpenAI-compatible translating handler.
func (b *openaiBackend) Proxy() http.Handler {
	return b.proxy
}

// Generate bridges the openaicompat client's streaming Generate to the Job
// worker's Generator interface, mirroring ollamaBackend.Generate.
func (b *openaiBackend) Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error) {
	out, _, err := b.c.Generate(ctx, openaicompat.GenerateRequest{Model: model, Prompt: prompt, Options: options}, onTokens)
	return out, err
}

// Reachable probes the upstream with GET {UPSTREAM_URL}/v1/models, the
// standard OpenAI-compatible model-listing endpoint, returning an error if
// the request fails or the upstream responds with a non-2xx status. Uses
// proper URL joining (url.JoinPath), never plain string concatenation,
// mirroring the pattern set by openaicompat.Client's own url() helper.
func (b *openaiBackend) Reachable(ctx context.Context) error {
	target := b.upstreamURL.JoinPath("v1", "models").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openai upstream: status %d", resp.StatusCode)
	}
	return nil
}

// Unloader always returns nil: the openai backend has no VRAM control of its
// own (an OpenAI-compatible upstream such as vLLM has no equivalent of
// Ollama's unload-on-idle mechanism this broker can drive).
//
// This MUST be a direct, literal nil of the yield.Unloader interface type —
// never an intermediate concrete (possibly-nil) pointer variable assigned
// then returned. yield.go's nil-guard (`if c.unloader != nil { go
// c.doUnload() }`) only catches a true nil *interface* value; a typed nil
// (a nil pointer of some concrete type boxed into the interface) would pass
// that check and then panic on a nil-receiver call to Unload inside an
// unrecovered goroutine, crashing the whole broker process at the exact
// moment real gaming/Plex contention is detected. See
// docs/openai-compatible-upstream-backend/plan.md's "Typed-nil safety for
// openaiBackend.Unloader()" paragraph and Design decision #1.
func (b *openaiBackend) Unloader() yield.Unloader { return nil }
