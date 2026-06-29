// Package tdarr is a thin client for the Tdarr v2 HTTP API used to pause and
// resume GPU transcode workers cooperatively around inference and gaming windows.
//
// The broker calls PauseGPU when yielding to gaming/Plex or when the
// estate-scraper schedule window starts, and ResumeGPU when those conditions
// clear. Tdarr is always lowest priority: gaming > Plex > Ollama inference >
// Tdarr background transcode.
package tdarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client talks to a running Tdarr server.
type Client struct {
	baseURL string
	nodeID  string
	http    *http.Client
}

// New returns a Client for the Tdarr server at baseURL managing the given node.
// baseURL is e.g. "http://localhost:8265"; nodeID is the Tdarr node _id.
func New(baseURL, nodeID string) *Client {
	return &Client{
		baseURL: baseURL,
		nodeID:  nodeID,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// PauseGPU sets transcodegpu to 0 on the managed node, suspending GPU
// transcode jobs until ResumeGPU is called.
func (c *Client) PauseGPU(ctx context.Context) error {
	err := c.setWorkers(ctx, workerLimits{TranscodeCPU: 1, TranscodeGPU: 0})
	if err != nil {
		slog.Warn("tdarr: pause GPU failed", "err", err)
	} else {
		slog.Info("tdarr: GPU workers paused")
	}
	return err
}

// ResumeGPU restores transcodegpu to 1 on the managed node.
func (c *Client) ResumeGPU(ctx context.Context) error {
	err := c.setWorkers(ctx, workerLimits{TranscodeCPU: 1, TranscodeGPU: 1})
	if err != nil {
		slog.Warn("tdarr: resume GPU failed", "err", err)
	} else {
		slog.Info("tdarr: GPU workers resumed")
	}
	return err
}

// WorkerLimits queries the current worker limits for the managed node.
// Returns the transcodegpu count and any error.
func (c *Client) WorkerLimits(ctx context.Context) (gpu int, err error) {
	type req struct {
		Data struct {
			NodeID string `json:"nodeID"`
		} `json:"data"`
	}
	var body req
	body.Data.NodeID = c.nodeID

	raw, err := c.post(ctx, "/api/v2/poll-worker-limits", body)
	if err != nil {
		return 0, err
	}
	var resp struct {
		WorkerLimits workerLimits `json:"workerLimits"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, fmt.Errorf("tdarr: decode worker-limits: %w", err)
	}
	return resp.WorkerLimits.TranscodeGPU, nil
}

// workerLimits mirrors the Tdarr API shape.
type workerLimits struct {
	HealthCheckCPU int `json:"healthcheckcpu"`
	HealthCheckGPU int `json:"healthcheckgpu"`
	TranscodeCPU   int `json:"transcodecpu"`
	TranscodeGPU   int `json:"transcodegpu"`
}

func (c *Client) setWorkers(ctx context.Context, lim workerLimits) error {
	type nodeUpdates struct {
		WorkerLimits workerLimits `json:"workerLimits"`
	}
	type req struct {
		Data struct {
			NodeID      string      `json:"nodeID"`
			NodeUpdates nodeUpdates `json:"nodeUpdates"`
		} `json:"data"`
	}
	var body req
	body.Data.NodeID = c.nodeID
	body.Data.NodeUpdates = nodeUpdates{WorkerLimits: lim}

	raw, err := c.post(ctx, "/api/v2/update-node", body)
	if err != nil {
		return err
	}
	// Tdarr returns "OK" (plain text) on success.
	if string(raw) != "OK" && len(raw) > 0 {
		// Try to detect an error JSON.
		var errResp struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &errResp) == nil && errResp.Message != "" {
			return fmt.Errorf("tdarr: update-node: %s", errResp.Message)
		}
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("tdarr: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("tdarr: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tdarr: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tdarr: %s: status %d: %s", path, resp.StatusCode, raw)
	}
	return raw, nil
}
