package job

import "sync"

// EventType labels a live Job event delivered over SSE.
type EventType string

const (
	// EventState: the Job changed lifecycle state.
	EventState EventType = "state"
	// EventProgress: a progress tick (tokens/elapsed) while RUNNING.
	EventProgress EventType = "progress"
	// EventPosition: the Job's place in the batch line moved.
	EventPosition EventType = "position"
	// EventDone: terminal; the result (if any) is ready to fetch.
	EventDone EventType = "done"
)

// Event is one SSE payload.
type Event struct {
	Type     EventType `json:"type"`
	State    State     `json:"state,omitempty"`
	Position int       `json:"position,omitempty"`
	Progress *Progress `json:"progress,omitempty"`
}

// bus fans Job events out to per-Job subscribers. Sends are non-blocking on a
// buffered channel: a slow SSE client drops ticks but never stalls the worker;
// it re-reads canonical state via GET /jobs/{id} when it reconnects.
type bus struct {
	mu   sync.Mutex
	subs map[string]map[int]chan Event
	next int
}

func newBus() *bus { return &bus{subs: make(map[string]map[int]chan Event)} }

func (b *bus) subscribe(id string) (int, <-chan Event) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	sub := b.next
	if b.subs[id] == nil {
		b.subs[id] = make(map[int]chan Event)
	}
	b.subs[id][sub] = ch
	return sub, ch
}

func (b *bus) unsubscribe(id string, sub int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.subs[id]; m != nil {
		if ch, ok := m[sub]; ok {
			close(ch)
			delete(m, sub)
		}
		if len(m) == 0 {
			delete(b.subs, id)
		}
	}
}

func (b *bus) publish(id string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[id] {
		select {
		case ch <- ev:
		default: // subscriber lagging; drop this tick
		}
	}
}
