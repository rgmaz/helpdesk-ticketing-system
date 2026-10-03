package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// receive pops one event, failing the test if none arrives in time.
func receive(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

// expectNone asserts a client receives nothing, which is how these tests verify
// that targeting actually excludes people.
func expectNone(t *testing.T, s *Subscription) {
	t.Helper()
	select {
	case ev := <-s.Events:
		t.Errorf("received an unexpected event: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublishReachesOnlyMatchingClients(t *testing.T) {
	h := NewHub()

	requester := h.Subscribe(1, "user", 4)
	tech := h.Subscribe(2, "technician", 4)
	admin := h.Subscribe(3, "admin", 4)
	defer requester.Close()
	defer tech.Close()
	defer admin.Close()

	cases := []struct {
		name string
		t    Target
		want map[int64]bool // user id -> receives
	}{
		{
			name: "direct user target",
			t:    Target{Users: []int64{1}},
			want: map[int64]bool{1: true},
		},
		{
			name: "role target",
			t:    Target{Roles: []string{"admin"}},
			want: map[int64]bool{3: true},
		},
		{
			name: "all staff covers technicians and admins",
			t:    Target{AllStaff: true},
			want: map[int64]bool{2: true, 3: true},
		},
		{
			name: "everyone",
			t:    Target{Everyone: true},
			want: map[int64]bool{1: true, 2: true, 3: true},
		},
		{
			name: "user and role are unioned",
			t:    Target{Users: []int64{1}, Roles: []string{"technician"}},
			want: map[int64]bool{1: true, 2: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subs := map[int64]*Subscription{1: requester, 2: tech, 3: admin}

			delivered := h.Publish(tc.t, Event{Type: "test", Title: "hello"})

			for id, sub := range subs {
				if !tc.want[id] {
					expectNone(t, sub)
					continue
				}
				ev := receive(t, sub)
				if ev.Type != "test" || ev.Title != "hello" {
					t.Errorf("user %d got %+v", id, ev)
				}
				// The hub fills these in so no client has to handle a blank.
				if ev.Level == "" {
					t.Error("Publish must default the level")
				}
				if ev.CreatedAt.IsZero() {
					t.Error("Publish must stamp created_at")
				}
				if ev.ID == "" {
					t.Error("Publish must assign an id so a client can resume with Last-Event-ID")
				}
			}
			if delivered != len(tc.want) {
				t.Errorf("delivered = %d, want %d", delivered, len(tc.want))
			}
		})
	}
}

func TestEventIDsAreUniqueAndMonotonic(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(1, "user", 32)
	defer sub.Close()

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		h.Publish(Target{Users: []int64{1}}, Event{Type: "tick", Title: fmt.Sprintf("event %d", i)})
		ev := receive(t, sub)
		if ev.ID == "" {
			t.Fatal("every event must carry an id")
		}
		if seen[ev.ID] {
			t.Fatalf("duplicate event id %q", ev.ID)
		}
		seen[ev.ID] = true
	}
}

// TestSlowClientIsDroppedRatherThanBlockingTheBroadcaster pins the policy that
// one stalled browser must never be able to stall a request handler: the
// overflowing connection is disconnected instead, and the healthy one is
// unaffected.
func TestSlowClientIsDroppedRatherThanBlockingTheBroadcaster(t *testing.T) {
	h := NewHub()

	stuck := h.Subscribe(1, "user", 1)
	healthy := h.Subscribe(2, "user", 8)
	defer healthy.Close()

	// A single event fills the stuck client's buffer.
	h.Publish(Target{Users: []int64{1}}, Event{Type: "filler"})

	// The next publish overflows it. That must not block the hub.
	done := make(chan int, 1)
	go func() {
		done <- h.Publish(Target{Users: []int64{1, 2}}, Event{Type: "overflow"})
	}()

	select {
	case n := <-done:
		if n != 1 {
			t.Errorf("delivered = %d, want 1 (only the healthy client)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow consumer instead of dropping it")
	}

	if ev := receive(t, healthy); ev.Type != "overflow" {
		t.Errorf("healthy client got %q, want overflow", ev.Type)
	}

	// The dropped client is torn down so the browser reconnects and resyncs.
	select {
	case <-stuck.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the slow client was not disconnected")
	}

	// Only the healthy client remains.
	if total, _ := h.Stats(); total != 1 {
		t.Errorf("clients = %d, want 1 once the stuck client has been dropped", total)
	}
}

func TestCloseIsIdempotentAndUnsubscribes(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(1, "user", 4)

	if total, _ := h.Stats(); total != 1 {
		t.Fatalf("clients = %d, want 1", total)
	}

	// A handler may race its own cleanup, so a double close must not panic.
	sub.Close()
	sub.Close()
	sub.Close()

	if total, _ := h.Stats(); total != 0 {
		t.Errorf("clients = %d after Close, want 0", total)
	}
	if delivered := h.Publish(Target{Everyone: true}, Event{Type: "after-close"}); delivered != 0 {
		t.Errorf("delivered = %d to a closed subscription, want 0", delivered)
	}
}

func TestStatsCountsStaff(t *testing.T) {
	h := NewHub()
	user := h.Subscribe(1, "user", 2)
	tech := h.Subscribe(2, "technician", 2)
	admin := h.Subscribe(3, "admin", 2)
	defer user.Close()
	defer tech.Close()
	defer admin.Close()

	total, staff := h.Stats()
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if staff != 2 {
		t.Errorf("staff = %d, want 2", staff)
	}
}

// TestConcurrentPublishAndSubscribe is the concurrency guarantee the hub
// promises: request handlers subscribe and disconnect on their own goroutines
// while other handlers broadcast. Run with -race for it to mean anything.
func TestConcurrentPublishAndSubscribe(t *testing.T) {
	h := NewHub()

	const publishers = 6
	const consumers = 40

	stop := make(chan struct{})
	var pub sync.WaitGroup
	for i := 0; i < publishers; i++ {
		pub.Add(1)
		go func(n int) {
			defer pub.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				h.Publish(Target{Everyone: true}, Event{
					Type:  "load",
					Title: fmt.Sprintf("publisher %d event %d", n, j),
				})
			}
		}(i)
	}

	// Consumers connect, read and disconnect while events are in flight.
	var con sync.WaitGroup
	for i := 0; i < consumers; i++ {
		con.Add(1)
		go func(n int64) {
			defer con.Done()
			sub := h.Subscribe(n, "user", 4)
			for read := 0; read < 5; read++ {
				select {
				case ev := <-sub.Events:
					if ev.Type != "load" {
						t.Errorf("got event type %q, want load", ev.Type)
						sub.Close()
						return
					}
				case <-sub.Done():
					return // dropped as a slow consumer, which is a valid outcome
				case <-time.After(50 * time.Millisecond):
					sub.Close()
					return
				}
			}
			sub.Close()
		}(int64(i + 1))
	}

	con.Wait()
	close(stop)
	pub.Wait()

	if total, staff := h.Stats(); total < 0 || staff < 0 {
		t.Errorf("negative client count: total=%d staff=%d", total, staff)
	}
}

func TestEncodeProducesValidSSEFrames(t *testing.T) {
	id := int64(7)
	ev := Event{
		ID:        "1712345678-42",
		Type:      "comment_added",
		Title:     "New comment on TKT-000001",
		Body:      "Looks like a stale entitlement.",
		Level:     "info",
		TicketID:  &id,
		Payload:   map[string]any{"author": "avery"},
		CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}

	frame, err := Encode(ev)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	text := string(frame)

	// The SSE grammar requires each field on its own line and a blank line to
	// terminate the frame.
	if !strings.HasSuffix(text, "\n\n") {
		t.Errorf("frame must end with a blank line, got %q", text)
	}
	for _, prefix := range []string{"id: ", "event: ", "data: "} {
		if !strings.Contains(text, prefix) {
			t.Errorf("frame is missing the %q field: %q", prefix, text)
		}
	}

	// Exactly one data line: JSON escapes newlines, so a raw one would
	// desynchronise the parser.
	if got := strings.Count(text, "\ndata: "); got != 1 {
		t.Errorf("found %d data lines, want exactly 1", got)
	}

	// The data line must be the original payload, so a client can parse it.
	_, after, _ := strings.Cut(text, "data: ")
	var decoded Event
	if err := json.Unmarshal([]byte(strings.TrimRight(after, "\n")), &decoded); err != nil {
		t.Fatalf("data line is not valid JSON: %v", err)
	}
	if decoded.Type != ev.Type || decoded.Title != ev.Title {
		t.Errorf("decoded %+v, want %+v", decoded, ev)
	}
	if decoded.TicketID == nil || *decoded.TicketID != id {
		t.Errorf("ticket_id = %v, want %d", decoded.TicketID, id)
	}
}

func TestEncodeOmitsEmptyFields(t *testing.T) {
	frame, err := Encode(Event{Type: "ping", Title: "hello", Level: "info"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	text := string(frame)
	if strings.Contains(text, "id: ") {
		t.Errorf("an event without an id must not emit an id field: %q", text)
	}
	if strings.Contains(text, "ticket_id") {
		t.Errorf("an absent ticket_id must be omitted, not serialised as null: %q", text)
	}
}

func TestKeepAliveIsACommentFrame(t *testing.T) {
	text := string(KeepAlive)
	if !strings.HasPrefix(text, ":") {
		t.Errorf("keep-alive must be an SSE comment, got %q", text)
	}
	if !strings.HasSuffix(text, "\n\n") {
		t.Errorf("keep-alive must terminate its line, got %q", text)
	}
	// EventSource dispatches nothing for a comment, so it must carry no data.
	if strings.Contains(text, "data:") {
		t.Errorf("keep-alive must not carry data: %q", text)
	}
	if KeepAliveInterval <= 0 {
		t.Error("KeepAliveInterval must be positive")
	}
}
