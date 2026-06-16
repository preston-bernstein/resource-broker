// Package ollama is a minimal client for the upstream Ollama control calls the
// broker needs — listing loaded models and unloading them to free VRAM.
package ollama

import (
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
}

// New returns a Client for the Ollama base URL.
func New(base *url.URL) *Client {
	return &Client{base: base, http: &http.Client{Timeout: 15 * time.Second}}
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
