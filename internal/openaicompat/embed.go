package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// EmbedRequest is Ollama's /api/embed request shape, already normalized to
// a slice of inputs regardless of whether the Consumer sent a single string
// or an array of strings (see DecodeEmbedRequest).
type EmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EmbedResponse is Ollama's /api/embed response shape. The order of
// Embeddings corresponds exactly to the order of Input in the originating
// EmbedRequest (FR-7, AC-22).
type EmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
}

// openaiEmbedRequest is the outbound {base}/v1/embeddings request shape.
type openaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// openaiEmbedResponse is the OpenAI-compatible /v1/embeddings response
// shape. Index is used to place each embedding into its correct output
// position rather than trusting Data's array order, since the OpenAI-
// compatible convention does not guarantee the upstream returns Data in
// input order (see Client.Embed).
type openaiEmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// DecodeEmbedRequest decodes an incoming Ollama-shaped /api/embed request
// body into an EmbedRequest. Ollama's real /api/embed API accepts `input`
// as either a single string or an array of strings; both are normalized
// here into EmbedRequest.Input so downstream code never has to special-case
// the single-string form.
func DecodeEmbedRequest(body io.Reader) (EmbedRequest, error) {
	var raw struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return EmbedRequest{}, fmt.Errorf("decode embed request: %w", err)
	}

	input, err := decodeEmbedInput(raw.Input)
	if err != nil {
		return EmbedRequest{}, err
	}

	return EmbedRequest{Model: raw.Model, Input: input}, nil
}

// decodeEmbedInput accepts either a JSON string or a JSON array of strings
// for the `input` field, per Ollama's /api/embed contract.
func decodeEmbedInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("embed request: missing required field \"input\"")
	}

	var multi []string
	if err := json.Unmarshal(raw, &multi); err == nil {
		return multi, nil
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}

	return nil, fmt.Errorf("embed request: \"input\" must be a string or an array of strings")
}

// Embed runs a POST {base}/v1/embeddings call, translating req into the
// OpenAI-compatible embeddings request shape and reshaping the response
// back into Ollama's /api/embed response shape.
//
// CRITICAL: the upstream's response.data[].index field (not data's array
// order) determines each embedding's position in the returned
// EmbedResponse.Embeddings slice, so the response vector order always
// matches req.Input's order even if the upstream returns data out of order
// (FR-7, AC-22).
func (c *Client) Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error) {
	payload, err := json.Marshal(openaiEmbedRequest(req))
	if err != nil {
		return EmbedResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("v1", "embeddings"), bytes.NewReader(payload))
	if err != nil {
		return EmbedResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return EmbedResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return EmbedResponse{}, fmt.Errorf("openai upstream: status %d", resp.StatusCode)
	}

	var oaiResp openaiEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		return EmbedResponse{}, fmt.Errorf("decode embeddings response: %w", err)
	}

	embeddings := make([][]float64, len(oaiResp.Data))
	for _, item := range oaiResp.Data {
		if item.Index < 0 || item.Index >= len(embeddings) {
			return EmbedResponse{}, fmt.Errorf("openai upstream: embedding index %d out of range for %d inputs", item.Index, len(embeddings))
		}
		embeddings[item.Index] = item.Embedding
	}

	model := oaiResp.Model
	if model == "" {
		model = req.Model
	}

	return EmbedResponse{Model: model, Embeddings: embeddings}, nil
}
