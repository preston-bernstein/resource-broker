package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/ollama"
)

// TestNewOllamaBackendReturnsNonNil verifies that New() with a valid
// UPSTREAM_BACKEND=ollama config returns a non-nil Backend, non-nil concrete
// type, and nil error.
func TestNewOllamaBackendReturnsNonNil(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "ollama",
		OllamaURL:       u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(ollama) = %v, want nil error", err)
	}
	if be == nil {
		t.Fatal("New(ollama) = nil, want non-nil Backend")
	}

	// Verify concrete type is *ollamaBackend.
	ob, ok := be.(*ollamaBackend)
	if !ok {
		t.Fatalf("New(ollama) returned %T, want *ollamaBackend", be)
	}
	if ob == nil {
		t.Fatal("type assertion succeeded but ob is nil")
	}
}

// TestNewOpenAIBackendReturnsNonNil verifies that New() with a valid
// UPSTREAM_BACKEND=openai config returns a non-nil Backend, non-nil concrete
// type, and nil error.
func TestNewOpenAIBackendReturnsNonNil(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "openai",
		UpstreamURL:     u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(openai) = %v, want nil error", err)
	}
	if be == nil {
		t.Fatal("New(openai) = nil, want non-nil Backend")
	}

	// Verify concrete type is *openaiBackend.
	oab, ok := be.(*openaiBackend)
	if !ok {
		t.Fatalf("New(openai) returned %T, want *openaiBackend", be)
	}
	if oab == nil {
		t.Fatal("type assertion succeeded but oab is nil")
	}
}

// TestOllamaBackendUnloaderNonNil verifies that the ollama backend's
// Unloader() returns a non-nil value (specifically *ollama.Client).
func TestOllamaBackendUnloaderNonNil(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "ollama",
		OllamaURL:       u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(ollama): %v", err)
	}

	unloader := be.Unloader()
	if unloader == nil {
		t.Fatal("ollama backend Unloader() = nil, want non-nil")
	}

	// Verify it's the expected type (*ollama.Client).
	if _, ok := unloader.(*ollama.Client); !ok {
		t.Errorf("ollama Unloader() = %T, want *ollama.Client", unloader)
	}
}

// TestOpenAIBackendUnloaderIsTrueNil verifies that the openai backend's
// Unloader() returns a direct, literal nil of the yield.Unloader interface
// type — not a typed-nil pointer. This is the critical regression test for
// the typed-nil safety requirement documented in plan.md.
func TestOpenAIBackendUnloaderIsTrueNil(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "openai",
		UpstreamURL:     u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(openai): %v", err)
	}

	unloader := be.Unloader()
	if unloader != nil {
		t.Fatalf("openai backend Unloader() = %#v, want a true nil interface value (typed-nil regression — see plan.md's Typed-nil safety note)", unloader)
	}
}

// TestNewInvalidBackendErrors verifies that New() gracefully errors
// (doesn't panic) when called with an invalid UpstreamBackend value.
// In production, config.Load() prevents this, but New() itself should also
// be defensive and error rather than panic.
func TestNewInvalidBackendErrors(t *testing.T) {
	cfg := &config.Config{
		UpstreamBackend: "bogus-backend",
	}

	be, err := New(cfg)
	if err == nil {
		t.Fatal("New(invalid) = nil error, want error")
	}
	if be != nil {
		t.Errorf("New(invalid) returned non-nil Backend = %#v, want nil", be)
	}

	// Verify error message is descriptive.
	if err.Error() != `backend: unknown UpstreamBackend "bogus-backend"` {
		t.Errorf("New(invalid) error message = %q, want descriptive error", err.Error())
	}
}

// TestOllamaBackendProxyReturnsHandler verifies that the ollama backend's
// Proxy() method returns a non-nil http.Handler (the reverse proxy).
func TestOllamaBackendProxyReturnsHandler(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "ollama",
		OllamaURL:       u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(ollama): %v", err)
	}

	proxy := be.Proxy()
	if proxy == nil {
		t.Fatal("ollama backend Proxy() = nil, want non-nil http.Handler")
	}
}

// TestOpenAIBackendProxyReturnsHandler verifies that the openai backend's
// Proxy() method returns a non-nil http.Handler (the translating handler).
func TestOpenAIBackendProxyReturnsHandler(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "openai",
		UpstreamURL:     u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(openai): %v", err)
	}

	proxy := be.Proxy()
	if proxy == nil {
		t.Fatal("openai backend Proxy() = nil, want non-nil http.Handler")
	}
}

// TestOllamaBackendReachableViaFactory verifies that the ollama backend
// wired by the factory has a working Reachable() method (complementary to
// the detailed Reachable tests in ollama_backend_test.go, this confirms
// the factory wires it correctly).
func TestOllamaBackendReachableViaFactory(t *testing.T) {
	// Live upstream mock.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
		}
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "ollama",
		OllamaURL:       u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(ollama): %v", err)
	}

	if err := be.Reachable(context.Background()); err != nil {
		t.Errorf("ollama Reachable() on live mock = %v, want nil", err)
	}
}

// TestOpenAIBackendReachableViaFactory verifies that the openai backend
// wired by the factory has a working Reachable() method (complementary to
// the detailed Reachable tests in openai_backend_test.go, this confirms
// the factory wires it correctly).
func TestOpenAIBackendReachableViaFactory(t *testing.T) {
	// Live upstream mock.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer mock.Close()

	u, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock url: %v", err)
	}

	cfg := &config.Config{
		UpstreamBackend: "openai",
		UpstreamURL:     u,
	}

	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New(openai): %v", err)
	}

	if err := be.Reachable(context.Background()); err != nil {
		t.Errorf("openai Reachable() on live mock = %v, want nil", err)
	}
}
