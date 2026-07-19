package queue

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type alwaysServe struct{}

func (alwaysServe) Yielding() (bool, string)      { return false, "" }
func (alwaysServe) ServeContext() context.Context { return context.Background() }

type alwaysYield struct{}

func (alwaysYield) Yielding() (bool, string)      { return true, "gaming-steam" }
func (alwaysYield) ServeContext() context.Context { return context.Background() }

func TestGateRefusesWhenYielding(t *testing.T) {
	var hit bool
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		io.WriteString(w, "ok")
	})
	s := New()
	srv := httptest.NewServer(s.Gate(Batch, 2*time.Second, alwaysYield{}, nil, upstream))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if hit {
		t.Fatal("upstream was hit while yielding")
	}
	// Slot must not be left held.
	if st := s.Stats(); st.Busy {
		t.Fatalf("scheduler busy after refused request: %+v", st)
	}
}
