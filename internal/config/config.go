// Package config loads broker configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// RouteBackend is one per-model routing rule, parsed from a single indexed
// BROKER_ROUTE_<N>_* group: requests for any model listed in Models are
// proxied to this instance instead of the default OllamaURL/UpstreamURL.
type RouteBackend struct {
	// Models is the set of model names routed to this instance. A model name
	// may appear in at most one RouteBackend across the whole Config.Routes
	// slice (and at most once within this instance's own list).
	Models []string
	// Backend selects the upstream API family for this instance: "ollama" or
	// "openai" — same values and meaning as Config.UpstreamBackend. Defaults
	// to "openai" (note: a different default than UpstreamBackend, which
	// defaults to "ollama").
	Backend string
	// URL is this instance's base URL, validated for scheme+host the same
	// way as OllamaURL/UpstreamURL.
	URL *url.URL
	// APIKey optionally authenticates requests to this instance (openai
	// family only), sent as "Authorization: Bearer <value>". Never log this
	// value raw.
	APIKey string
	// UnitName is the systemd unit name to stop/start on yield-start/yield-
	// clear for this instance only. Empty disables the Unloader for this
	// instance, the same precedent as Config.UpstreamUnitName.
	UnitName string
	// IdleTimeout is how long this instance may sit idle before its systemd
	// unit is stopped to free resources. Zero (unset or explicitly "0")
	// disables idle-unload for this instance. Requires UnitName to be set —
	// Load() rejects a nonzero value with an empty UnitName, the same
	// precedent as Config.UpstreamIdleTimeout.
	IdleTimeout time.Duration
	// Lane optionally scopes this rule to a single lane: "interactive" or
	// "batch". Empty (the default) means the rule applies to both lanes.
	Lane string
	// Displaces lists BROKER_YIELD_UNITS entries that give this instance the
	// GPU (BROKER_ROUTE_<N>_DISPLACES, comma separated; ADR-0020): they run
	// only while this instance is idle-unloaded. Requires IdleTimeout, so the
	// instance always hands the GPU back.
	Displaces []string
}

// Config holds all broker runtime settings.
type Config struct {
	// InteractiveAddr is the listen address for the high-priority class.
	InteractiveAddr string
	// BatchAddr is the listen address for the low-priority class.
	BatchAddr string
	// UpstreamBackend selects which upstream API family the Broker fronts:
	// "ollama" (default) or "openai" (an OpenAI-compatible server such as
	// vLLM). Determines whether OllamaURL or UpstreamURL is required/
	// validated at Load() time.
	UpstreamBackend string
	// OllamaURL is the upstream real Ollama base URL. Required and validated
	// (scheme+host) only when UpstreamBackend is "ollama"; nil when
	// UpstreamBackend is "openai".
	OllamaURL *url.URL
	// UpstreamURL is the OpenAI-compatible upstream base URL (e.g. a vLLM
	// server). Required and validated (scheme+host, same pattern as
	// OllamaURL) only when UpstreamBackend is "openai"; nil when
	// UpstreamBackend is "ollama".
	UpstreamURL *url.URL
	// UpstreamAPIKey optionally authenticates requests to the OpenAI-
	// compatible upstream (sent as "Authorization: Bearer <value>"; empty
	// means no Authorization header is sent). Never log this value raw.
	UpstreamAPIKey string
	// UpstreamUnitName is the systemd unit name to stop/start on yield-start/
	// yield-clear when set. Used only by the openai backend. Empty, unset, or
	// whitespace-only (trimmed to "") disables the Unloader entirely.
	UpstreamUnitName string
	// UpstreamIdleTimeout is how long the default backend may sit idle
	// before its systemd unit is stopped to free GPU/resources. Zero (unset
	// or explicitly "0") disables idle-unload entirely. Requires
	// UpstreamUnitName to be set — Load() rejects a nonzero value with an
	// empty UpstreamUnitName.
	UpstreamIdleTimeout time.Duration
	// Routes holds per-model routing rules parsed from the indexed
	// BROKER_ROUTE_<N>_* env vars (N = 1..32). A request for a model listed
	// in a RouteBackend.Models is proxied to that instance instead of the
	// default OllamaURL/UpstreamURL. An unset/empty BROKER_ROUTE_1_MODELS
	// disables routing entirely, leaving Routes nil (FR-9/AC-5).
	Routes []RouteBackend
	// InfinityURL is the upstream Infinity image-embedding base URL. Empty
	// disables the embed lane entirely.
	InfinityURL *url.URL
	// EmbedAddr is the listen address for the image-embedding lane (fronts
	// Infinity, gated by yield but on its own scheduler so CPU embedding does
	// not share the GPU concurrency slot).
	EmbedAddr string
	// EmbedTimeout bounds how long the embed lane's own upstream call to
	// Infinity may run once admitted, separate from BatchWait (which only
	// bounds how long a request may queue for the slot). The embed lane's
	// MaxInflight is hardcoded to 1 (cmd/broker/main.go), so a stuck backend
	// call with no bound wedges the lane's single slot forever (ADR-0013).
	// Zero disables the bound; only the embed lane wiring uses this — the
	// Ollama interactive/batch lanes stay unbounded (a legitimate generation
	// can run for minutes and must not be cut off).
	EmbedTimeout time.Duration
	// InteractiveWait is the queue wait budget for interactive requests.
	InteractiveWait time.Duration
	// BatchWait is the queue wait budget for batch requests.
	BatchWait time.Duration
	// ControlAddr is the listen address for the admin/control plane.
	ControlAddr string
	// ControlToken gates POST /control (ADR-0005). Empty means mutations are
	// accepted only from loopback; set means a matching "Bearer <token>"
	// Authorization header is required regardless of source.
	ControlToken string
	// DetectInterval is how often process detection re-evaluates contention.
	DetectInterval time.Duration
	// YieldConfirmPolls is how many consecutive same-reason detections are
	// required before entering yield, filtering single-poll false-positive
	// blips (launcher background housekeeping) without weakening the
	// hard-yield response once contention is confirmed real (ADR-0003).
	// Clearing contention is never debounced. Default 2 (see the 2026-07-15
	// research doc: most observed false positives were single-poll blips).
	YieldConfirmPolls int
	// PlexURL is the local Plex Media Server base URL used to corroborate a
	// "Plex Transcoder" process match against an actual playback session
	// (background maintenance runs the same binary — see internal/plex).
	PlexURL string
	// PlexToken authenticates PlexURL. Empty disables Plex session
	// corroboration entirely: a process-name match alone is then treated as
	// contention, the pre-existing behavior.
	PlexToken string
	// MaxWaiters caps queued requests per class before fast-failing with 503.
	MaxWaiters int
	// MaxInflight caps concurrent requests reaching Ollama (ADR-0004).
	MaxInflight int
	// YieldUnits are extra systemd units the Broker stops on yield-start and
	// starts on yield-clear (BROKER_YIELD_UNITS, comma separated), for GPU
	// workloads that are not inference backends the Broker fronts (ADR-0019).
	// Each must be unique and must not repeat UPSTREAM_UNIT_NAME or any
	// BROKER_ROUTE_<N>_UNIT_NAME. Nil turns this off.
	YieldUnits []string
	// UnitResumeHoldoff is how long a displaced yield unit's GPU must stay
	// free (no Broker yield, every displacer idle-unloaded) before the
	// Broker starts the unit again (BROKER_YIELD_UNIT_RESUME_HOLDOFF,
	// ADR-0021). Zero starts it on the first free tick, as before.
	UnitResumeHoldoff time.Duration
	// CPUModels are models that run on CPU only (BROKER_CPU_MODELS, comma
	// separated). Their requests skip the GPU slot and GPU yields
	// (ADR-0018). Nil turns the bypass off.
	CPUModels []string
	// BatchQuantum is the min-run window before interactive may preempt a Job.
	BatchQuantum time.Duration

	// JobDBPath is the SQLite file backing the durable Job queue (ADR-0007).
	JobDBPath string
	// JobMaxAttempts caps re-runs of a Job before it is FAILED.
	JobMaxAttempts int
	// JobPruneInterval is how often terminal Jobs are swept.
	JobPruneInterval time.Duration
	// JobFetchedGrace is how long a fetched result is retained before pruning.
	JobFetchedGrace time.Duration
	// JobHardCap is the maximum age of any terminal Job before pruning.
	JobHardCap time.Duration

	// TdarrURL is the Tdarr server base URL (e.g. "http://localhost:8265").
	// Empty string disables Tdarr cooperative GPU management.
	TdarrURL string
	// TdarrNodeID is the Tdarr node _id whose GPU workers the broker manages.
	TdarrNodeID string
	// TdarrGPUWorkers is how many transcodegpu workers to restore after yielding.
	TdarrGPUWorkers int

	// ParkHold is the max duration a Batch request may sit parked during a
	// GPU yield before it is released as an expiry (ADR-0009, NFR-1/NFR-2).
	// Must stay comfortably under LightRAG's 1200s EMBEDDING_TIMEOUT.
	ParkHold time.Duration
	// ParkMaxQueue is the hard ceiling on parked Batch requests. 0 is a
	// MEANINGFUL, deliberate value: parking is disabled outright (the
	// documented kill-switch — a Batch request arriving during yield then
	// takes the same immediate-deferRequest path Interactive always has).
	ParkMaxQueue int
	// ParkDrainBurst caps how many parked requests are released per 1s
	// drain-ticker interval, so a full park queue drains gently rather than
	// presenting Ollama with a burst larger than it would tolerate.
	ParkDrainBurst int
}

// Load reads configuration from the environment, applying defaults. Each step
// reads one group of variables into cfg; the first failure wins, so the order
// below is the order errors are reported in.
func Load() (*Config, error) {
	cfg := &Config{}
	steps := []func(*Config) error{
		loadUpstream,
		loadInfinity,
		loadWaitsAndLimits,
		loadPark,
		loadRoutes,
		loadYieldUnits,
		loadPlainSettings,
	}
	for _, step := range steps {
		if err := step(cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// loadUpstream reads the default backend: UPSTREAM_BACKEND, its URL, API key
// and systemd unit.
func loadUpstream(cfg *Config) error {
	// UPSTREAM_BACKEND selects which upstream API family is active. It gates
	// which of OLLAMA_URL / UPSTREAM_URL is required below (FR-3/FR-4/FR-5,
	// FR-23, AC-2/AC-3/AC-16).
	cfg.UpstreamBackend = getenv("UPSTREAM_BACKEND", "ollama")
	if cfg.UpstreamBackend != "ollama" && cfg.UpstreamBackend != "openai" {
		return fmt.Errorf("UPSTREAM_BACKEND %q must be one of: ollama, openai", cfg.UpstreamBackend)
	}

	// OLLAMA_URL is required/validated only when the active backend is
	// "ollama" (the default) — this is a behavior CHANGE from the previous
	// unconditional requirement (FR-23, AC-16). The empty-string-resolves-
	// to-default behavior (via getenv's def parameter) is unchanged: an
	// unset/empty OLLAMA_URL still resolves to "http://127.0.0.1:11434" and
	// loads successfully; only a truly malformed value fails.
	var err error
	if cfg.UpstreamBackend == "ollama" {
		cfg.OllamaURL, err = parseBaseURL("OLLAMA_URL", getenv("OLLAMA_URL", "http://127.0.0.1:11434"))
		if err != nil {
			return err
		}
	}

	// UPSTREAM_URL (the OpenAI-compatible upstream base URL) is required/
	// validated only when the active backend is "openai", using the same
	// scheme+host check as OLLAMA_URL above (FR-5, AC-3).
	if cfg.UpstreamBackend == "openai" {
		cfg.UpstreamURL, err = parseBaseURL("UPSTREAM_URL", getenv("UPSTREAM_URL", ""))
		if err != nil {
			return err
		}
	}

	// UPSTREAM_API_KEY is optional. When set, it must not contain a CR or LF
	// control character, which would allow HTTP header injection into the
	// outbound "Authorization: Bearer <value>" header (FR-24, AC-17).
	cfg.UpstreamAPIKey = getenv("UPSTREAM_API_KEY", "")
	if strings.ContainsAny(cfg.UpstreamAPIKey, "\r\n") {
		return fmt.Errorf("UPSTREAM_API_KEY must not contain control characters")
	}

	// UPSTREAM_UNIT_NAME is read once, here, and reused by the cross-instance
	// unit-name dedup logic and the idle-timeout check — there must be exactly
	// one place this env var is read from.
	cfg.UpstreamUnitName = strings.TrimSpace(getenv("UPSTREAM_UNIT_NAME", ""))
	return nil
}

// loadInfinity reads INFINITY_URL. It is optional; when set it must be a valid
// absolute URL. Empty leaves InfinityURL nil and the embed lane is not started.
func loadInfinity(cfg *Config) error {
	raw := getenv("INFINITY_URL", "")
	if raw == "" {
		return nil
	}
	u, err := parseBaseURL("INFINITY_URL", raw)
	if err != nil {
		return err
	}
	cfg.InfinityURL = u
	return nil
}

// loadWaitsAndLimits reads the durations and counts whose bad values fail Load.
func loadWaitsAndLimits(cfg *Config) error {
	settings := []func() error{
		durSetting("BROKER_INTERACTIVE_WAIT", 30*time.Second, &cfg.InteractiveWait),
		durSetting("BROKER_BATCH_WAIT", 5*time.Second, &cfg.BatchWait),
		durSetting("BROKER_EMBED_TIMEOUT", 30*time.Second, &cfg.EmbedTimeout),
		durSetting("BROKER_DETECT_INTERVAL", 3*time.Second, &cfg.DetectInterval),
		durSetting("UPSTREAM_IDLE_TIMEOUT", 0, &cfg.UpstreamIdleTimeout),
		func() error { return checkUpstreamIdle(cfg) },
		intSetting("BROKER_YIELD_CONFIRM_POLLS", 2, &cfg.YieldConfirmPolls),
		intSetting("BROKER_MAX_WAITERS", 256, &cfg.MaxWaiters),
		intSetting("BROKER_MAX_INFLIGHT", 1, &cfg.MaxInflight),
		durSetting("BROKER_BATCH_QUANTUM", 10*time.Second, &cfg.BatchQuantum),
		intSetting("BROKER_JOB_MAX_ATTEMPTS", 3, &cfg.JobMaxAttempts),
		durSetting("BROKER_JOB_PRUNE_INTERVAL", 10*time.Minute, &cfg.JobPruneInterval),
		durSetting("BROKER_JOB_FETCHED_GRACE", time.Hour, &cfg.JobFetchedGrace),
		durSetting("BROKER_JOB_HARD_CAP", 7*24*time.Hour, &cfg.JobHardCap),
		intSetting("BROKER_TDARR_GPU_WORKERS", 1, &cfg.TdarrGPUWorkers),
	}
	for _, read := range settings {
		if err := read(); err != nil {
			return err
		}
	}
	return nil
}

func durSetting(key string, def time.Duration, dst *time.Duration) func() error {
	return func() (err error) {
		*dst, err = getdur(key, def)
		return err
	}
}

func intSetting(key string, def int, dst *int) func() error {
	return func() (err error) {
		*dst, err = getint(key, def)
		return err
	}
}

// checkUpstreamIdle rejects a negative UPSTREAM_IDLE_TIMEOUT, and a nonzero one
// without a unit name to stop.
func checkUpstreamIdle(cfg *Config) error {
	uit := cfg.UpstreamIdleTimeout
	if uit < 0 {
		return fmt.Errorf("UPSTREAM_IDLE_TIMEOUT must not be negative, got %v", uit)
	}
	if uit != 0 && cfg.UpstreamUnitName == "" {
		return fmt.Errorf("UPSTREAM_IDLE_TIMEOUT is set but UPSTREAM_UNIT_NAME is empty; idle-unload requires a unit name")
	}
	return nil
}

// loadPark reads the park config. It is intentionally non-fatal to load: a bad
// park var must never keep the Broker from starting (parking is a resilience
// feature layered on top of the core proxy, not core config like OLLAMA_URL).
// Invalid values are logged via slog and fall back to the default, rather than
// failing Load() like getdur/getint above.
func loadPark(cfg *Config) error {
	cfg.ParkHold = getdurWarn("BROKER_PARK_HOLD", 600*time.Second)
	// BROKER_PARK_MAX_QUEUE=0 is the documented kill-switch (parking
	// disabled) — getintMin permits 0 and only rejects (warns+defaults on)
	// values below min, unlike getint which rejects anything < 1.
	cfg.ParkMaxQueue = getintMin("BROKER_PARK_MAX_QUEUE", 32, 0)
	cfg.ParkDrainBurst = getintMin("BROKER_PARK_DRAIN_BURST", 8, 1)
	return nil
}

// loadYieldUnits reads BROKER_YIELD_UNITS and everything that depends on it.
func loadYieldUnits(cfg *Config) error {
	var err error
	cfg.YieldUnits, err = parseYieldUnits(getenv("BROKER_YIELD_UNITS", ""), cfg.UpstreamUnitName, cfg.Routes)
	if err != nil {
		return err
	}
	if err := checkDisplaces(cfg.Routes, cfg.YieldUnits); err != nil {
		return err
	}
	cfg.UnitResumeHoldoff, err = getdur("BROKER_YIELD_UNIT_RESUME_HOLDOFF", 0)
	if err != nil {
		return err
	}
	if cfg.UnitResumeHoldoff < 0 {
		return fmt.Errorf("BROKER_YIELD_UNIT_RESUME_HOLDOFF must not be negative, got %v", cfg.UnitResumeHoldoff)
	}
	return nil
}

// loadPlainSettings reads the settings that cannot fail: strings and lists.
func loadPlainSettings(cfg *Config) error {
	cfg.InteractiveAddr = getenv("BROKER_INTERACTIVE_ADDR", ":11435")
	cfg.BatchAddr = getenv("BROKER_BATCH_ADDR", ":11436")
	cfg.EmbedAddr = getenv("BROKER_EMBED_ADDR", ":11438")
	cfg.ControlAddr = getenv("BROKER_CONTROL_ADDR", ":11437")
	cfg.ControlToken = getenv("BROKER_CONTROL_TOKEN", "")
	cfg.PlexURL = getenv("PLEX_URL", "http://localhost:32400")
	cfg.PlexToken = getenv("PLEX_TOKEN", "")
	cfg.CPUModels = splitList(getenv("BROKER_CPU_MODELS", ""))
	cfg.JobDBPath = getenv("BROKER_JOB_DB", "broker-jobs.db")
	cfg.TdarrURL = getenv("BROKER_TDARR_URL", "")
	cfg.TdarrNodeID = getenv("BROKER_TDARR_NODE_ID", "")
	return nil
}

// parseBaseURL parses raw and validates it includes both a scheme and a
// host. key names the source env var, used only for error messages. Shared
// by OLLAMA_URL, UPSTREAM_URL, INFINITY_URL, and each BROKER_ROUTE_<N>_URL —
// all validated the same way.
func parseBaseURL(key, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%s %q must include scheme and host", key, raw)
	}
	return u, nil
}

// splitList splits a comma-separated value, trimming spaces and dropping
// empty entries. It returns nil when nothing is left.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getint(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s must be >= 1, got %d", key, n)
	}
	return n, nil
}

func getdur(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	return d, nil
}

// getdurWarn parses a duration env var like getdur, but never fails Load():
// an unparseable value or a value <= 0 is logged via slog and replaced with
// def, rather than surfacing an error. Used for park config, which must
// never keep the Broker from starting (see the ParkHold/ParkMaxQueue/
// ParkDrainBurst loading comment in Load()).
func getdurWarn(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("config: invalid duration, using default", "key", key, "value", v, "default", def)
		return def
	}
	if d <= 0 {
		slog.Warn("config: duration must be positive, using default", "key", key, "value", d, "default", def)
		return def
	}
	return d
}

// getintMin parses an int env var, warning and falling back to def rather
// than failing Load(), if the value is unparseable or below min. Unlike
// getint (which rejects anything < 1), min is caller-supplied so 0 can be a
// permitted, meaningful value — e.g. BROKER_PARK_MAX_QUEUE=0, the documented
// parking kill-switch.
func getintMin(key string, def, min int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("config: invalid int, using default", "key", key, "value", v, "default", def)
		return def
	}
	if n < min {
		slog.Warn("config: value below minimum, using default", "key", key, "value", n, "min", min, "default", def)
		return def
	}
	return n
}
