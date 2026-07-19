package queue

import (
	"io"
	"net/http"
	"net/http/httptest"
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
