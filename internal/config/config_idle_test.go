package config

import (
	"testing"
	"time"
)

// --- Idle-unload timeout (UPSTREAM_IDLE_TIMEOUT / BROKER_ROUTE_<N>_IDLE_TIMEOUT) ---

func TestLoadUpstreamIdleTimeoutValid(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_UNIT_NAME", "vllm")
	t.Setenv("UPSTREAM_IDLE_TIMEOUT", "1h30m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamIdleTimeout != 90*time.Minute {
		t.Errorf("UpstreamIdleTimeout = %v, want 1h30m", cfg.UpstreamIdleTimeout)
	}
}

// TestLoadUpstreamIdleTimeoutRequiresUnitName asserts a nonzero
// UPSTREAM_IDLE_TIMEOUT without UPSTREAM_UNIT_NAME set is a config error: an
// idle-unload timeout only means anything alongside a systemd unit to stop.
func TestLoadUpstreamIdleTimeoutRequiresUnitName(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_UNIT_NAME", "")
	t.Setenv("UPSTREAM_IDLE_TIMEOUT", "5m")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for UPSTREAM_IDLE_TIMEOUT set without UPSTREAM_UNIT_NAME")
	}
}

func TestLoadUpstreamIdleTimeoutNegative(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_UNIT_NAME", "vllm")
	t.Setenv("UPSTREAM_IDLE_TIMEOUT", "-5m")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for negative UPSTREAM_IDLE_TIMEOUT")
	}
}

// TestLoadUpstreamIdleTimeoutZeroDisabled asserts an explicit "0" is treated
// identically to unset: it disables idle-unload and does not require
// UPSTREAM_UNIT_NAME to be set.
func TestLoadUpstreamIdleTimeoutZeroDisabled(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_UNIT_NAME", "")
	t.Setenv("UPSTREAM_IDLE_TIMEOUT", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v (UPSTREAM_IDLE_TIMEOUT=0 must not require UPSTREAM_UNIT_NAME)", err)
	}
	if cfg.UpstreamIdleTimeout != 0 {
		t.Errorf("UpstreamIdleTimeout = %v, want 0", cfg.UpstreamIdleTimeout)
	}
}

// TestLoadUpstreamIdleTimeoutUnsetDisabled asserts both vars left unset load
// successfully with idle-unload disabled, leaving the rest of Config
// unchanged from the pre-existing shape.
func TestLoadUpstreamIdleTimeoutUnsetDisabled(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_UNIT_NAME", "")
	t.Setenv("UPSTREAM_IDLE_TIMEOUT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamIdleTimeout != 0 {
		t.Errorf("UpstreamIdleTimeout = %v, want 0 (disabled)", cfg.UpstreamIdleTimeout)
	}
	if cfg.UpstreamUnitName != "" {
		t.Errorf("UpstreamUnitName = %q, want empty", cfg.UpstreamUnitName)
	}
}

func TestLoadUpstreamIdleTimeoutInvalidDuration(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_UNIT_NAME", "vllm")
	t.Setenv("UPSTREAM_IDLE_TIMEOUT", "soon")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unparseable UPSTREAM_IDLE_TIMEOUT")
	}
}

func TestLoadRouteIdleTimeoutValid(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_UNIT_NAME", "vllm-qwen")
	t.Setenv("BROKER_ROUTE_1_IDLE_TIMEOUT", "10m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 1 {
		t.Fatalf("len(Routes) = %d, want 1", len(cfg.Routes))
	}
	if cfg.Routes[0].IdleTimeout != 10*time.Minute {
		t.Errorf("Routes[0].IdleTimeout = %v, want 10m", cfg.Routes[0].IdleTimeout)
	}
}

// TestLoadRouteIdleTimeoutRequiresUnitName mirrors
// TestLoadUpstreamIdleTimeoutRequiresUnitName for a per-route rule.
func TestLoadRouteIdleTimeoutRequiresUnitName(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_IDLE_TIMEOUT", "10m")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for BROKER_ROUTE_1_IDLE_TIMEOUT set without BROKER_ROUTE_1_UNIT_NAME")
	}
}

func TestLoadRouteIdleTimeoutNegative(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_UNIT_NAME", "vllm-qwen")
	t.Setenv("BROKER_ROUTE_1_IDLE_TIMEOUT", "-10m")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for negative BROKER_ROUTE_1_IDLE_TIMEOUT")
	}
}

func TestLoadRouteIdleTimeoutZeroDisabled(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_ROUTE_1_MODELS", "qwen")
	t.Setenv("BROKER_ROUTE_1_URL", "http://192.0.2.10:8000")
	t.Setenv("BROKER_ROUTE_1_IDLE_TIMEOUT", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v (BROKER_ROUTE_1_IDLE_TIMEOUT=0 must not require BROKER_ROUTE_1_UNIT_NAME)", err)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].IdleTimeout != 0 {
		t.Errorf("Routes[0].IdleTimeout = %v, want 0", cfg.Routes[0].IdleTimeout)
	}
}
