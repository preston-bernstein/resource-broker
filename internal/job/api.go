package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/preston-bernstein/ollama-resource-broker/internal/httpx"
)

// Routes returns the Job HTTP surface (ADR-0006). Mount it on the control plane
// at both "/jobs" and "/jobs/" so the list/submit and per-Job routes resolve.
func (s *Service) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.handleSubmit)
	mux.HandleFunc("GET /jobs", s.handleList)
	mux.HandleFunc("GET /jobs/{id}", s.handleGet)
	mux.HandleFunc("GET /jobs/{id}/result", s.handleResult)
	mux.HandleFunc("GET /jobs/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.handleCancel)
	return mux
}

func (s *Service) handleSubmit(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	var body struct {
		Model   string         `json:"model"`
		Prompt  string         `json:"prompt"`
		Source  string         `json:"source"`
		Owner   string         `json:"owner"`
		Options map[string]any `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	j, created, err := s.Submit(r.Context(), SubmitRequest{
		IdempotencyKey: key,
		Source:         body.Source,
		Owner:          body.Owner,
		Model:          body.Model,
		Prompt:         body.Prompt,
		Options:        body.Options,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "submit failed")
		return
	}
	code := http.StatusCreated
	if !created {
		code = http.StatusOK // idempotent replay of an existing Job
	}
	httpx.WriteJSON(w, code, map[string]string{"job_id": j.ID})
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	st, err := s.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

func (s *Service) handleResult(w http.ResponseWriter, r *http.Request) {
	result, err := s.Result(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id"), "result": result})
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{Source: q.Get("source"), Owner: q.Get("owner"), State: State(q.Get("state"))}
	jobs, err := s.List(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]Status, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, Status{
			ID: j.ID, State: j.State, Source: j.Source, Owner: j.Owner,
			Attempts: j.Attempts, Error: j.Error, CreatedAt: j.CreatedAt, FetchedAt: j.FetchedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	j, err := s.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": j.ID, "state": string(j.State)})
}

// handleEvents streams Job events as SSE: an initial snapshot, then live ticks
// until the Job is terminal or the client disconnects.
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, err := s.Get(r.Context(), id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsubscribe := s.Subscribe(id)
	defer unsubscribe()

	// Initial snapshot so a late subscriber learns current state immediately.
	sendEvent(w, flusher, Event{Type: EventState, State: st.State, Position: st.Position, Progress: st.Progress})
	if st.State.Terminal() {
		sendEvent(w, flusher, Event{Type: EventDone, State: st.State})
		return
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			sendEvent(w, flusher, ev)
			if ev.Type == EventDone {
				return
			}
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func sendEvent(w http.ResponseWriter, f http.Flusher, ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
	f.Flush()
}

func writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrNotReady):
		writeErr(w, http.StatusConflict, "job result not ready")
	default:
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	httpx.WriteJSON(w, code, map[string]string{"error": msg})
}
