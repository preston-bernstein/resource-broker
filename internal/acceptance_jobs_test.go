package broker_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/job"
	"github.com/preston-bernstein/resource-broker/internal/testhttp"
)

// pacedChunks is a chat upstream that sends n "tok<i> " deltas, 60ms apart,
// then (when finalSSE is non-empty) the final events. A pause between chunks
// gives a test's poll loop time to observe more than one in-progress token
// count.
func pacedChunks(t *testing.T, n int, final ...string) *httptest.Server {
	t.Helper()
	return pathUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		for i := 0; i < n; i++ {
			writeSSE(w, sseDelta(fmt.Sprintf("tok%d ", i)))
			fl.Flush()
			time.Sleep(60 * time.Millisecond)
		}
		writeSSE(w, final...)
		fl.Flush()
	})
}

// pollJob polls the Job until it is terminal or the deadline passes. It
// returns the final state ("" if it never finished), the highest token count
// seen, and whether that count was seen to increase between polls.
func pollJob(t *testing.T, controlURL, jobID string) (final job.State, maxTokens int, sawIncrease bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := getJobStatus(t, controlURL, jobID)
		if st.Progress != nil && st.Progress.Tokens > maxTokens {
			sawIncrease = sawIncrease || maxTokens > 0
			maxTokens = st.Progress.Tokens
		}
		if st.State.Terminal() {
			return st.State, maxTokens, sawIncrease
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", maxTokens, sawIncrease
}

// --- AC-6: durable Job lifecycle end to end ---

// TestAcceptance_AC6_JobLifecycleEndToEnd proves a Job submitted via POST
// /jobs through the full stack (durable Service+Worker -> openai backend's
// real Client.Generate -> a mock OpenAI-compatible upstream) runs to
// SUCCEEDED, and that GET /jobs/<id> shows an increasing token count while
// RUNNING. The mock upstream paces its chunks so this test's poll loop has
// time to observe more than one distinct in-progress token count.
func TestAcceptance_AC6_JobLifecycleEndToEnd(t *testing.T) {
	r := newRig(t, openaiCfg(t, pacedChunks(t, 6, sseDone).URL))

	jobID := submitJobBody(t, r.control.URL, "acceptance-ac6", `{"model":"acceptance-model","prompt":"hi"}`)
	finalState, maxTokensSeen, sawIncrease := pollJob(t, r.control.URL, jobID)

	if finalState != job.StateSucceeded {
		t.Fatalf("job final state = %q, want %q (last max tokens seen: %d)", finalState, job.StateSucceeded, maxTokensSeen)
	}
	if !sawIncrease {
		t.Errorf("token count progress never increased across polls (max seen: %d) — GET /jobs/<id> should show growth during execution", maxTokensSeen)
	}
	if maxTokensSeen == 0 {
		t.Error("token count progress never observed above 0")
	}
}

// --- AC-7: gaming/Plex Yield preempts an in-flight Job end to end ---

// TestAcceptance_AC7_YieldPreemptsJobEndToEnd proves that forcing Yield
// through the real control plane (POST /control {"mode":"yield"} — the same
// HTTP surface an operator or a future gaming-detector integration uses,
// exercising *yield.Controller's real SetMode/applyLocked transition rather
// than a fake) while a durable openai-backend Job is RUNNING cancels that
// Job's in-flight upstream call and requeues it to the front of the Queue:
// Position 1, State QUEUED. internal/job/worker_test.go's
// TestWorkerGamingPreempt already pins this at the Worker-unit level (with a
// fakeYield); this proves the same outcome through the full wired stack (real
// HTTP control endpoint -> real Controller -> real Worker -> real openai
// backend/client -> mock upstream).
func TestAcceptance_AC7_YieldPreemptsJobEndToEnd(t *testing.T) {
	upstream := pathUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		writeSSE(w, sseDelta("x"))
		fl.Flush()
		<-r.Context().Done() // held open until Yield cancels the Job's upstream call
	})
	r := newRig(t, openaiCfg(t, upstream.URL))

	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac7")
	waitForJobState(t, r.control.URL, jobID, job.StateRunning)

	requireStatus(t, post(t, r.control.URL+"/control", `{"mode":"yield"}`), http.StatusOK)

	st := waitForJobState(t, r.control.URL, jobID, job.StateQueued)
	if st.Position != 1 {
		t.Fatalf("position after gaming preempt = %d, want 1 (front of queue)", st.Position)
	}
	if st.Attempts != 0 {
		t.Fatalf("gaming preempt burned an attempt: %d", st.Attempts)
	}
}

// --- AC-8: interactive request preempts a batch Job past BROKER_BATCH_QUANTUM ---

// batchJobUpstream is a chat upstream whose first request for jobModel blocks
// until preemption cancels it. Once preempted, the Worker immediately
// re-claims the freed slot and re-runs the same Job (nothing else is queued),
// so its second connection must complete normally: the Job converges to a
// deterministic terminal state instead of blocking forever on a connection
// nothing will ever cancel again, which would otherwise hang the server's
// cleanup.
func batchJobUpstream(t *testing.T, jobModel string) *httptest.Server {
	t.Helper()
	var jobCalls atomic.Int32
	return pathUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var reqBody struct {
			Model string `json:"model"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &reqBody)

		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		writeSSE(w, sseDelta("x"))
		fl.Flush()
		if reqBody.Model == jobModel && jobCalls.Add(1) == 1 {
			<-r.Context().Done() // held open until quantum-based preemption cancels it
			return
		}
		writeSSE(w, sseStop, sseDone)
		fl.Flush()
	})
}

// TestAcceptance_AC8_InteractivePreemptsBatchJobPastQuantumEndToEnd proves
// that a real interactive HTTP request, parked behind a running batch Job on
// a MaxInflight=1 gate, does NOT preempt the Job before a short test-friendly
// BROKER_BATCH_QUANTUM has elapsed, and DOES preempt it once the quantum has
// elapsed — through the full wired stack (real interactive Gate -> real
// Worker's monitor/shouldPreempt -> real openai backend -> mock upstream),
// complementing internal/job/worker_test.go's
// TestWorkerInteractivePreemptPastQuantum (Worker-unit level, direct
// sched.Acquire) with the real end-to-end HTTP path.
func TestAcceptance_AC8_InteractivePreemptsBatchJobPastQuantumEndToEnd(t *testing.T) {
	const jobModel = "batch-job-model"
	cfg := openaiCfg(t, batchJobUpstream(t, jobModel).URL)
	quantum := 400 * time.Millisecond // test-friendly, short BROKER_BATCH_QUANTUM
	cfg.BatchQuantum = quantum
	r := newRig(t, cfg)

	jobID := submitJobBody(t, r.control.URL, "acceptance-ac8", fmt.Sprintf(`{"model":%q,"prompt":"hi"}`, jobModel))
	waitForJobState(t, r.control.URL, jobID, job.StateRunning)
	start := time.Now()

	// A real interactive request, parked on the busy MaxInflight=1 gate behind
	// the running batch Job, drives w.gate.Stats().Interactive > 0 the same
	// way a live Consumer request would.
	interactiveDone := make(chan struct{})
	go func() {
		defer close(interactiveDone)
		resp, err := testhttp.Post(r.interactive.URL+"/api/generate", "application/json", strings.NewReader(`{"model":"interactive-model","prompt":"hi","stream":true}`))
		if err != nil {
			t.Error("interactive request: " + err.Error())
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	requireQuantumPreemption(t, interactiveDone, start, quantum)

	// The batch Job was requeued (resume-first) rather than failed: the
	// Worker immediately re-claims the now-free slot and re-runs it to
	// SUCCEEDED (its second connection, per the mock above) with no attempt
	// burned by the preempt itself (AC-8). Waiting for the terminal state,
	// rather than trying to catch the transient QUEUED window between the
	// preempt and the automatic re-run, is what makes this assertion
	// deterministic instead of racing the Worker's own re-acquire.
	st := waitForJobState(t, r.control.URL, jobID, job.StateSucceeded)
	if st.Attempts != 0 {
		t.Fatalf("interactive preempt burned an attempt: %d", st.Attempts)
	}
}

// requireQuantumPreemption checks the interactive request completes only after
// the quantum: not before (AC-8's "not before", checked at a third of the
// quantum, well short of both the quantum itself and the Worker's 200ms
// re-check ticker), and once past it the Worker must preempt so the
// request's slot frees up.
func requireQuantumPreemption(t *testing.T, interactiveDone <-chan struct{}, start time.Time, quantum time.Duration) {
	t.Helper()
	select {
	case <-interactiveDone:
		t.Fatalf("interactive request completed after only %v, before quantum %v elapsed (preempted too early)", time.Since(start), quantum)
	case <-time.After(quantum / 3):
	}
	select {
	case <-interactiveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("interactive request never completed: batch Job was never preempted")
	}
	const tolerance = 150 * time.Millisecond // absorbs the 200ms monitor ticker + HTTP round trips
	if elapsed := time.Since(start); elapsed < quantum-tolerance {
		t.Fatalf("interactive request completed after %v, want >= ~quantum %v (preempted before its trigger time)", elapsed, quantum)
	}
}

// --- AC-11: metrics end to end ---

// TestAcceptance_AC11_MetricsEndToEnd scrapes /metrics through the full rig
// after a served Synchronous request and a completed Job, and confirms
// broker_requests_total{class,outcome} and broker_job_outcomes_total{outcome}
// carry the expected label values, and that a couple of well-known
// pre-existing metric names (from internal/metrics/metrics.go, unrelated to
// this feature) are still present unrenamed — a spot check, not an
// exhaustive one, that this feature didn't rename/remove anything.
func TestAcceptance_AC11_MetricsEndToEnd(t *testing.T) {
	// The Job path always speaks SSE to the upstream regardless of the
	// inbound request's stream field (internal/openaicompat's openChatStream
	// always sends stream:true), so the mock must respond in proper SSE shape
	// for both the interactive request below and the Job.
	r := newRig(t, openaiCfg(t, chatUpstream(t, sseDelta("hi"), sseStop, sseDone).URL))

	// One served Synchronous request on the interactive class.
	status, _ := postDrained(t, r.interactive.URL+"/api/generate", `{"model":"acceptance-model","prompt":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	// One completed Job.
	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac11")
	waitForJobState(t, r.control.URL, jobID, job.StateSucceeded)

	mresp := get(t, r.control.URL+"/metrics")
	requireStatus(t, mresp, http.StatusOK)
	raw, err := io.ReadAll(mresp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	body := string(raw)

	wants := []string{
		`broker_requests_total{class="interactive",outcome="served"} 1`,
		`broker_job_outcomes_total{outcome="succeeded"} 1`,
		// Spot-check: pre-existing metric names (unrelated to this feature)
		// must not have been renamed or removed.
		"broker_yielding ",
		"broker_wait_seconds_count ",
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q (renamed or removed?):\n%s", want, body)
		}
	}
}

// --- AC-13: Job failure on upstream errors ---

// TestAcceptance_AC13_JobFailsAfterExhaustingRetriesOnUpstreamErrorEndToEnd
// proves that with UPSTREAM_BACKEND=openai, a Job whose mock upstream always
// returns a non-2xx status exhausts BROKER_JOB_MAX_ATTEMPTS and lands FAILED
// — through the real durable Service+Worker+openai-backend wiring, not a
// generic fakeGen (internal/job/worker_test.go's TestWorkerRetryThenFail
// already pins the generic retry-then-fail mechanics; this proves the same
// outcome specifically for an openai-backend upstream error, end to end).
func TestAcceptance_AC13_JobFailsAfterExhaustingRetriesOnUpstreamErrorEndToEnd(t *testing.T) {
	var calls atomic.Int32
	upstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	cfg := openaiCfg(t, upstream.URL)
	cfg.JobMaxAttempts = 2
	r := newRig(t, cfg)

	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac13-job")
	st := waitForJobState(t, r.control.URL, jobID, job.StateFailed)

	if st.Attempts != 2 {
		t.Errorf("job attempts = %d, want 2 (BROKER_JOB_MAX_ATTEMPTS)", st.Attempts)
	}
	if st.Error == "" {
		t.Error("failed job carries no error message")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("mock upstream called %d times, want 2 (one per attempt)", got)
	}
}

// --- AC-19: token count when the upstream omits usage ---

// TestAcceptance_AC19_UsageOmittedFallbackTokenCountEndToEnd proves that when
// the mock upstream omits the usage field on its final SSE chunk (replicating
// vLLM's default behavior), a Job's token-count progress still increases and
// reaches the correct final count, derived from the per-chunk fallback
// counter (internal/openaicompat/stream.go's parseSSEStream) — and that the
// outbound request to the mock upstream included stream_options:
// {include_usage:true} regardless.
func TestAcceptance_AC19_UsageOmittedFallbackTokenCountEndToEnd(t *testing.T) {
	const chunks = 4
	var gotBody atomic.Value
	upstream := pathUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody.Store(string(raw))
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		for i := 0; i < chunks; i++ {
			writeSSE(w, sseDelta(fmt.Sprintf("tok%d ", i)))
			fl.Flush()
			time.Sleep(60 * time.Millisecond)
		}
		// The final chunk deliberately omits "usage" (sseStop), replicating
		// vLLM's default streaming behavior (FR-26/AC-19).
		writeSSE(w, sseStop, sseDone)
		fl.Flush()
	})
	r := newRig(t, openaiCfg(t, upstream.URL))
	jobID := submitJobViaHTTP(t, r.control.URL, "acceptance-ac19")

	finalState, maxTokensSeen, _ := pollJob(t, r.control.URL, jobID)
	if finalState != job.StateSucceeded {
		t.Fatalf("job final state = %q, want SUCCEEDED (max tokens seen: %d)", finalState, maxTokensSeen)
	}
	if maxTokensSeen != chunks {
		t.Errorf("max tokens observed while RUNNING = %d, want %d (fallback per-chunk counter, upstream omitted usage)", maxTokensSeen, chunks)
	}

	raw, _ := gotBody.Load().(string)
	var outbound map[string]any
	if err := json.Unmarshal([]byte(raw), &outbound); err != nil {
		t.Fatalf("decode captured outbound request body %q: %v", raw, err)
	}
	so, ok := outbound["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("outbound request body missing stream_options: %s", raw)
	}
	if so["include_usage"] != true {
		t.Errorf("outbound stream_options.include_usage = %v, want true", so["include_usage"])
	}
}
