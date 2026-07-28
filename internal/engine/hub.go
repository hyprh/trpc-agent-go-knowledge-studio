package engine

import (
	"sync"

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
)

// Event is a server-sent event pushed to connected browsers.
type Event struct {
	Type     string          `json:"type"` // task | progress | sources_changed
	BaseID   string          `json:"base_id"`
	TaskID   string          `json:"task_id,omitempty"`
	Task     *model.Task     `json:"task,omitempty"`
	Progress *model.Progress `json:"progress,omitempty"`
}

// Hub fans events out to subscribers.
type Hub struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

// NewHub creates an event hub.
func NewHub() *Hub { return &Hub{subs: map[int]chan Event{}} }

// Subscribe registers a listener and returns its channel plus an unsubscribe
// function.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
		h.mu.Unlock()
	}
}

// Publish delivers an event to every subscriber. A subscriber that is not
// keeping up drops the event rather than stalling the task runner.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
