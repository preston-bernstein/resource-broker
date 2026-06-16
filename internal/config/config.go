// Package config loads broker configuration from environment variables.
package config

import (
	"fmt"
	"net/url"
	"os"
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

	return &Config{
		InteractiveAddr: getenv("BROKER_INTERACTIVE_ADDR", ":11435"),
		BatchAddr:       getenv("BROKER_BATCH_ADDR", ":11436"),
		OllamaURL:       u,
		InteractiveWait: iw,
		BatchWait:       bw,
	}, nil
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
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
