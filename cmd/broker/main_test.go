package main

// TestMainStartsWithoutPanic builds the real binary and runs it briefly under
// each UPSTREAM_BACKEND value, asserting clean startup (a "broker up" log
// line, no panic/crash). This exists because every other test in this repo
// exercises a package in isolation — nothing exercised main() itself, which
// is exactly how a nil-pointer panic on startup under the default backend
// slipped past `go build`, `go vet`, and a green `go test ./... -race` (see
// the composition-root gap found during this feature's integration
// validation pass). A package-level unit test can't easily drive main()
// itself (it blocks on ListenAndServe/signal handling), so this builds and
// runs the actual binary as a subprocess instead.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func buildBrokerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ollama-broker-test")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runBrokerBriefly(t *testing.T, bin string, env []string) (stdout string, err error) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if startErr := cmd.Start(); startErr != nil {
		t.Fatalf("start broker: %v", startErr)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Poll for the startup log line rather than sleeping a fixed window —
	// under -race, startup is measurably slower due to instrumentation
	// overhead, and a fixed window flakes exactly the way the composition-
	// root gap this test exists to catch would: silently, only sometimes.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), `"msg":"broker up"`) || strings.Contains(buf.String(), "panic:") {
			break
		}
		select {
		case werr := <-done:
			return buf.String(), werr
		case <-time.After(20 * time.Millisecond):
		}
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	return buf.String(), nil
}

// syncBuffer is a concurrency-safe bytes.Buffer: cmd.Stdout/Stderr are
// written from the subprocess-reading goroutines exec.Cmd spawns, while the
// poll loop above reads it concurrently from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMainStartsWithoutPanic(t *testing.T) {
	bin := buildBrokerBinary(t)

	cases := []struct {
		name string
		env  []string
	}{
		{
			name: "default (UPSTREAM_BACKEND unset, ollama)",
			env: []string{
				"OLLAMA_URL=http://127.0.0.1:19999",
				"BROKER_CONTROL_ADDR=:0",
				"BROKER_INTERACTIVE_ADDR=:0",
				"BROKER_BATCH_ADDR=:0",
			},
		},
		{
			name: "explicit ollama",
			env: []string{
				"UPSTREAM_BACKEND=ollama",
				"OLLAMA_URL=http://127.0.0.1:19999",
				"BROKER_CONTROL_ADDR=:0",
				"BROKER_INTERACTIVE_ADDR=:0",
				"BROKER_BATCH_ADDR=:0",
			},
		},
		{
			name: "openai backend",
			env: []string{
				"UPSTREAM_BACKEND=openai",
				"UPSTREAM_URL=http://127.0.0.1:19999",
				"BROKER_CONTROL_ADDR=:0",
				"BROKER_INTERACTIVE_ADDR=:0",
				"BROKER_BATCH_ADDR=:0",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runBrokerBriefly(t, bin, tc.env)
			if strings.Contains(out, "panic:") {
				t.Fatalf("broker panicked on startup:\n%s", out)
			}
			if err != nil {
				t.Fatalf("broker exited unexpectedly: %v\noutput:\n%s", err, out)
			}
			if !strings.Contains(out, `"msg":"broker up"`) {
				t.Fatalf("broker did not log a clean startup:\n%s", out)
			}
		})
	}
}
