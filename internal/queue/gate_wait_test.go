package queue

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGateWaitBudgetExceeded: a request that cannot get the slot within its
// wait budget gets 503 + Retry-After, and the upstream is never hit.
func TestGateWaitBudgetExceeded(t *testing.T) {
	release := make(chan struct{})
	var secondHit bool
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			<-release // occupy the slot
			return
		}
		secondHit = true
	})

	s := New()
	srv := httptest.NewServer(s.Gate(Batch, 100*time.Millisecond, alwaysServe{}, nil, upstream))
	defer srv.Close()

	// Occupy the slot with a long-held request.
	go func() {
		resp, err := http.Get(srv.URL + "/hold")
		if err == nil {
			resp.Body.Close()
		}
	}()
	// Wait until the holder actually owns the slot (robust vs a fixed sleep).
	deadline := time.Now().Add(2 * time.Second)
	for !s.Stats().Busy && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// Second request cannot get in within 100ms.
	resp, err := http.Get(srv.URL + "/second")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
	if secondHit {
		t.Error("upstream hit despite exceeded budget")
	}
	close(release)
}
