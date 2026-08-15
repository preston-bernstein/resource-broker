package backend

import (
	"net/http/httputil"
	"os"
	"reflect"
	"testing"

	"github.com/preston-bernstein/ollama-resource-broker/internal/config"
	"github.com/preston-bernstein/ollama-resource-broker/internal/proxy"
)

// withEnv sets key to value for the duration of the test (or unsets it, when
// value is ""), restoring whatever the environment had beforehand on
// cleanup. Used below to make the UPSTREAM_BACKEND-unset case deterministic
// regardless of the surrounding shell's environment.
func withEnv(t *testing.T, key, value string) {
	t.Helper()
	old, had := os.LookupEnv(key)
	if value == "" {
		os.Unsetenv(key)
	} else {
		os.Setenv(key, value)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}

// TestOllamaBackendProxyStructuralParity is the FR-22/AC-15 proof required by
// docs/openai-compatible-upstream-backend/requirements.md and plan.md's
// Design decision #8: "latency-equivalence for the ollama backend is
// verified structurally ... not by wall-clock benchmarking". It constructs a
// Backend the same way cmd/broker/main.go does in production — via
// config.Load() followed by backend.New(cfg) — for both UPSTREAM_BACKEND
// left unset (the documented default) and UPSTREAM_BACKEND=ollama set
// explicitly, then asserts that ollamaBackend.Proxy() is exactly what the
// pre-feature call site, proxy.New(cfg.OllamaURL), produces: the same
// concrete *httputil.ReverseProxy type, the same shared proxy.Transport
// instance (by pointer identity, since that transport is a single
// package-level var by design), and the same FlushInterval — proving no
// additional translation/dispatch layer has been interposed on the ollama
// Synchronous path, by construction rather than by measurement.
func TestOllamaBackendProxyStructuralParity(t *testing.T) {
	cases := []struct {
		name               string
		upstreamBackendEnv string
	}{
		{"UPSTREAM_BACKEND unset (default)", ""},
		{"UPSTREAM_BACKEND=ollama (explicit)", "ollama"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate from the surrounding environment so this assertion is
			// deterministic regardless of what the test process's real
			// environment happens to have set.
			withEnv(t, "UPSTREAM_BACKEND", tc.upstreamBackendEnv)
			withEnv(t, "OLLAMA_URL", "")
			withEnv(t, "UPSTREAM_URL", "")
			withEnv(t, "UPSTREAM_API_KEY", "")

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("config.Load(): %v", err)
			}
			if cfg.UpstreamBackend != "ollama" {
				t.Fatalf("cfg.UpstreamBackend = %q, want ollama", cfg.UpstreamBackend)
			}
			if cfg.OllamaURL == nil {
				t.Fatal("cfg.OllamaURL = nil, want the default OLLAMA_URL")
			}

			// This is New(cfg), the single production factory
			// (internal/backend/backend.go) — not newOllamaBackend directly
			// — so the assertion below exercises the exact same call site
			// cmd/broker/main.go uses.
			be, err := New(cfg)
			if err != nil {
				t.Fatalf("New(cfg): %v", err)
			}
			ob, ok := be.(*ollamaBackend)
			if !ok {
				t.Fatalf("New(cfg) returned %T, want *ollamaBackend", be)
			}

			got := ob.Proxy()
			gotRP, ok := got.(*httputil.ReverseProxy)
			if !ok {
				t.Fatalf("ollamaBackend.Proxy() returned %T, want *httputil.ReverseProxy (FR-22: no dispatch/translation layer may be interposed on the ollama path)", got)
			}

			// Reference construction: exactly what the pre-feature call site,
			// proxy.New(cfg.OllamaURL), produces — built independently here
			// (a second, fresh call) so this isn't just comparing
			// ob.Proxy() against itself.
			wantHandler := proxy.New(cfg.OllamaURL)
			wantRP, ok := wantHandler.(*httputil.ReverseProxy)
			if !ok {
				t.Fatalf("proxy.New() returned %T, want *httputil.ReverseProxy", wantHandler)
			}

			if reflect.TypeOf(got) != reflect.TypeOf(wantHandler) {
				t.Fatalf("reflect.TypeOf(ollamaBackend.Proxy()) = %v, want %v (same concrete type as proxy.New())", reflect.TypeOf(got), reflect.TypeOf(wantHandler))
			}

			// Transport identity: ollamaBackend.Proxy() must use the exact
			// same shared proxy.Transport instance proxy.New() always wires
			// in — pointer equality, not merely an equal-looking value,
			// since Transport is a single package-level var by design
			// (internal/proxy/proxy.go's connection-retry transport). A
			// wrapping/dispatch layer that built its own transport would
			// fail this check even if it otherwise behaved identically.
			if gotRP.Transport != proxy.Transport {
				t.Fatalf("ollamaBackend.Proxy()'s Transport != the shared proxy.Transport package var; a dispatch layer may have substituted its own transport")
			}
			if gotRP.Transport != wantRP.Transport {
				t.Fatalf("ollamaBackend.Proxy()'s Transport = %v, want the identical instance proxy.New() uses (%v)", gotRP.Transport, wantRP.Transport)
			}

			// FlushInterval:-1 is what makes streaming NDJSON relay
			// unbuffered (AC-4); a reconstructed ReverseProxy that omitted
			// this setting would silently reintroduce buffering.
			if gotRP.FlushInterval != wantRP.FlushInterval {
				t.Fatalf("ollamaBackend.Proxy().FlushInterval = %v, want %v (matching proxy.New())", gotRP.FlushInterval, wantRP.FlushInterval)
			}

			if gotRP.Rewrite == nil {
				t.Fatal("ollamaBackend.Proxy().Rewrite is nil")
			}
			if gotRP.ErrorHandler == nil {
				t.Fatal("ollamaBackend.Proxy().ErrorHandler is nil")
			}
		})
	}
}
