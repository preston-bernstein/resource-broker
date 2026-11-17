package config

import (
	"fmt"
	"slices"
	"testing"
)

// --- Per-model route configuration (BROKER_ROUTE_<N>_*) ---

func TestLoadRoutesThreeRoutes(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")

	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen,llama")
	t.Setenv("BROKER_ROUTE_1_BACKEND", "openai")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_LANE", "interactive")

	t.Setenv("BROKER_ROUTE_2_MODELS", "mistral")
	t.Setenv("BROKER_ROUTE_2_BACKEND", "ollama")
	t.Setenv("BROKER_ROUTE_2_URL", "http://192.0.2.11:11434")
	t.Setenv("BROKER_ROUTE_2_LANE", "batch")

	t.Setenv("BROKER_ROUTE_3_MODELS", "phi")
	t.Setenv("BROKER_ROUTE_3_URL", "http://192.0.2.12:9000")
	// BROKER_ROUTE_3_LANE unset: applies to both lanes.

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 3 {
		t.Fatalf("len(Routes) = %d, want 3", len(cfg.Routes))
	}

	requireRoute(t, cfg.Routes[0], wantRoute{models: []string{"qwen", "llama"}, backend: "openai", url: "http://192.0.2.10:8000", lane: "interactive"})
	requireRoute(t, cfg.Routes[1], wantRoute{models: []string{"mistral"}, backend: "ollama", url: "http://192.0.2.11:11434", lane: "batch"})
	requireRoute(t, cfg.Routes[2], wantRoute{models: []string{"phi"}, backend: "openai", url: "http://192.0.2.12:9000", lane: ""})
}

type wantRoute struct {
	models  []string
	backend string
	url     string
	lane    string
}

// requireRoute checks the fields TestLoadRoutesThreeRoutes sets. A route with
// no BROKER_ROUTE_<N>_BACKEND defaults to openai, and one with no LANE applies
// to both lanes (empty).
func requireRoute(t *testing.T, got RouteBackend, want wantRoute) {
	t.Helper()
	if !slices.Equal(got.Models, want.models) {
		t.Errorf("Models = %v, want %v", got.Models, want.models)
	}
	if got.Backend != want.backend {
		t.Errorf("Backend = %q, want %q", got.Backend, want.backend)
	}
	if got.URL == nil || got.URL.String() != want.url {
		t.Errorf("URL = %v, want %s", got.URL, want.url)
	}
	if got.Lane != want.lane {
		t.Errorf("Lane = %q, want %q", got.Lane, want.lane)
	}
}

func TestLoadRoutesUnsetDisablesRouting(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 0 {
		t.Errorf("len(Routes) = %d, want 0 when BROKER_ROUTE_1_MODELS unset", len(cfg.Routes))
	}
}

func TestLoadRouteInvalidURL(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "not-a-url")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for malformed BROKER_ROUTE_1_URL")
	}
}

func TestLoadRouteEmptyModelName(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen,,llama")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for empty model name in BROKER_ROUTE_1_MODELS")
	}
}

func TestLoadRouteDuplicateModelSameList(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen,qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for duplicate model name within one route's MODELS list")
	}
}

func TestLoadRouteDuplicateModelAcrossRoutes(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_2_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_2_URL", "http://192.0.2.11:8000")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for the same model name routed by two different routes")
	}
}

func TestLoadRouteInvalidLane(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_LANE", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for BROKER_ROUTE_1_LANE not in \"\"/interactive/batch")
	}
}

func TestLoadRouteIndexGap(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	// index 2 deliberately left unset.
	t.Setenv("BROKER_ROUTE_3_MODELS", "phi")
	t.Setenv("BROKER_ROUTE_3_URL", "http://192.0.2.12:9000")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for an index gap (route 2 unset while route 3 configured)")
	}
}

func TestLoadRouteDuplicateUnitNameAcrossRoutes(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_UNIT_NAME", "vllm-shared")
	t.Setenv("BROKER_ROUTE_2_MODELS", "llama")
	t.Setenv("BROKER_ROUTE_2_URL", "http://192.0.2.11:8000")
	t.Setenv("BROKER_ROUTE_2_UNIT_NAME", "vllm-shared")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for two routes sharing the same resolved _UNIT_NAME")
	}
}

func TestLoadRouteDuplicateURLAcrossRoutes(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_2_MODELS", "llama")
	t.Setenv("BROKER_ROUTE_2_URL", "http://192.0.2.10:8000")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for two routes sharing the same resolved URL")
	}
}

func TestLoadRouteDuplicatesDefaultBackendURL(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://127.0.0.1:11434")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for a route URL duplicating the default backend's URL")
	}
}

func TestLoadRouteDuplicatesDefaultBackendUnitName(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_UNIT_NAME", "vllm")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_UNIT_NAME", "vllm")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for a route UNIT_NAME duplicating the default backend's UPSTREAM_UNIT_NAME")
	}
}

func TestLoadRouteAPIKeyControlCharRejected(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_API_KEY", "sekret\r\nX-Injected: true")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for BROKER_ROUTE_1_API_KEY containing CR/LF")
	}
}

func TestLoadRouteTooManyRoutes(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	for i := 1; i <= 17; i++ {
		idx := i
		t.Setenv(fmt.Sprintf("BROKER_ROUTE_%d_MODELS", idx), fmt.Sprintf("model%d", idx))
		t.Setenv(fmt.Sprintf("BROKER_ROUTE_%d_URL", idx), fmt.Sprintf("http://10.0.1.%d:8000", idx))
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected error for more than 16 configured routes")
	}
}

func TestLoadRouteBackendInvalid(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_BACKEND", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for BROKER_ROUTE_1_BACKEND=bogus")
	}
}
