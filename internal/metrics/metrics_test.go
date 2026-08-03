package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandlerRendersCountersAndGauges(t *testing.T) {
	r := New()
	r.Record("batch", "served", 120*time.Millisecond)
	r.Record("batch", "served", 80*time.Millisecond)
	r.Record("interactive", "deferred", 0)

	h := r.Handler(func() Gauges {
		return Gauges{Yielding: true, Busy: true, Interactive: 2, Batch: 1}
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	want := []string{
		`broker_requests_total{class="batch",outcome="served"} 2`,
		`broker_requests_total{class="interactive",outcome="deferred"} 1`,
		`broker_wait_seconds_count 2`,
		`broker_wait_seconds_sum 0.2`,
		`broker_yielding 1`,
		`broker_busy 1`,
		`broker_queue_depth{class="interactive"} 2`,
		`broker_queue_depth{class="batch"} 1`,
		`broker_detect_errors_total 0`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("metrics output missing %q\n--- got ---\n%s", w, body)
		}
	}
}

// TestIncDetectError proves the detect.ErrorRecorder wiring (2026-08-01
// audit fix): each call is a separate poll's fail-open, tallied as a
// monotonic counter, visible on /metrics.
func TestIncDetectError(t *testing.T) {
	r := New()
	r.IncDetectError()
	r.IncDetectError()
	r.IncDetectError()

	h := r.Handler(func() Gauges { return Gauges{} })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "broker_detect_errors_total 3") {
		t.Errorf("metrics output missing broker_detect_errors_total 3\n--- got ---\n%s", body)
	}
}
