package job

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/testhttp"
)

func newAPI(t *testing.T) (*Service, Store, http.Handler) {
	t.Helper()
	store := newStore(t)
	svc := NewService(store, 3)
	return svc, store, svc.Routes()
}

// postJob sends POST /jobs with body and headers.
func postJob(t *testing.T, h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/jobs", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return serve(h, req)
}

// call sends a body-less request.
func call(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(h, httptest.NewRequestWithContext(context.Background(), method, path, nil))
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPISubmitRequiresIdempotencyKey(t *testing.T) {
	_, _, h := newAPI(t)
	rec := postJob(t, h, `{"model":"m","prompt":"p"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAPISubmitRequiresModel(t *testing.T) {
	_, _, h := newAPI(t)
	rec := postJob(t, h, `{"prompt":"p"}`, map[string]string{"Idempotency-Key": "k"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAPISubmitIdempotent(t *testing.T) {
	_, _, h := newAPI(t)
	hdr := map[string]string{"Idempotency-Key": "dup"}

	rec := postJob(t, h, `{"model":"m","prompt":"p"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first submit status = %d, want 201", rec.Code)
	}
	var first struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first.JobID == "" {
		t.Fatal("no job_id returned")
	}

	rec2 := postJob(t, h, `{"model":"m2","prompt":"other"}`, hdr)
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", rec2.Code)
	}
	var second struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &second)
	if second.JobID != first.JobID {
		t.Fatalf("replay job_id = %s, want %s", second.JobID, first.JobID)
	}
}

func TestAPIGetAndPosition(t *testing.T) {
	svc, _, h := newAPI(t)
	id := submitJob(t, svc, "g")

	rec := call(t, h, "GET", "/jobs/"+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var st Status
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.State != StateQueued || st.Position != 1 {
		t.Fatalf("status = %+v, want QUEUED position 1", st)
	}

	if r := call(t, h, "GET", "/jobs/nope"); r.Code != http.StatusNotFound {
		t.Fatalf("unknown get = %d, want 404", r.Code)
	}
}

func TestAPIResultLifecycle(t *testing.T) {
	svc, store, h := newAPI(t)
	id := submitJob(t, svc, "res")
	ctx := context.Background()

	// Not ready while queued.
	if r := call(t, h, "GET", "/jobs/"+id+"/result"); r.Code != http.StatusConflict {
		t.Fatalf("premature result = %d, want 409", r.Code)
	}

	// Drive it to SUCCEEDED directly through the store.
	_, _ = store.ClaimNext(ctx)
	_ = store.Succeed(ctx, id, "the output")

	r := call(t, h, "GET", "/jobs/"+id+"/result")
	if r.Code != http.StatusOK {
		t.Fatalf("result status = %d", r.Code)
	}
	var body struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &body)
	if body.Result != "the output" {
		t.Fatalf("result = %q", body.Result)
	}
}

func TestAPIListFilter(t *testing.T) {
	svc, _, h := newAPI(t)
	a := submitJobWith(t, svc, "la", "alice")
	_ = submitJobWith(t, svc, "lb", "bob")

	rec := call(t, h, "GET", "/jobs?owner=alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var body struct {
		Jobs []Status `json:"jobs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Jobs) != 1 || body.Jobs[0].ID != a {
		t.Fatalf("filtered list = %+v, want only %s", body.Jobs, a)
	}
}

func TestAPICancel(t *testing.T) {
	svc, _, h := newAPI(t)
	id := submitJob(t, svc, "can")
	rec := call(t, h, "POST", "/jobs/"+id+"/cancel")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d", rec.Code)
	}
	st, _ := svc.Get(context.Background(), id)
	if st.State != StateCanceled {
		t.Fatalf("state = %s, want CANCELED", st.State)
	}
}

func TestAPIEventsTerminalSnapshot(t *testing.T) {
	svc, store, _ := newAPI(t)
	id := submitJob(t, svc, "ev")
	ctx := context.Background()
	_, _ = store.ClaimNext(ctx)
	_ = store.Succeed(ctx, id, "done")

	// A real server so the SSE handler's Flusher works.
	srv := httptest.NewServer(svc.Routes())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	resp, err := testhttp.Do(ctx, http.MethodGet, srv.URL+"/jobs/"+id+"/events", nil)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, "event: state") || !strings.Contains(body, "event: done") {
		t.Fatalf("events body missing snapshot/done: %q", body)
	}
}

func submitJobWith(t *testing.T, svc *Service, key, owner string) string {
	t.Helper()
	j, _, err := svc.Submit(context.Background(), SubmitRequest{IdempotencyKey: key, Owner: owner, Model: "m"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return j.ID
}
