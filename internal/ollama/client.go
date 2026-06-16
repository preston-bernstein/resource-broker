// Package ollama is a minimal client for the upstream Ollama control calls the
// broker needs — listing loaded models and unloading them to free VRAM.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a single Ollama instance.
type Client struct {
	base *url.URL
	http *http.Client
	// gen has no timeout: a Job generation can stream for minutes, bounded only
	// by the caller's context (job cancel, gaming yield, interactive preempt).
	gen *http.Client
}

// New returns a Client for the Ollama base URL.
func New(base *url.URL) *Client {
	return &Client{
		base: base,
		http: &http.Client{Timeout: 15 * time.Second},
		gen:  &http.Client{},
	}
}

// GenerateRequest is a Job's inference request: a model, a prompt, and any
// extra Ollama options (temperature, num_predict, …) passed through verbatim.
type GenerateRequest struct {
	Model   string
	Prompt  string
	Options map[string]any
}

// Generate runs a streaming /api/generate call, accumulating the response text.
// onTokens (optional) is called with the running token count as chunks arrive,
// so a caller can surface live progress. It returns the full response and the
// final token count, or ctx.Err() if the call is cancelled (job cancel, yield,
// or preemption) — the partial response is discarded in that case.
func (c *Client) Generate(ctx context.Context, req GenerateRequest, onTokens func(int)) (string, int, error) {
	body := map[string]any{"model": req.Model, "prompt": req.Prompt, "stream": true}
	for k, v := range req.Options {
		// Never let caller options override the fields the broker controls.
		if k == "model" || k == "prompt" || k == "stream" {
			continue
		}
		body[k] = v
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", 0, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/generate"), bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.gen.Do(httpReq)
	if err != nil {
		return "", 0, err
	}
	defer drainClose(resp)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("ollama /api/generate: status %d", resp.StatusCode)
	}

	var sb strings.Builder
	tokens := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // tolerate long NDJSON lines
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Response  string `json:"response"`
			Done      bool   `json:"done"`
			EvalCount int    `json:"eval_count"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			return "", tokens, fmt.Errorf("decode stream chunk: %w", err)
		}
		if chunk.Error != "" {
			return "", tokens, fmt.Errorf("ollama: %s", chunk.Error)
		}
		if chunk.Response != "" {
			sb.WriteString(chunk.Response)
			tokens++
			if onTokens != nil {
				onTokens(tokens)
			}
		}
		if chunk.Done {
			if chunk.EvalCount > 0 {
				tokens = chunk.EvalCount
			}
			return sb.String(), tokens, nil
		}
	}
	if err := sc.Err(); err != nil {
		// Context cancellation surfaces here as a read error; report it as such.
		if ctx.Err() != nil {
			return "", tokens, ctx.Err()
		}
		return "", tokens, err
	}
	// Stream ended without a done marker (e.g. upstream closed) — treat as error
	// so the Job can retry rather than persist a truncated result as success.
	if ctx.Err() != nil {
		return "", tokens, ctx.Err()
	}
	return "", tokens, fmt.Errorf("ollama stream ended without done")
}

// LoadedModels returns the names of models currently resident in memory,
// from GET /api/ps.
func (c *Client) LoadedModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/api/ps"), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/ps: status %d", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// Unload frees VRAM by unloading every currently-loaded model. Ollama unloads
// a model when sent a generate request with keep_alive=0. Best-effort: it
// attempts every model and returns the first error encountered.
func (c *Client) Unload(ctx context.Context) error {
	names, err := c.LoadedModels(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, name := range names {
		if err := c.unloadOne(ctx, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Client) unloadOne(ctx context.Context, model string) error {
	payload, _ := json.Marshal(map[string]any{"model": model, "keep_alive": 0})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/generate"), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drainClose(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unload %s: status %d", model, resp.StatusCode)
	}
	return nil
}

// drainClose reads any remaining body and closes it so the keep-alive
// connection can be reused instead of leaking.
func drainClose(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func (c *Client) url(path string) string {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String()
}
