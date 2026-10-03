package notify

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Event is one server-sent event payload.
type Event struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Body      string         `json:"body,omitempty"`
	Level     string         `json:"level"` // info | success | warning | error
	TicketID  *int64         `json:"ticket_id,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Target describes which connected users should receive an event.
type Target struct {
	// Users receives the event directly.
	Users []int64
	// Roles receives the event for every connected member of each role.
	Roles []string
	// AllStaff broadcasts to technicians and admins.
	AllStaff bool
	// Everyone broadcasts to all authenticated connections.
	Everyone bool
}

// client is one connected browser stream.
type client struct {
	userID    int64
	role      string
	ch        chan Event
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *client) close() {
	c.closeOnce.Do(func() { close(c.closed) })
}

// Hub fans events out to connected clients. It is safe for concurrent use.
//
// The hub is deliberately in-memory only: durable delivery lives in the
// notifications table, and the SSE stream is the low-latency push channel on
// top of it. That split means a reconnecting client cannot miss events.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}

	seq   uint64
	seqMu sync.Mutex
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{clients: make(map[*client]struct{})}
}

// Subscription is returned to a handler that owns an open SSE stream.
type Subscription struct {
	Events <-chan Event
	hub    *Hub
	c      *client
}

// Close removes the subscription and releases its channel.
func (s *Subscription) Close() {
	s.hub.remove(s.c)
}

// Done is closed when the subscription is torn down.
func (s *Subscription) Done() <-chan struct{} { return s.c.closed }

// Subscribe registers a connection for a user and returns its event stream.
//
// The buffer absorbs short bursts; if a client falls far enough behind to fill
// it, the slowest-consumer policy drops the connection rather than letting one
// stalled browser block the broadcaster or grow memory without bound. The
// client then reconnects and resyncs from the notifications table.
func (h *Hub) Subscribe(userID int64, role string, buffer int) *Subscription {
	if buffer <= 0 {
		buffer = 32
	}
	c := &client{
		userID: userID,
		role:   role,
		ch:     make(chan Event, buffer),
		closed: make(chan struct{}),
	}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	n := len(h.clients)
	h.mu.Unlock()

	slog.Debug("sse client connected", "user_id", userID, "role", role, "clients", n)

	return &Subscription{Events: c.ch, hub: h, c: c}
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	_, existed := h.clients[c]
	delete(h.clients, c)
	n := len(h.clients)
	h.mu.Unlock()

	if existed {
		c.close()
		slog.Debug("sse client disconnected", "user_id", c.userID, "clients", n)
	}
}

// Publish delivers an event to every matching client. Returns the number of
// connections written to.
func (h *Hub) Publish(t Target, ev Event) int {
	h.seqMu.Lock()
	h.seq++
	ev.ID = fmt.Sprintf("%d-%d", time.Now().Unix(), h.seq)
	h.seqMu.Unlock()

	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if ev.Level == "" {
		ev.Level = "info"
	}

	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if h.matches(c, t) {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	delivered := 0
	for _, c := range targets {
		select {
		case c.ch <- ev:
			delivered++
		case <-c.closed:
		default:
			// Buffer full: this connection is too slow. Disconnect it.
			slog.Warn("dropping slow sse client", "user_id", c.userID, "type", ev.Type)
			go h.remove(c)
		}
	}
	return delivered
}

func (h *Hub) matches(c *client, t Target) bool {
	if t.Everyone {
		return true
	}
	for _, id := range t.Users {
		if c.userID == id {
			return true
		}
	}
	for _, r := range t.Roles {
		if c.role == r {
			return true
		}
	}
	if t.AllStaff && (c.role == "technician" || c.role == "admin") {
		return true
	}
	return false
}

// Stats reports connection counts, used by the health endpoint.
func (h *Hub) Stats() (total int, staff int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		total++
		if c.role == "technician" || c.role == "admin" {
			staff++
		}
	}
	return total, staff
}

// Encode renders an event in the SSE wire format. Field names must omit the
// "data:" prefix per the EventSource specification.
func Encode(ev Event) ([]byte, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("marshal sse event: %w", err)
	}
	var b []byte
	if ev.ID != "" {
		b = append(b, "id: "...)
		b = append(b, ev.ID...)
		b = append(b, '\n')
	}
	if ev.Type != "" {
		b = append(b, "event: "...)
		b = append(b, ev.Type...)
		b = append(b, '\n')
	}
	// The data payload is a single line: JSON never contains a raw newline.
	b = append(b, "data: "...)
	b = append(b, payload...)
	b = append(b, '\n', '\n')
	return b, nil
}

// KeepAliveInterval is how often the server sends an SSE comment to keep
// intermediaries from closing an idle connection.
const KeepAliveInterval = 25 * time.Second

// KeepAlive is a comment-only frame; EventSource ignores it but it keeps the
// connection warm and lets the client detect a dead link.
var KeepAlive = []byte(": keep-alive\n\n")
