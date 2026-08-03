package queue

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGateUpstreamTimeoutFreesSlot proves ADR-0013: a next.ServeHTTP call
// that never returns on its own (the embed-lane symptom — a stuck backend
// with no response) is bounded by upstreamTimeout, not left to wedge the
// slot forever. The fake upstream here stands in for httputil.ReverseProxy,
// which aborts and returns once its outbound request's context is
// cancelled — this test pins Gate's side of that contract (it must
// actually cancel ctx at the bound, and must not hold the slot past
// next.ServeHTTP returning).
func TestGateUpstreamTimeoutFreesSlot(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // simulate a stuck backend that only quits on cancellation
	})
	s := New()
	rec := newCountRec()
	srv := httptest.NewServer(s.Gate(Batch, 2*time.Second, 50*time.Millisecond, alwaysServe{}, rec, upstream))
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("request took %v, want bounded near the 50ms upstreamTimeout (not left to hang)", elapsed)
	}
	if got := rec.count("upstream_timeout"); got != 1 {
		t.Fatalf("upstream_timeout count = %d, want 1 (outcomes: %+v)", got, rec.outcomes)
	}

	// The slot must be free: a second request must not be stuck queueing
	// behind a "leaked" inflight slot from the first.
	if st := s.Stats(); st.Inflight != 0 {
		t.Fatalf("Inflight = %d after timeout, want 0 (slot must be released)", st.Inflight)
	}
}

// TestGateNoUpstreamTimeoutRunsUnbounded pins the zero-value contract:
// upstreamTimeout == 0 must behave exactly as before this feature existed —
// no bound at all. This is the setting every lane except the embed lane
// uses, and it must never accidentally cut off a legitimately long-running
// interactive/batch generation.
func TestGateNoUpstreamTimeoutRunsUnbounded(t *testing.T) {
	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, "ok")
	})
	s := New()
	srv := httptest.NewServer(s.Gate(Batch, 2*time.Second, 0, alwaysServe{}, nil, upstream))
	defer srv.Close()

	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Errorf("get: %v", err)
			return
		}
		done <- resp
	}()

	// Outlive what would be a short upstreamTimeout, proving nothing fired.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("request returned early; upstreamTimeout=0 must not bound the request")
	default:
	}
	close(release)

	select {
	case resp := <-done:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request never completed after release")
	}
}
