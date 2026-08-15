package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestClientEmbed_PreservesInputOrder proves the response reshaping code
// reorders by the upstream's data[].index field rather than trusting data's
// array order: the mock server deliberately returns index 1's embedding
// before index 0's, and uses distinguishable per-input values (AC-22).
func TestClientEmbed_PreservesInputOrder(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("mock server: decode request: %v", err)
		}

		// Deliberately out of order: index 1 before index 0, plus a third
		// entry for index 2 to prove more than a simple two-element swap.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"model": "test-embed-model",
			"data": [
				{"index": 1, "embedding": [2.0, 2.0]},
				{"index": 2, "embedding": [3.0, 3.0]},
				{"index": 0, "embedding": [1.0, 1.0]}
			]
		}`))
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := New(base, "test-key")

	req := EmbedRequest{
		Model: "test-embed-model",
		Input: []string{"first", "second", "third"},
	}

	resp, err := client.Embed(context.Background(), req)
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}

	if gotPath != "/v1/embeddings" {
		t.Errorf("expected request path /v1/embeddings, got %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("expected Authorization header %q, got %q", "Bearer test-key", gotAuth)
	}
	if gotBody.Model != "test-embed-model" {
		t.Errorf("expected upstream model %q, got %q", "test-embed-model", gotBody.Model)
	}
	if len(gotBody.Input) != 3 || gotBody.Input[0] != "first" || gotBody.Input[1] != "second" || gotBody.Input[2] != "third" {
		t.Errorf("expected upstream input [first second third], got %v", gotBody.Input)
	}

	if resp.Model != "test-embed-model" {
		t.Errorf("expected response model %q, got %q", "test-embed-model", resp.Model)
	}
	if len(resp.Embeddings) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(resp.Embeddings))
	}

	want := [][]float64{{1.0, 1.0}, {2.0, 2.0}, {3.0, 3.0}}
	for i, w := range want {
		got := resp.Embeddings[i]
		if len(got) != len(w) || got[0] != w[0] || got[1] != w[1] {
			t.Errorf("embeddings[%d] = %v, want %v (order must match request input order)", i, got, w)
		}
	}
}

// TestClientEmbed_NoAuthHeaderWhenKeyEmpty confirms the Authorization
// header is never sent when the API key is empty, matching client.go's
// convention exactly.
func TestClientEmbed_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	var authHeaderPresent bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, authHeaderPresent = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"m","data":[{"index":0,"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := New(base, "")

	_, err = client.Embed(context.Background(), EmbedRequest{Model: "m", Input: []string{"x"}})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}

	if authHeaderPresent {
		t.Error("expected no Authorization header when apiKey is empty")
	}
}

// TestClientEmbed_UpstreamError confirms a non-2xx upstream status is
// mapped to the standard "openai upstream: status %d" error.
func TestClientEmbed_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := New(base, "")

	_, err = client.Embed(context.Background(), EmbedRequest{Model: "m", Input: []string{"x"}})
	if err == nil {
		t.Fatal("expected an error for a non-2xx upstream response, got nil")
	}
}

// TestClientEmbed_IndexEqualToLengthIsOutOfRange verifies the upstream
// index-range check rejects item.Index == len(embeddings) — the exact
// boundary where "last valid index" (len-1) and "one past the end" (len)
// diverge — rather than only rejecting indexes strictly greater than len.
// No prior test ever sent an out-of-range index at all (every fixture used
// valid indexes 0..len-1), leaving embed.go's
// `item.Index < 0 || item.Index >= len(embeddings)` upper-bound boundary
// unexercised.
func TestClientEmbed_IndexEqualToLengthIsOutOfRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// One input, but the upstream reports index 1 (== len(embeddings),
		// one past the only valid index 0) — must be rejected, not silently
		// indexed out of range.
		w.Write([]byte(`{"model":"m","data":[{"index":1,"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := New(base, "")

	_, err = client.Embed(context.Background(), EmbedRequest{Model: "m", Input: []string{"only-one"}})
	if err == nil {
		t.Fatal("expected an out-of-range index error, got nil")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %q, want it to contain %q", err.Error(), "out of range")
	}
}

// TestClientEmbed_EmptyUpstreamModelFallsBackToRequestModel verifies that
// when the upstream's /v1/embeddings response omits (or empties) its
// "model" field, the returned EmbedResponse.Model falls back to the
// request's own model rather than surfacing an empty string. No prior test
// ever sent an empty upstream model — every fixture set a non-empty
// "model" — leaving embed.go's `if model == "" { model = req.Model }`
// fallback unexercised.
func TestClientEmbed_EmptyUpstreamModelFallsBackToRequestModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"","data":[{"index":0,"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := New(base, "")

	resp, err := client.Embed(context.Background(), EmbedRequest{Model: "req-model", Input: []string{"x"}})
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	if resp.Model != "req-model" {
		t.Fatalf("resp.Model = %q, want %q (must fall back to the request model)", resp.Model, "req-model")
	}
}

func TestDecodeEmbedRequest_ArrayInput(t *testing.T) {
	body := strings.NewReader(`{"model":"m","input":["a","b","c"]}`)
	req, err := DecodeEmbedRequest(body)
	if err != nil {
		t.Fatalf("DecodeEmbedRequest returned error: %v", err)
	}
	if req.Model != "m" {
		t.Errorf("expected model %q, got %q", "m", req.Model)
	}
	want := []string{"a", "b", "c"}
	if len(req.Input) != len(want) {
		t.Fatalf("expected %d inputs, got %d", len(want), len(req.Input))
	}
	for i := range want {
		if req.Input[i] != want[i] {
			t.Errorf("Input[%d] = %q, want %q", i, req.Input[i], want[i])
		}
	}
}

func TestDecodeEmbedRequest_SingleStringInput(t *testing.T) {
	body := strings.NewReader(`{"model":"m","input":"only-one"}`)
	req, err := DecodeEmbedRequest(body)
	if err != nil {
		t.Fatalf("DecodeEmbedRequest returned error: %v", err)
	}
	if len(req.Input) != 1 || req.Input[0] != "only-one" {
		t.Errorf("expected Input [only-one], got %v", req.Input)
	}
}

func TestDecodeEmbedRequest_MissingInput(t *testing.T) {
	body := strings.NewReader(`{"model":"m"}`)
	if _, err := DecodeEmbedRequest(body); err == nil {
		t.Fatal("expected an error for a request missing \"input\", got nil")
	}
}

func TestDecodeEmbedRequest_InvalidInputType(t *testing.T) {
	body := strings.NewReader(`{"model":"m","input":42}`)
	if _, err := DecodeEmbedRequest(body); err == nil {
		t.Fatal("expected an error for a non-string/array \"input\", got nil")
	}
}
