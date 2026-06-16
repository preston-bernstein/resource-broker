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

	return &Config{
		InteractiveAddr: getenv("BROKER_INTERACTIVE_ADDR", ":11435"),
		BatchAddr:       getenv("BROKER_BATCH_ADDR", ":11436"),
		OllamaURL:       u,
		InteractiveWait: iw,
		BatchWait:       bw,
		ControlAddr:     getenv("BROKER_CONTROL_ADDR", ":11437"),
		DetectInterval:  di,
		MaxWaiters:      mw,
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
