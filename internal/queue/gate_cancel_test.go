package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// manualAdm is a controllable Admission for tests.
type manualAdm struct {
	mu     sync.Mutex
	yield  bool
	reason string
	ctx    context.Context
	cancel context.CancelFunc
}

func newManualAdm() *manualAdm {
	ctx, cancel := context.WithCancel(context.Background())
	return &manualAdm{ctx: ctx, cancel: cancel}
}

func (m *manualAdm) Yielding() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.yield, m.reason
}

func (m *manualAdm) ServeContext() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ctx
}

func (m *manualAdm) startYield() {
	m.mu.Lock()
	m.yield, m.reason = true, "gaming-steam"
	cancel := m.cancel
	m.mu.Unlock()
	cancel()
}

// TestGateCancelsInFlightOnYield proves an in-flight upstream call is aborted
// when yielding begins mid-request.
func TestGateCancelsInFlightOnYield(t *testing.T) {
	started := make(chan struct{})
	var canceled bool
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done(): // upstream context aborted by the gate
			canceled = true
		case <-time.After(3 * time.Second):
		}
	})

	adm := newManualAdm()
	s := New()
	srv := httptest.NewServer(s.Gate(Batch, 2*time.Second, adm, nil, upstream))
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		resp, err := http.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		close(done)
	}()

	<-started        // upstream is in flight
	adm.startYield() // gaming begins

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not return after yield started")
	}
	if !canceled {
		t.Fatal("in-flight upstream context was not cancelled on yield")
	}
}
