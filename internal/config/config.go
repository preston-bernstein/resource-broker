// Package config loads broker configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config holds all broker runtime settings.
type Config struct {
	// InteractiveAddr is the listen address for the high-priority class.
	InteractiveAddr string
	// BatchAddr is the listen address for the low-priority class.
	BatchAddr string
	// OllamaURL is the upstream real Ollama base URL.
	OllamaURL *url.URL
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

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	rawURL := getenv("OLLAMA_URL", "http://127.0.0.1:11434")
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid OLLAMA_URL %q: %w", rawURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("OLLAMA_URL %q must include scheme and host", rawURL)
	}

	// INFINITY_URL is optional; when set it must be a valid absolute URL. Empty
	// leaves InfinityURL nil and the embed lane is not started.
	var infinityURL *url.URL
	if raw := getenv("INFINITY_URL", ""); raw != "" {
		iu, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid INFINITY_URL %q: %w", raw, err)
		}
		if iu.Scheme == "" || iu.Host == "" {
			return nil, fmt.Errorf("INFINITY_URL %q must include scheme and host", raw)
		}
		infinityURL = iu
	}

	iw, err := getdur("BROKER_INTERACTIVE_WAIT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	bw, err := getdur("BROKER_BATCH_WAIT", 5*time.Second)
	if err != nil {
		return nil, err
	}
	et, err := getdur("BROKER_EMBED_TIMEOUT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	di, err := getdur("BROKER_DETECT_INTERVAL", 3*time.Second)
	if err != nil {
		return nil, err
	}
	ycp, err := getint("BROKER_YIELD_CONFIRM_POLLS", 2)
	if err != nil {
		return nil, err
	}
	mw, err := getint("BROKER_MAX_WAITERS", 256)
	if err != nil {
		return nil, err
	}
	mi, err := getint("BROKER_MAX_INFLIGHT", 1)
	if err != nil {
		return nil, err
	}
	bq, err := getdur("BROKER_BATCH_QUANTUM", 10*time.Second)
	if err != nil {
		return nil, err
	}
	jma, err := getint("BROKER_JOB_MAX_ATTEMPTS", 3)
	if err != nil {
		return nil, err
	}
	jpi, err := getdur("BROKER_JOB_PRUNE_INTERVAL", 10*time.Minute)
	if err != nil {
		return nil, err
	}
	jfg, err := getdur("BROKER_JOB_FETCHED_GRACE", time.Hour)
	if err != nil {
		return nil, err
	}
	jhc, err := getdur("BROKER_JOB_HARD_CAP", 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	tgw, err := getint("BROKER_TDARR_GPU_WORKERS", 1)
	if err != nil {
		return nil, err
	}

	// Park config is intentionally non-fatal to load: a bad park var must
	// never keep the Broker from starting (parking is a resilience feature
	// layered on top of the core proxy, not core config like OLLAMA_URL).
	// Invalid values are logged via slog and fall back to the default,
	// rather than failing Load() like getdur/getint above.
	ph := getdurWarn("BROKER_PARK_HOLD", 600*time.Second)
	// BROKER_PARK_MAX_QUEUE=0 is the documented kill-switch (parking
	// disabled) — getintMin permits 0 and only rejects (warns+defaults on)
	// values below min, unlike getint which rejects anything < 1.
	pmq := getintMin("BROKER_PARK_MAX_QUEUE", 32, 0)
	pdb := getintMin("BROKER_PARK_DRAIN_BURST", 8, 1)

	return &Config{
		InteractiveAddr:   getenv("BROKER_INTERACTIVE_ADDR", ":11435"),
		BatchAddr:         getenv("BROKER_BATCH_ADDR", ":11436"),
		EmbedAddr:         getenv("BROKER_EMBED_ADDR", ":11438"),
		EmbedTimeout:      et,
		OllamaURL:         u,
		InfinityURL:       infinityURL,
		InteractiveWait:   iw,
		BatchWait:         bw,
		ControlAddr:       getenv("BROKER_CONTROL_ADDR", ":11437"),
		ControlToken:      getenv("BROKER_CONTROL_TOKEN", ""),
		DetectInterval:    di,
		YieldConfirmPolls: ycp,
		PlexURL:           getenv("PLEX_URL", "http://localhost:32400"),
		PlexToken:         getenv("PLEX_TOKEN", ""),
		MaxWaiters:        mw,
		MaxInflight:       mi,
		BatchQuantum:      bq,
		JobDBPath:         getenv("BROKER_JOB_DB", "broker-jobs.db"),
		JobMaxAttempts:    jma,
		JobPruneInterval:  jpi,
		JobFetchedGrace:   jfg,
		JobHardCap:        jhc,
		TdarrURL:          getenv("BROKER_TDARR_URL", ""),
		TdarrNodeID:       getenv("BROKER_TDARR_NODE_ID", ""),
		TdarrGPUWorkers:   tgw,
		ParkHold:          ph,
		ParkMaxQueue:      pmq,
		ParkDrainBurst:    pdb,
	}, nil
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
