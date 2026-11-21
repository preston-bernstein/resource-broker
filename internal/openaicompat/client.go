// Package openaicompat implements the OpenAI-*compatible* wire protocol
// against a self-hosted upstream (the real target is vLLM) — never a call to
// OpenAI's actual hosted service. See docs/openai-compatible-upstream-backend
// for the full design; the package name deliberately says "compat" rather
// than "openai" to avoid reading as a hosted-API integration in a shop that
// never calls cloud inference.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/preston-bernstein/resource-broker/internal/proxy"
)

// Client talks to a single OpenAI-compatible upstream (e.g. vLLM) for the
// Job worker's Generate path.
type Client struct {
	base   *url.URL
	apiKey string
	http   *http.Client
}

// New returns a Client for the OpenAI-compatible upstream base URL. apiKey
// may be empty, in which case no Authorization header is ever sent (never
// send "Authorization: Bearer " with an empty token).
func New(base *url.URL, apiKey string) *Client {
	return &Client{
		base:   base,
		apiKey: apiKey,
		// Reuse internal/proxy's connection-retry transport rather than a bare
		// http.Client{} — the same class of connection-level failure on a busy
		// shared host that motivated that transport for Ollama/Infinity applies
		// equally here (Design decision #5, docs/openai-compatible-upstream-backend/plan.md).
		http: &http.Client{Transport: proxy.Transport},
	}
}

// GenerateRequest is a Job's inference request: a model, a prompt, and any
// extra options (temperature, num_predict/max_tokens, …) passed through
// verbatim. Shape mirrors ollama.Client's GenerateRequest so the eventual
// internal/backend/openai_backend.go adapter (which satisfies the same
// job.Generator interface as the ollama backend) can construct one directly
// from the (model, prompt, options) arguments job.Worker already passes.
type GenerateRequest struct {
	Model   string
	Prompt  string
	Options map[string]any
}

// Generate runs a streaming POST {UPSTREAM_URL}/v1/chat/completions call,
// translating req into an OpenAI-compatible chat/completions body (a single
// user-role message carrying Prompt, per the Ollama/OpenAI-compat
// translation table) and accumulating the response text. onTokens (optional)
// is called with the running token count as chunks arrive. It returns the
// full response and the final token count, mirroring ollama.Client.Generate's
// signature and error-handling shape (including ctx.Err() short-circuiting on
// cancellation) for symmetry and testability.
//
// Request building and the non-2xx check live in openChatStream; the response
// body's SSE parsing lives in parseSSEStream (internal/openaicompat/stream.go).
func (c *Client) Generate(ctx context.Context, req GenerateRequest, onTokens func(int)) (string, int, error) {
	return c.chatCompletion(ctx, chatRequest{
		Model:    req.Model,
		Messages: []map[string]any{{"role": "user", "content": req.Prompt}},
		Options:  req.Options,
	}, onTokens)
}

// postJSON POSTs body as JSON to the upstream path joined from elem, adding the
// Authorization header when an API key is set. The caller owns the response
// and its status check.
func (c *Client) postJSON(ctx context.Context, body any, elem ...string) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(elem...), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai upstream: %w", err)
	}
	return resp, nil
}

// drainClose reads any remaining body and closes it so the keep-alive
// connection can be reused instead of leaking.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// url joins the client's base URL with elem using proper URL joining (never
// plain string concatenation, which breaks when base has a trailing slash) —
// see the "URL construction" section of docs/openai-compatible-upstream-backend
// /plan.md. Mirrors the precedent set by internal/ollama/client.go's own
// url(path string) helper, adapted to url.JoinPath's variadic-element form
// since OpenAI-compatible routes are joined from multiple path segments.
func (c *Client) url(elem ...string) string {
	return c.base.JoinPath(elem...).String()
}
