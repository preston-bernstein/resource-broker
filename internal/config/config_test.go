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
	t.Setenv("BROKER_MAX_WAITERS", "")

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
	if cfg.MaxWaiters != 256 {
		t.Errorf("MaxWaiters = %d", cfg.MaxWaiters)
	}
	if cfg.EmbedAddr != ":11438" {
		t.Errorf("EmbedAddr = %q", cfg.EmbedAddr)
	}
	if cfg.InfinityURL != nil {
		t.Errorf("InfinityURL = %v, want nil when INFINITY_URL unset", cfg.InfinityURL)
	}
	if cfg.DetectInterval != 3*time.Second {
		t.Errorf("DetectInterval = %v, want 3s", cfg.DetectInterval)
	}
	if cfg.BatchQuantum != 10*time.Second {
		t.Errorf("BatchQuantum = %v, want 10s", cfg.BatchQuantum)
	}
	if cfg.JobPruneInterval != 10*time.Minute {
		t.Errorf("JobPruneInterval = %v, want 10m", cfg.JobPruneInterval)
	}
	if cfg.JobHardCap != 7*24*time.Hour {
		t.Errorf("JobHardCap = %v, want 168h", cfg.JobHardCap)
	}
}

// TestLoadIntBoundaryMinimumAccepted proves getint's `n < 1` check accepts
// exactly 1 — the boundary itself, which TestLoadInvalidMaxWaiters (n=0,
// rejected) does not exercise, leaving a `<` vs `<=` mutation undetected.
func TestLoadIntBoundaryMinimumAccepted(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_MAX_WAITERS", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxWaiters != 1 {
		t.Errorf("MaxWaiters = %d, want 1", cfg.MaxWaiters)
	}
}

func TestLoadInfinityURL(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("INFINITY_URL", "http://127.0.0.1:7997")
	t.Setenv("BROKER_EMBED_ADDR", ":9999")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InfinityURL == nil || cfg.InfinityURL.Host != "127.0.0.1:7997" {
		t.Errorf("InfinityURL = %v, want host 127.0.0.1:7997", cfg.InfinityURL)
	}
	if cfg.EmbedAddr != ":9999" {
		t.Errorf("EmbedAddr = %q", cfg.EmbedAddr)
	}
}

func TestLoadInvalidInfinityURL(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("INFINITY_URL", "not-a-url")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for INFINITY_URL without scheme/host")
	}
}

func TestLoadInvalidMaxWaiters(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_MAX_WAITERS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for MaxWaiters < 1")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://10.0.0.243:11434")
	t.Setenv("BROKER_INTERACTIVE_WAIT", "45s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OllamaURL.Host != "10.0.0.243:11434" {
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

// --- Upstream backend selection (UPSTREAM_BACKEND / UPSTREAM_URL / UPSTREAM_API_KEY) ---

func TestLoadUpstreamBackendInvalid(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_BACKEND", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for UPSTREAM_BACKEND=bogus")
	}
}

func TestLoadUpstreamBackendOpenAIWithoutUpstreamURL(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "openai")
	t.Setenv("UPSTREAM_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for UPSTREAM_BACKEND=openai without UPSTREAM_URL")
	}
}

func TestLoadUpstreamBackendOpenAIWithValidUpstreamURL(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "openai")
	t.Setenv("UPSTREAM_URL", "http://127.0.0.1:8000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamBackend != "openai" {
		t.Errorf("UpstreamBackend = %q, want openai", cfg.UpstreamBackend)
	}
	if cfg.UpstreamURL == nil || cfg.UpstreamURL.String() != "http://127.0.0.1:8000" {
		t.Errorf("UpstreamURL = %v, want http://127.0.0.1:8000", cfg.UpstreamURL)
	}
	if cfg.OllamaURL != nil {
		t.Errorf("OllamaURL = %v, want nil when UPSTREAM_BACKEND=openai", cfg.OllamaURL)
	}
}

// TestLoadUpstreamBackendOllamaWithoutOllamaURL pins a NEW failure case
// introduced by making OLLAMA_URL's requirement conditional on
// UPSTREAM_BACKEND=ollama (FR-23): a truly malformed OLLAMA_URL still fails
// when the backend is (explicitly or by default) "ollama".
func TestLoadUpstreamBackendOllamaWithoutOllamaURL(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "ollama")
	t.Setenv("OLLAMA_URL", "not-a-url")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for UPSTREAM_BACKEND=ollama with malformed OLLAMA_URL")
	}
}

// TestLoadUpstreamBackendDefaultOllamaEmptyURLResolvesToDefault confirms the
// pre-existing empty-string-resolves-to-default behavior for OLLAMA_URL is
// unchanged when UPSTREAM_BACKEND is left unset (defaulting to "ollama"):
// an empty OLLAMA_URL must still resolve to the default and load
// successfully, not fail, exactly as TestLoadDefaults already asserts.
func TestLoadUpstreamBackendDefaultOllamaEmptyURLResolvesToDefault(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "")
	t.Setenv("OLLAMA_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamBackend != "ollama" {
		t.Errorf("UpstreamBackend = %q, want default ollama", cfg.UpstreamBackend)
	}
	if cfg.OllamaURL == nil || cfg.OllamaURL.String() != "http://127.0.0.1:11434" {
		t.Errorf("OllamaURL = %v, want default http://127.0.0.1:11434", cfg.OllamaURL)
	}
}

func TestLoadUpstreamAPIKeyControlCharRejected(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_API_KEY", "sekret\r\nX-Injected: true")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for UPSTREAM_API_KEY containing CR/LF")
	}
}

func TestLoadUpstreamAPIKeyValid(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("UPSTREAM_API_KEY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamAPIKey != "" {
		t.Errorf("UpstreamAPIKey = %q, want empty when unset", cfg.UpstreamAPIKey)
	}

	t.Setenv("UPSTREAM_API_KEY", "sk-valid-key")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpstreamAPIKey != "sk-valid-key" {
		t.Errorf("UpstreamAPIKey = %q, want sk-valid-key", cfg.UpstreamAPIKey)
	}
}

// --- Park config (BROKER_PARK_HOLD / BROKER_PARK_MAX_QUEUE / BROKER_PARK_DRAIN_BURST) ---

func TestLoadParkDefaults(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_PARK_HOLD", "")
	t.Setenv("BROKER_PARK_MAX_QUEUE", "")
	t.Setenv("BROKER_PARK_DRAIN_BURST", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ParkHold != 600*time.Second {
		t.Errorf("ParkHold = %v, want 600s", cfg.ParkHold)
	}
	if cfg.ParkMaxQueue != 32 {
		t.Errorf("ParkMaxQueue = %d, want 32", cfg.ParkMaxQueue)
	}
	if cfg.ParkDrainBurst != 8 {
		t.Errorf("ParkDrainBurst = %d, want 8", cfg.ParkDrainBurst)
	}
}

func TestLoadParkOverrides(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_PARK_HOLD", "120s")
	t.Setenv("BROKER_PARK_MAX_QUEUE", "10")
	t.Setenv("BROKER_PARK_DRAIN_BURST", "3")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ParkHold != 120*time.Second {
		t.Errorf("ParkHold = %v, want 120s", cfg.ParkHold)
	}
	if cfg.ParkMaxQueue != 10 {
		t.Errorf("ParkMaxQueue = %d, want 10", cfg.ParkMaxQueue)
	}
	if cfg.ParkDrainBurst != 3 {
		t.Errorf("ParkDrainBurst = %d, want 3", cfg.ParkDrainBurst)
	}
}

// TestLoadParkMaxQueueZero pins the documented kill-switch: 0 must load
// successfully (not be rejected like getint's "< 1" rule), because
// BROKER_PARK_MAX_QUEUE=0 is the operator's first-line rollback if parking
// misbehaves — see ADR-0009 point 4 and plan.md's SetParkConfig contract.
func TestLoadParkMaxQueueZero(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_PARK_MAX_QUEUE", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ParkMaxQueue != 0 {
		t.Errorf("ParkMaxQueue = %d, want 0 (kill-switch)", cfg.ParkMaxQueue)
	}
}

// TestLoadParkMaxQueueNegative asserts a negative value is a config error,
// not a disable request: it is ignored (falls back to the default) with a
// warning logged, rather than crashing Load() or being silently treated as
// the 0 kill-switch (which would hide a typo behind seemingly-correct
// behavior).
func TestLoadParkMaxQueueNegative(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_PARK_MAX_QUEUE", "-5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v (must not crash on negative ParkMaxQueue)", err)
	}
	if cfg.ParkMaxQueue != 32 {
		t.Errorf("ParkMaxQueue = %d, want default 32 after negative override", cfg.ParkMaxQueue)
	}
}

func TestLoadParkMaxQueueGarbage(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_PARK_MAX_QUEUE", "not-a-number")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v (must not crash on garbage ParkMaxQueue)", err)
	}
	if cfg.ParkMaxQueue != 32 {
		t.Errorf("ParkMaxQueue = %d, want default 32 after garbage override", cfg.ParkMaxQueue)
	}
}

// TestLoadParkDrainBurstInvalid asserts values < 1 (including 0 and
// negative) are ignored in favor of the default — unlike ParkMaxQueue, 0 has
// no meaningful "disabled" interpretation for a drain burst size.
func TestLoadParkDrainBurstInvalid(t *testing.T) {
	for _, v := range []string{"0", "-1", "garbage"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
			t.Setenv("BROKER_PARK_DRAIN_BURST", v)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v (must not crash on invalid ParkDrainBurst %q)", err, v)
			}
			if cfg.ParkDrainBurst != 8 {
				t.Errorf("ParkDrainBurst = %d, want default 8 for input %q", cfg.ParkDrainBurst, v)
			}
		})
	}
}

// TestLoadParkHoldInvalid asserts a zero/negative/unparseable ParkHold is
// ignored in favor of the default rather than crashing Load() or silently
// producing an instant-expire (0 or negative) hold bound — see plan.md's
// note that hold<=0 must never be applied silently.
func TestLoadParkHoldInvalid(t *testing.T) {
	for _, v := range []string{"0s", "-5s", "soon"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
			t.Setenv("BROKER_PARK_HOLD", v)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v (must not crash on invalid ParkHold %q)", err, v)
			}
			if cfg.ParkHold != 600*time.Second {
				t.Errorf("ParkHold = %v, want default 600s for input %q", cfg.ParkHold, v)
			}
		})
	}
}

// --- Embed lane upstream timeout (BROKER_EMBED_TIMEOUT, ADR-0013) ---

func TestLoadEmbedTimeoutDefault(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_EMBED_TIMEOUT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbedTimeout != 30*time.Second {
		t.Errorf("EmbedTimeout = %v, want default 30s", cfg.EmbedTimeout)
	}
}

func TestLoadEmbedTimeoutOverride(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_EMBED_TIMEOUT", "10s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbedTimeout != 10*time.Second {
		t.Errorf("EmbedTimeout = %v, want 10s", cfg.EmbedTimeout)
	}
}

func TestLoadEmbedTimeoutInvalid(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_EMBED_TIMEOUT", "soon")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unparseable BROKER_EMBED_TIMEOUT")
	}
}

// --- Plex session corroboration (PLEX_URL / PLEX_TOKEN) and yield debounce (BROKER_YIELD_CONFIRM_POLLS) ---

func TestLoadYieldConfirmPollsDefault(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_YIELD_CONFIRM_POLLS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.YieldConfirmPolls != 2 {
		t.Errorf("YieldConfirmPolls = %d, want default 2", cfg.YieldConfirmPolls)
	}
}

func TestLoadYieldConfirmPollsOverride(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_YIELD_CONFIRM_POLLS", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.YieldConfirmPolls != 5 {
		t.Errorf("YieldConfirmPolls = %d, want 5", cfg.YieldConfirmPolls)
	}
}

func TestLoadYieldConfirmPollsInvalid(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_YIELD_CONFIRM_POLLS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for YieldConfirmPolls < 1 (getint rejects, unlike getintMin-backed park vars)")
	}
}

func TestLoadPlexDefaults(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("PLEX_URL", "")
	t.Setenv("PLEX_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PlexURL != "http://localhost:32400" {
		t.Errorf("PlexURL = %q, want default http://localhost:32400", cfg.PlexURL)
	}
	if cfg.PlexToken != "" {
		t.Errorf("PlexToken = %q, want empty (corroboration disabled) by default", cfg.PlexToken)
	}
}

func TestLoadPlexOverrides(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("PLEX_URL", "http://10.0.0.243:32400")
	t.Setenv("PLEX_TOKEN", "secret-token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PlexURL != "http://10.0.0.243:32400" {
		t.Errorf("PlexURL = %q", cfg.PlexURL)
	}
	if cfg.PlexToken != "secret-token" {
		t.Errorf("PlexToken = %q", cfg.PlexToken)
	}
}

// TestParkHoldBatchWaitBudget is the NFR-2 headroom guard: BROKER_PARK_HOLD
// plus BROKER_BATCH_WAIT is a wait-time-only budget that must leave
// comfortable margin under LightRAG's 1200s wrapping EMBEDDING_TIMEOUT (the
// serve-plus-retry-transport budget is what's left over, and it is not
// slack — see plan.md's "Config surface" table). This asserts the two
// loaded defaults together stay under 900s, so a future default bump to
// either var is caught here instead of silently eating into that serve
// budget.
func TestParkHoldBatchWaitBudget(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
	t.Setenv("BROKER_PARK_HOLD", "")
	t.Setenv("BROKER_BATCH_WAIT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	budget := cfg.ParkHold + cfg.BatchWait
	if budget >= 900*time.Second {
		t.Errorf("ParkHold(%v) + BatchWait(%v) = %v, want < 900s (LightRAG 1200s headroom guard)",
			cfg.ParkHold, cfg.BatchWait, budget)
	}
}

// --- Upstream unit name (UPSTREAM_UNIT_NAME) ---

// TestLoadUpstreamUnitName covers three cases for UPSTREAM_UNIT_NAME: unset
// (empty), whitespace-only (trimmed to empty), and set with surrounding
// whitespace (trimmed to the value). TrimSpace is applied to all values.
func TestLoadUpstreamUnitName(t *testing.T) {
	for name, env := range map[string]string{
		"unset":            "",
		"whitespace-only":  "   ",
		"with-surrounding": "  vllm  ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OLLAMA_URL", "http://127.0.0.1:11434")
			t.Setenv("UPSTREAM_UNIT_NAME", env)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			var want string
			switch name {
			case "unset", "whitespace-only":
				want = ""
			case "with-surrounding":
				want = "vllm"
			}

			if cfg.UpstreamUnitName != want {
				t.Errorf("UpstreamUnitName = %q, want %q (input: %q)", cfg.UpstreamUnitName, want, env)
			}
		})
	}
}
