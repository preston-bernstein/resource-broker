package config

import (
	"fmt"
	"testing"
	"time"
)

func TestLoadCPUModels(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_CPU_MODELS", " bge-m3-cpu , ,nomic-embed-cpu:latest")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"bge-m3-cpu", "nomic-embed-cpu:latest"}
	if fmt.Sprint(cfg.CPUModels) != fmt.Sprint(want) {
		t.Errorf("CPUModels = %q, want %q (trimmed, empties dropped)", cfg.CPUModels, want)
	}

	t.Setenv("BROKER_CPU_MODELS", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CPUModels != nil {
		t.Errorf("CPUModels = %q, want nil when unset (bypass off)", cfg.CPUModels)
	}
}

// --- Extra yield units (BROKER_YIELD_UNITS, ADR-0019) ---

func TestLoadYieldUnits(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_YIELD_UNITS", " gpu-worker.service , ,other.service")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"gpu-worker.service", "other.service"}
	if fmt.Sprint(cfg.YieldUnits) != fmt.Sprint(want) {
		t.Fatalf("YieldUnits = %v, want %v", cfg.YieldUnits, want)
	}
}

func TestLoadYieldUnitsUnsetIsOff(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.YieldUnits != nil {
		t.Fatalf("YieldUnits = %v, want nil", cfg.YieldUnits)
	}
}

func TestLoadYieldUnitsRejectsAUnitWithTwoControllers(t *testing.T) {
	cases := map[string]map[string]string{
		"repeated in the list": {
			"OLLAMA_URL":         "http://127.0.0.1:11434",
			"BROKER_YIELD_UNITS": "a.service,a.service",
		},
		"same as UPSTREAM_UNIT_NAME": {
			"UPSTREAM_BACKEND":   "openai",
			"UPSTREAM_URL":       "http://127.0.0.1:8000",
			"UPSTREAM_UNIT_NAME": "vllm.service",
			"BROKER_YIELD_UNITS": "vllm.service",
		},
		"same as a route unit": {
			"OLLAMA_URL":               "http://127.0.0.1:11434",
			"BROKER_ROUTE_1_MODELS":    "qwen",
			"BROKER_ROUTE_1_URL":       "http://192.0.2.10:8000",
			"BROKER_ROUTE_1_UNIT_NAME": "vllm.service",
			"BROKER_YIELD_UNITS":       "vllm.service",
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected an error for a unit with two controllers")
			}
		})
	}
}

// --- Route displacement (BROKER_ROUTE_<N>_DISPLACES, ADR-0020) ---

func displaceEnv() map[string]string {
	return map[string]string{
		"OLLAMA_URL":                  "http://127.0.0.1:11434",
		"BROKER_ROUTE_1_MODELS":       "qwen2.5",
		"BROKER_ROUTE_1_URL":          "http://127.0.0.1:8000",
		"BROKER_ROUTE_1_UNIT_NAME":    "vllm",
		"BROKER_ROUTE_1_IDLE_TIMEOUT": "60m",
		"BROKER_ROUTE_1_DISPLACES":    " gpu-worker@a.service ",
		"BROKER_YIELD_UNITS":          "gpu-worker@a.service",
	}
}

func TestLoadRouteDisplaces(t *testing.T) {
	for k, v := range displaceEnv() {
		t.Setenv(k, v)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := fmt.Sprint(cfg.Routes[0].Displaces); got != "[gpu-worker@a.service]" {
		t.Fatalf("Displaces = %s", got)
	}
}

func TestLoadRouteDisplacesRejectsBadConfig(t *testing.T) {
	cases := map[string]map[string]string{
		"no idle timeout":       {"BROKER_ROUTE_1_IDLE_TIMEOUT": ""},
		"not a yield unit":      {"BROKER_ROUTE_1_DISPLACES": "other.service"},
		"listed twice":          {"BROKER_ROUTE_1_DISPLACES": "gpu-worker@a.service,gpu-worker@a.service"},
		"no yield units at all": {"BROKER_YIELD_UNITS": ""},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			env := displaceEnv()
			for k, v := range override {
				env[k] = v
			}
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// --- Displaced-unit resume hold-off (BROKER_YIELD_UNIT_RESUME_HOLDOFF, ADR-0021) ---

func TestLoadUnitResumeHoldoff(t *testing.T) {
	cases := map[string]time.Duration{"": 0, "5m": 5 * time.Minute}
	for v, want := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
			t.Setenv("BROKER_YIELD_UNIT_RESUME_HOLDOFF", v)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.UnitResumeHoldoff != want {
				t.Fatalf("UnitResumeHoldoff = %v, want %v", cfg.UnitResumeHoldoff, want)
			}
		})
	}
}

func TestLoadUnitResumeHoldoffRejectsBadValues(t *testing.T) {
	for _, v := range []string{"-1m", "soon"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
			t.Setenv("BROKER_YIELD_UNIT_RESUME_HOLDOFF", v)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted BROKER_YIELD_UNIT_RESUME_HOLDOFF=%q", v)
			}
		})
	}
}
