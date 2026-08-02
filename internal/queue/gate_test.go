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
		io.WriteString(w, "ok")
	})

	s := New()
	srv := httptest.NewServer(s.Gate(Batch, 5*time.Second, alwaysServe{}, nil, upstream))
	defer srv.Close()

	const n = 5
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := http.Get(srv.URL)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
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
		io.WriteString(w, "ok")
	})
	s := New()
	srv := httptest.NewServer(s.Gate(Batch, 2*time.Second, alwaysServe{}, nil, upstream))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/generate")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	reqID := resp.Header.Get("X-Broker-Request-Id")
	if reqID == "" {
		t.Fatal("missing X-Broker-Request-Id header")
	}
	if len(reqID) != 16 { // 8 bytes, hex-encoded
		t.Fatalf("X-Broker-Request-Id = %q, want 16 hex chars", reqID)
	}

	// The access log line must carry the same id, plus path/method — not just
	// class/outcome/wait_ms as before.
	found := false
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["msg"] != "request" {
			continue
		}
		if entry["req_id"] == reqID && entry["path"] == "/api/generate" && entry["method"] == "GET" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no access log line correlates req_id=%q path=/api/generate method=GET\n--- log ---\n%s", reqID, logBuf.String())
	}
}
