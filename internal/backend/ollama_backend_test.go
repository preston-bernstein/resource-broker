package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/ollama"
)

// newTestOllamaBackend builds an *ollamaBackend pointed at the given mock
// Ollama server URL, bypassing config.Load() so tests don't need a full
// environment.
func newTestOllamaBackend(t *testing.T, mockURL string) *ollamaBackend {
	t.Helper()
	u, err := url.Parse(mockURL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}
	cfg := &config.Config{OllamaURL: u}
	be, err := newOllamaBackend(cfg)
	if err != nil {
		t.Fatalf("newOllamaBackend: %v", err)
	}
	ob, ok := be.(*ollamaBackend)
	if !ok {
		t.Fatalf("newOllamaBackend returned %T, want *ollamaBackend", be)
	}
	return ob
}

func TestOllamaBackendProxyPassesThrough(t *testing.T) {
	var gotPath string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mock.Close()

	ob := newTestOllamaBackend(t, mock.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	rec := httptest.NewRecorder()
	ob.Proxy().ServeHTTP(rec, req)

	if gotPath != "/api/tags" {
		t.Errorf("upstream saw path %q, want /api/tags", gotPath)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want {\"ok\":true}", body)
	}
}

// mockGenerateServer returns an httptest server that emits the given NDJSON
// stream chunks verbatim for POST /api/generate.
func mockGenerateServer(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			w.Write([]byte(c + "\n"))
		}
	}))
}

func TestOllamaBackendGenerateConcatenatesResponse(t *testing.T) {
	chunks := []string{
		`{"response":"Hel","done":false}`,
		`{"response":"lo, ","done":false}`,
		`{"response":"world","done":false}`,
		`{"response":"","done":true,"eval_count":3}`,
	}
	mock := mockGenerateServer(t, chunks)
	defer mock.Close()

	ob := newTestOllamaBackend(t, mock.URL)

	var tokenCounts []int
	out, err := ob.Generate(context.Background(), "llama3", "hi", nil, func(n int) {
		tokenCounts = append(tokenCounts, n)
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "Hello, world" {
		t.Errorf("Generate() = %q, want %q", out, "Hello, world")
	}
	if len(tokenCounts) != 3 {
		t.Errorf("onTokens called %d times, want 3", len(tokenCounts))
	}
}

func TestOllamaBackendReachable(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "llama3"}}})
	}))
	defer mock.Close()

	ob := newTestOllamaBackend(t, mock.URL)
	if err := ob.Reachable(context.Background()); err != nil {
		t.Errorf("Reachable() on live mock = %v, want nil", err)
	}
}

func TestOllamaBackendUnreachable(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	mock.Close() // closed immediately: connection refused

	ob := newTestOllamaBackend(t, mock.URL)
	if err := ob.Reachable(context.Background()); err == nil {
		t.Error("Reachable() on unreachable mock = nil, want error")
	}
}

func TestOllamaBackendUnloaderReturnsClient(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	ob := newTestOllamaBackend(t, mock.URL)
	u := ob.Unloader()
	if u == nil {
		t.Fatal("Unloader() = nil, want non-nil")
	}
	if _, ok := u.(*ollama.Client); !ok {
		t.Errorf("Unloader() = %T, want *ollama.Client", u)
	}
}
