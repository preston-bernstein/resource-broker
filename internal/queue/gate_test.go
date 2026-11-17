package queue

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/testhttp"
)

// TestGateSerializes proves two gated requests do not hit the upstream
// concurrently: the handler asserts max in-flight is 1.
func TestGateSerializes(t *testing.T) {
	var inflight, maxSeen int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		_, _ = io.WriteString(w, "ok")
	})

	s := New()
	srv := httptest.NewServer(s.Gate(GateConfig{Class: Batch, Wait: 5 * time.Second, Admission: alwaysServe{}}, upstream))
	defer srv.Close()

	const n = 5
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := testhttp.Get(srv.URL)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}

	if got := atomic.LoadInt32(&maxSeen); got != 1 {
		t.Fatalf("max concurrent upstream = %d, want 1", got)
	}
}

// TestGateRequestIDCorrelation proves the 2026-08-01 audit's "correlation"
// fix: the Synchronous path (unlike the durable Job path, which has always
// had j.ID) now mints a per-request id, echoes it as X-Broker-Request-Id,
// and includes it (plus path/method) on the access log line so a consumer's
// own failure can be matched to a specific broker log line by id, not just
// by timestamp.
func TestGateRequestIDCorrelation(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	srv := startGate(t, New(), GateConfig{Class: Batch, Wait: 2 * time.Second, Admission: alwaysServe{}}, upstream)

	resp, err := testhttp.Get(srv.URL + "/api/generate")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	reqID := resp.Header.Get("X-Broker-Request-Id")
	if reqID == "" {
		t.Fatal("missing X-Broker-Request-Id header")
	}
	if len(reqID) != 16 { // 8 bytes, hex-encoded
		t.Fatalf("X-Broker-Request-Id = %q, want 16 hex chars", reqID)
	}
	if !hasAccessLine(logBuf.String(), reqID, "/api/generate", "GET") {
		t.Fatalf("no access log line correlates req_id=%q path=/api/generate method=GET\n--- log ---\n%s", reqID, logBuf.String())
	}
}

// hasAccessLine reports whether the JSON log holds a "request" line carrying
// reqID, path and method — not just class/outcome/wait_ms as before.
func hasAccessLine(log, reqID, path, method string) bool {
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry["msg"] != "request" {
			continue
		}
		if entry["req_id"] == reqID && entry["path"] == path && entry["method"] == method {
			return true
		}
	}
	return false
}
