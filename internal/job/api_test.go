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
)

func newAPI(t *testing.T) (*Service, Store, http.Handler) {
	t.Helper()
	store := newStore(t)
	svc := NewService(store, 3)
	return svc, store, svc.Routes()
}

func do(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPISubmitRequiresIdempotencyKey(t *testing.T) {
	_, _, h := newAPI(t)
	rec := do(t, h, "POST", "/jobs", `{"model":"m","prompt":"p"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAPISubmitRequiresModel(t *testing.T) {
	_, _, h := newAPI(t)
	rec := do(t, h, "POST", "/jobs", `{"prompt":"p"}`, map[string]string{"Idempotency-Key": "k"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAPISubmitIdempotent(t *testing.T) {
	_, _, h := newAPI(t)
	hdr := map[string]string{"Idempotency-Key": "dup"}

	rec := do(t, h, "POST", "/jobs", `{"model":"m","prompt":"p"}`, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first submit status = %d, want 201", rec.Code)
	}
	var first struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &first)
	if first.JobID == "" {
		t.Fatal("no job_id returned")
	}

	rec2 := do(t, h, "POST", "/jobs", `{"model":"m2","prompt":"other"}`, hdr)
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", rec2.Code)
	}
	var second struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &second)
	if second.JobID != first.JobID {
		t.Fatalf("replay job_id = %s, want %s", second.JobID, first.JobID)
	}
}

func TestAPIGetAndPosition(t *testing.T) {
	svc, _, h := newAPI(t)
	id := submitJob(t, svc, "g")

	rec := do(t, h, "GET", "/jobs/"+id, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var st Status
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.State != StateQueued || st.Position != 1 {
		t.Fatalf("status = %+v, want QUEUED position 1", st)
	}

	if r := do(t, h, "GET", "/jobs/nope", "", nil); r.Code != http.StatusNotFound {
		t.Fatalf("unknown get = %d, want 404", r.Code)
	}
}

func TestAPIResultLifecycle(t *testing.T) {
	svc, store, h := newAPI(t)
	id := submitJob(t, svc, "res")
	ctx := context.Background()

	// Not ready while queued.
	if r := do(t, h, "GET", "/jobs/"+id+"/result", "", nil); r.Code != http.StatusConflict {
		t.Fatalf("premature result = %d, want 409", r.Code)
	}

	// Drive it to SUCCEEDED directly through the store.
	store.ClaimNext(ctx)
	store.Succeed(ctx, id, "the output")

	r := do(t, h, "GET", "/jobs/"+id+"/result", "", nil)
	if r.Code != http.StatusOK {
		t.Fatalf("result status = %d", r.Code)
	}
	var body struct {
		Result string `json:"result"`
	}
	json.Unmarshal(r.Body.Bytes(), &body)
	if body.Result != "the output" {
		t.Fatalf("result = %q", body.Result)
	}
}

func TestAPIListFilter(t *testing.T) {
	svc, _, h := newAPI(t)
	a := submitJobWith(t, svc, "la", "alice")
	_ = submitJobWith(t, svc, "lb", "bob")

	rec := do(t, h, "GET", "/jobs?owner=alice", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var body struct {
		Jobs []Status `json:"jobs"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Jobs) != 1 || body.Jobs[0].ID != a {
		t.Fatalf("filtered list = %+v, want only %s", body.Jobs, a)
	}
}

func TestAPICancel(t *testing.T) {
	svc, _, h := newAPI(t)
	id := submitJob(t, svc, "can")
	rec := do(t, h, "POST", "/jobs/"+id+"/cancel", "", nil)
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
	store.ClaimNext(ctx)
	store.Succeed(ctx, id, "done")

	// A real server so the SSE handler's Flusher works.
	srv := httptest.NewServer(svc.Routes())
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(srv.URL + "/jobs/" + id + "/events")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()
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
