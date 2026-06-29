// Package config loads broker configuration from environment variables.
package config

import (
	"fmt"
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
	// InteractiveWait is the queue wait budget for interactive requests.
	InteractiveWait time.Duration
	// BatchWait is the queue wait budget for batch requests.
	BatchWait time.Duration
	// ControlAddr is the listen address for the admin/control plane.
	ControlAddr string
	// DetectInterval is how often process detection re-evaluates contention.
	DetectInterval time.Duration
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

	iw, err := getdur("BROKER_INTERACTIVE_WAIT", 30*time.Second)
	if err != nil {
		return nil, err
	}
	bw, err := getdur("BROKER_BATCH_WAIT", 5*time.Second)
	if err != nil {
		return nil, err
	}
	di, err := getdur("BROKER_DETECT_INTERVAL", 3*time.Second)
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

	return &Config{
		InteractiveAddr:  getenv("BROKER_INTERACTIVE_ADDR", ":11435"),
		BatchAddr:        getenv("BROKER_BATCH_ADDR", ":11436"),
		OllamaURL:        u,
		InteractiveWait:  iw,
		BatchWait:        bw,
		ControlAddr:      getenv("BROKER_CONTROL_ADDR", ":11437"),
		DetectInterval:   di,
		MaxWaiters:       mw,
		MaxInflight:      mi,
		BatchQuantum:     bq,
		JobDBPath:        getenv("BROKER_JOB_DB", "broker-jobs.db"),
		JobMaxAttempts:   jma,
		JobPruneInterval: jpi,
		JobFetchedGrace:  jfg,
		JobHardCap:       jhc,
		TdarrURL:         getenv("BROKER_TDARR_URL", ""),
		TdarrNodeID:      getenv("BROKER_TDARR_NODE_ID", ""),
		TdarrGPUWorkers:  tgw,
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
