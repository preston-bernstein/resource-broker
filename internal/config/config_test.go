package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// Empty values resolve to defaults.
	t.Setenv("OLLAMA_URL", "")
	t.Setenv("BROKER_INTERACTIVE_ADDR", "")
	t.Setenv("BROKER_BATCH_ADDR", "")
	t.Setenv("BROKER_INTERACTIVE_WAIT", "")
	t.Setenv("BROKER_BATCH_WAIT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InteractiveAddr != ":11435" {
		t.Errorf("InteractiveAddr = %q", cfg.InteractiveAddr)
	}
	if cfg.BatchAddr != ":11436" {
		t.Errorf("BatchAddr = %q", cfg.BatchAddr)
	}
	if cfg.OllamaURL.String() != "http://127.0.0.1:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL.String())
	}
	if cfg.InteractiveWait != 30*time.Second {
		t.Errorf("InteractiveWait = %v", cfg.InteractiveWait)
	}
	if cfg.BatchWait != 5*time.Second {
		t.Errorf("BatchWait = %v", cfg.BatchWait)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://desktop.example.internal:11434")
	t.Setenv("BROKER_INTERACTIVE_WAIT", "45s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OllamaURL.Host != "desktop.example.internal:11434" {
		t.Errorf("OllamaURL.Host = %q", cfg.OllamaURL.Host)
	}
	if cfg.InteractiveWait != 45*time.Second {
		t.Errorf("InteractiveWait = %v", cfg.InteractiveWait)
	}
}

func TestLoadInvalidURL(t *testing.T) {
	t.Setenv("OLLAMA_URL", "not-a-url")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for URL without scheme/host")
	}
}

func TestLoadInvalidDuration(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_BATCH_WAIT", "soon")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for bad duration")
	}
}
