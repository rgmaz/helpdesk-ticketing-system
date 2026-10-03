package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

func TestCreateTicketStampsSLADeadlinesAndTimeline(t *testing.T) {
	h := newHarness(t)
	_, tech, _, _, _ := h.seed()
	// Filed by a technician rather than an end user: this test is about the
	// priority-to-policy wiring, so it needs a priority the requester cap would
	// otherwise clamp before the policy was ever consulted. The requester path
	// is covered by TestEndUserPriorityIsCapped.
	token := h.tokenFor(tech, models.RoleTechnician)

	res := h.do(http.MethodPost, "/api/v1/tickets", token, map[string]any{
		"title":       "The branch office router reboots every night",
		"description": "Every morning the router is back at factory defaults and the VPN is down.",
		"category":    "network",
		"priority":    "high",
	})
	if res.Status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", res.Status, res.Raw)
	}

	ticket := res.Body["ticket"].(map[string]any)
	if num, _ := ticket["ticket_number"].(string); num == "" {
		t.Error("expected a generated ticket number")
	}
	if ticket["status"] != "open" {
		t.Errorf("status = %v, want open", ticket["status"])
	}

	// A priority must map to the seeded SLA policy's deadlines. High is 1h/8h.
	// The wire format prefixes the deadline fields with "sla_".
	created := parseTime(t, ticket["created_at"])
	responseDue := parseTime(t, ticket["sla_response_due_at"])
	resolutionDue := parseTime(t, ticket["sla_resolution_due_at"])
	if got := responseDue.Sub(created); got != time.Hour {
		t.Errorf("response deadline = %v after creation, want 1h", got)
	}
	if got := resolutionDue.Sub(created); got != 8*time.Hour {
		t.Errorf("resolution deadline = %v after creation, want 8h", got)
	}

	// The creation must be on the timeline.
	events := h.do(http.MethodGet, "/api/v1/tickets/"+itoa(int64(ticket["id"].(float64)))+"/events",
		token, nil)
	if events.Status != http.StatusOK {
		t.Fatalf("events = %d, body = %s", events.Status, events.Raw)
	}
	if len(events.items()) == 0 {
		t.Error("expected at least a ticket_created event")
	}
}

func TestCreateTicketValidation(t *testing.T) {
	h := newHarness(t)
	_, _, _, user, _ := h.seed()
	token := h.tokenFor(user, models.RoleUser)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing title", map[string]any{"description": "no title", "category": "network"}, http.StatusBadRequest},
		{"blank title", map[string]any{"title": "   ", "description": "x", "category": "network"}, http.StatusBadRequest},
		{"unknown category", map[string]any{"title": "t", "description": "x", "category": "teapot"}, http.StatusBadRequest},
		{"unknown priority", map[string]any{"title": "t", "description": "x", "category": "network", "priority": "urgent"}, http.StatusBadRequest},
		{"unknown field", map[string]any{"title": "t", "description": "x", "category": "network", "nope": 1}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(http.MethodPost, "/api/v1/tickets", token, tc.body)
			if res.Status != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", res.Status, tc.want, res.Raw)
			}
		})
	}
}

// TestEndUserPriorityIsCapped covers the business rule that an unprivileged
// requester cannot escalate their own ticket by asking for a high priority.
func TestEndUserPriorityIsCapped(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()

	askHigh := map[string]any{
		"title": "Executive needs the report", "description": "Urgent.",
		"category": "access", "priority": "critical",
	}
	res := h.do(http.MethodPost, "/api/v1/tickets", h.tokenFor(user, models.RoleUser), askHigh)
	if res.Status != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
	}
	if got := res.Body["ticket"].(map[string]any)["priority"]; got != "medium" {
		t.Errorf("priority = %v, want medium (end users are capped at medium)", got)
	}

	// The cap has to cover the SLA as well as the label. Capping the stored
	// priority but resolving the policy from the *requested* one would hand the
	// requester critical deadlines on a ticket that says medium, which is both a
	// lie about the contract and a free bypass of the cap. The policy's own
	// targets are asserted rather than its id, because the ids depend on seed
	// order while the durations are the thing that matters.
	capped := res.Body["ticket"].(map[string]any)
	mediumPolicy := policyByPriority(t, h, models.PriorityMedium)
	// JSON numbers decode as float64, so the comparison is made in that domain
	// rather than against the model's int64.
	if got, want := capped["sla_policy_id"], float64(mediumPolicy.ID); got != want {
		t.Errorf("sla_policy_id = %v, want %v (the medium policy)", got, want)
	}
	if got := capped["sla_policy_name"]; got != mediumPolicy.Name {
		t.Errorf("sla_policy_name = %v, want %q", got, mediumPolicy.Name)
	}
	if got, want := capped["sla_response_due_at"], deadlineFor(t, h, models.PriorityMedium, true); !sameInstant(got, want) {
		t.Errorf("response deadline = %v, want the medium policy's %v", got, want)
	}
	if got, want := capped["sla_resolution_due_at"], deadlineFor(t, h, models.PriorityMedium, false); !sameInstant(got, want) {
		t.Errorf("resolution deadline = %v, want the medium policy's %v", got, want)
	}

	// Staff may file at any severity, and do get the matching policy.
	staffRes := h.do(http.MethodPost, "/api/v1/tickets", h.tokenFor(tech, models.RoleTechnician), askHigh)
	if staffRes.Status != http.StatusCreated {
		t.Fatalf("staff create = %d; body = %s", staffRes.Status, staffRes.Raw)
	}
	if got := staffRes.Body["ticket"].(map[string]any)["priority"]; got != "critical" {
		t.Errorf("staff priority = %v, want critical", got)
	}
	criticalPolicy := policyByPriority(t, h, models.PriorityCritical)
	if got := staffRes.Body["ticket"].(map[string]any)["sla_policy_name"]; got != criticalPolicy.Name {
		t.Errorf("staff sla_policy_name = %v, want %q", got, criticalPolicy.Name)
	}
}

// policyByPriority returns the active policy a ticket of that priority inherits.
func policyByPriority(t *testing.T, h *harness, priority models.Priority) models.SLAPolicy {
	t.Helper()
	policies, err := h.store.SLA.All(context.Background(), true)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	for _, p := range policies {
		if p.Priority == string(priority) {
			return p
		}
	}
	t.Fatalf("no active policy for priority %q", priority)
	return models.SLAPolicy{}
}

// deadlineFor computes what a ticket created now should be given as its deadline
// for the given priority. Recomputed rather than read off the response so the
// assertion is about the policy's targets, not about echoing back an id.
func deadlineFor(t *testing.T, h *harness, priority models.Priority, response bool) time.Time {
	t.Helper()
	policy := policyByPriority(t, h, priority)
	created := time.Now().UTC()

	minutes := policy.ResolutionMinutes
	if response {
		minutes = policy.ResponseMinutes
	}

	// Compared at second resolution: the deadline is stored as a second-precision
	// string, so a millisecond-precision expectation could never match.
	return created.Add(time.Duration(minutes) * time.Minute).Truncate(time.Second)
}

// sameInstant compares two wire timestamps tolerantly of representation.
func sameInstant(got any, want time.Time) bool {
	text, ok := got.(string)
	if !ok {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return false
	}
	// A minute of slack absorbs the seconds elapsing between the request and
	// this comparison without letting a wrong policy slip through.
	return parsed.After(want.Add(-time.Minute)) && parsed.Before(want.Add(time.Minute))
}

// TestStatusTransitionsFollowTheStateMachine pins the lifecycle rules: legal
// edges succeed, illegal ones are refused with 422 and leave the row untouched.
func TestStatusTransitionsFollowTheStateMachine(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()
	techToken := h.tokenFor(tech, models.RoleTechnician)

	newTicket := func(title string) int64 {
		t.Helper()
		return h.createTicketAs(user, title, "software", models.PriorityMedium).ID
	}

	t.Run("illegal transition is rejected", func(t *testing.T) {
		id := newTicket("Cannot open the expense form")

		// "reopened" is reserved for work that was previously resolved, so it is
		// unreachable from open.
		res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(id), techToken,
			map[string]any{"status": "reopened"})
		if res.Status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", res.Status, res.Raw)
		}
		if code := res.str("code"); code != "invalid_status_transition" {
			t.Errorf("code = %q, want invalid_status_transition", code)
		}

		// The rejected write must not have moved the ticket.
		after, err := h.store.Tickets.ByID(context.Background(), id)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if after.Status != models.StatusOpen {
			t.Errorf("status = %q, want open (the illegal move must not persist)", after.Status)
		}
	})

	t.Run("the full legal path is accepted", func(t *testing.T) {
		id := newTicket("Timesheet export is empty")

		for _, step := range []models.TicketStatus{
			models.StatusInProgress, models.StatusPending,
			models.StatusInProgress, models.StatusResolved, models.StatusClosed,
		} {
			res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(id), techToken,
				map[string]any{"status": string(step)})
			if res.Status != http.StatusOK {
				t.Fatalf("moving to %s = %d; body = %s", step, res.Status, res.Raw)
			}
			got := res.Body["ticket"].(map[string]any)["status"]
			if got != string(step) {
				t.Fatalf("status = %v, want %s", got, step)
			}
		}

		after, err := h.store.Tickets.ByID(context.Background(), id)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if after.ResolvedAt == nil {
			t.Error("resolved_at must be stamped when a ticket resolves")
		}
		if after.ClosedAt == nil {
			t.Error("closed_at must be stamped when a ticket closes")
		}
	})

	t.Run("a closed ticket only reopens", func(t *testing.T) {
		id := newTicket("Printer driver rolls back")

		shut := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(id), techToken,
			map[string]any{"status": "closed"})
		if shut.Status != http.StatusOK {
			t.Fatalf("closing = %d; body = %s", shut.Status, shut.Raw)
		}

		// Work in progress cannot be silently un-done from the closed state.
		for _, illegal := range []string{"open", "in_progress", "pending", "resolved"} {
			res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(id), techToken,
				map[string]any{"status": illegal})
			if res.Status != http.StatusUnprocessableEntity {
				t.Errorf("closed -> %s = %d, want 422; body = %s",
					illegal, res.Status, res.Raw)
			}
		}

		// Reopening is the one legal exit, and it clears the resolution stamp.
		reopen := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(id), techToken,
			map[string]any{"status": "reopened"})
		if reopen.Status != http.StatusOK {
			t.Fatalf("reopening = %d; body = %s", reopen.Status, reopen.Raw)
		}
		after, err := h.store.Tickets.ByID(context.Background(), id)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if after.Status != models.StatusReopened {
			t.Errorf("status = %q, want reopened", after.Status)
		}
		if after.ResolvedAt != nil || after.ClosedAt != nil {
			t.Error("reopening must clear the resolved and closed timestamps")
		}
	})

	t.Run("a resolved ticket awaits confirmation", func(t *testing.T) {
		// 'resolved' still counts as open work: the requester has not confirmed.
		res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(h.createTicketAs(user,
			"Wifi drops in the annex", "network", models.PriorityLow).ID), techToken,
			map[string]any{"status": "resolved"})
		if res.Status != http.StatusOK {
			t.Fatalf("resolving = %d; body = %s", res.Status, res.Raw)
		}

		list := h.do(http.MethodGet, "/api/v1/tickets?status=resolved", techToken, nil)
		if total := int(list.num("total")); total != 1 {
			t.Errorf("resolved filter total = %d, want 1", total)
		}

		// Every non-closed ticket in the fixture, including the resolved one.
		open := h.do(http.MethodGet, "/api/v1/tickets?open=true", techToken, nil)
		all := h.do(http.MethodGet, "/api/v1/tickets", techToken, nil)
		if open.num("total") != all.num("total")-1 {
			t.Errorf("open filter total = %v, want %v (resolved is open work, closed is not)",
				open.num("total"), all.num("total")-1)
		}
	})
}

func TestReopenResetsTheResolutionClock(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()
	techToken := h.tokenFor(tech, models.RoleTechnician)

	ticket := h.createTicketAs(user, "Shared drive is inaccessible", "network", models.PriorityHigh)
	if res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(ticket.ID), techToken,
		map[string]any{"status": "resolved"}); res.Status != http.StatusOK {
		t.Fatalf("resolve: %d %s", res.Status, res.Raw)
	}

	// The requester reports the fix did not work, so the ticket reopens. The
	// resolution deadline must move out again, otherwise a reopened ticket is
	// instantly breached.
	if res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(ticket.ID),
		h.tokenFor(user, models.RoleUser), map[string]any{"status": "reopened"}); res.Status != http.StatusOK {
		t.Fatalf("reopen: %d %s", res.Status, res.Raw)
	}

	after, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.ResolvedAt != nil {
		t.Error("resolved_at must be cleared on reopen")
	}
	if after.ResolutionDueAt == nil || !after.ResolutionDueAt.Time.After(after.CreatedAt.Time) {
		t.Error("a reopened ticket needs a live resolution deadline")
	}
	if after.ResolutionBreached {
		t.Error("a reopened ticket must not be flagged as breached")
	}
}

func TestStaffPublicCommentStopsTheResponseClock(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()

	ticket := h.createTicketAs(user, "Laptop will not charge", "hardware", models.PriorityHigh)
	if ticket.FirstResponseAt != nil {
		t.Fatal("a fresh ticket must not have a first_response_at")
	}

	// An internal note is staff thinking out loud and must not count as a
	// response to the requester.
	internal := h.do(http.MethodPost, "/api/v1/tickets/"+itoa(ticket.ID)+"/comments",
		h.tokenFor(tech, models.RoleTechnician),
		map[string]any{"body": "Probably the same charger pin issue as last week.", "internal": true})
	if internal.Status != http.StatusCreated {
		t.Fatalf("internal note = %d, body = %s", internal.Status, internal.Raw)
	}
	after, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.FirstResponseAt != nil {
		t.Error("an internal note must not stop the response clock")
	}

	// A public reply does.
	public := h.do(http.MethodPost, "/api/v1/tickets/"+itoa(ticket.ID)+"/comments",
		h.tokenFor(tech, models.RoleTechnician),
		map[string]any{"body": "Sorry about this, I am booking a replacement charger."})
	if public.Status != http.StatusCreated {
		t.Fatalf("public reply = %d, body = %s", public.Status, public.Raw)
	}
	after, err = h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.FirstResponseAt == nil {
		t.Error("a public staff reply must stamp first_response_at")
	}
}

func TestEndUserCannotChangePriorityOrAssignee(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()

	ticket := h.createTicketAs(user, "Need a second monitor", "hardware", models.PriorityMedium)
	techUser, err := h.store.Users.ByUsername(context.Background(), tech)
	if err != nil {
		t.Fatalf("load technician: %v", err)
	}
	userToken := h.tokenFor(user, models.RoleUser)

	// Priority elevation is a staff action.
	res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(ticket.ID), userToken,
		map[string]any{"priority": "critical"})
	if res.Status != http.StatusOK && res.Status != http.StatusForbidden {
		t.Fatalf("priority change = %d; body = %s", res.Status, res.Raw)
	}
	after, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if res.Status == http.StatusOK && after.Priority == models.PriorityCritical {
		t.Error("an end user elevated their own ticket to critical")
	}

	// Self-assignment would let an end user dodge the queue.
	resign := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(ticket.ID), userToken,
		map[string]any{"assignee_id": techUser.ID})
	if resign.Status != http.StatusOK && resign.Status != http.StatusForbidden {
		t.Fatalf("assign = %d; body = %s", resign.Status, resign.Raw)
	}
	after, err = h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if res.Status == http.StatusOK && after.AssigneeID != nil && *after.AssigneeID == techUser.ID {
		t.Error("an end user assigned a technician at their own request")
	}
}

func TestListFiltersAndSorting(t *testing.T) {
	h := newHarness(t)
	_, tech, _, _, _ := h.seed()
	techToken := h.tokenFor(tech, models.RoleTechnician)

	// A small, known set with distinguishable attributes. These are filed by an
	// administrator so the priority is not capped to medium.
	for _, spec := range []struct {
		title, category string
		priority        models.Priority
	}{
		{"Alpha: printer offline on floor 2", "hardware", models.PriorityLow},
		{"Beta: VPN drops during video calls", "network", models.PriorityCritical},
		{"Gamma: expense form will not submit", "software", models.PriorityMedium},
		{"Delta: payroll export is missing rows", "software", models.PriorityHigh},
	} {
		h.createTicketAsStaff(spec.title, spec.category, spec.priority)
	}

	t.Run("by status", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets?status=open", techToken, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
		}
		if total := int(res.num("total")); total != 4 {
			t.Errorf("open total = %d, want 4", total)
		}
	})

	t.Run("by category", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets?category=software", techToken, nil)
		if total := int(res.num("total")); total != 2 {
			t.Errorf("software total = %d, want 2", total)
		}
	})

	t.Run("by priority, highest first", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets?sort_by=priority&sort_dir=desc", techToken, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
		}
		items := res.items()
		if len(items) != 4 {
			t.Fatalf("got %d items, want 4", len(items))
		}
		// Priority is an enum, so this must sort by rank, not alphabetically.
		want := []string{"critical", "high", "medium", "low"}
		for i, expected := range want {
			if got, _ := items[i]["priority"].(string); got != expected {
				t.Errorf("item %d priority = %q, want %q (rank order, not alphabetical)",
					i, got, expected)
			}
		}
	})

	t.Run("by multiple priorities", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets?priority=high,critical", techToken, nil)
		if total := int(res.num("total")); total != 2 {
			t.Errorf("total = %d, want 2", total)
		}
	})

	t.Run("by multiple statuses", func(t *testing.T) {
		techA, err := h.store.Users.ByUsername(context.Background(), tech)
		if err != nil {
			t.Fatalf("load technician: %v", err)
		}
		list := h.do(http.MethodGet, "/api/v1/tickets", techToken, nil)
		id := int64(list.items()[0]["id"].(float64))
		for _, step := range []string{"in_progress", "pending"} {
			res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(id), techToken,
				map[string]any{"status": step})
			if res.Status != http.StatusOK {
				t.Fatalf("%s: %d %s", step, res.Status, res.Raw)
			}
		}
		_ = techA

		res := h.do(http.MethodGet, "/api/v1/tickets?status=in_progress,pending", techToken, nil)
		if total := int(res.num("total")); total != 1 {
			t.Errorf("total = %d, want 1", total)
		}
	})

	t.Run("by breached", func(t *testing.T) {
		// Breach flags are set by the SLA engine, not by the filter, so the
		// engine has to be running for this filter to have anything to find.
		list := h.do(http.MethodGet, "/api/v1/tickets", techToken, nil)
		id := int64(list.items()[0]["id"].(float64))
		makeOverdue(t, h, id, time.Hour)

		h.runEngine()
		waitForBreach(t, h)

		res := h.do(http.MethodGet, "/api/v1/tickets?breached=true", techToken, nil)
		if total := int(res.num("total")); total != 1 {
			t.Errorf("breached total = %d, want 1", total)
		}
		clean := h.do(http.MethodGet, "/api/v1/tickets?breached=false", techToken, nil)
		if total := int(clean.num("total")); total != 3 {
			t.Errorf("clean total = %d, want 3", total)
		}
		escalated := h.do(http.MethodGet, "/api/v1/tickets?escalated=true", techToken, nil)
		if total := int(escalated.num("total")); total != 1 {
			t.Errorf("escalated total = %d, want 1", total)
		}
	})

	t.Run("by assignee", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets?assignee=none", techToken, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
		}
		// The router assigns automatically, so nothing should be unassigned.
		if total := int(res.num("total")); total != 0 {
			t.Errorf("unassigned total = %d, want 0 (auto-assignment should cover every ticket)", total)
		}
	})

	t.Run("by created range", func(t *testing.T) {
		today := time.Now().UTC().Format("2006-01-02")
		res := h.do(http.MethodGet, "/api/v1/tickets?created_from="+today, techToken, nil)
		if total := int(res.num("total")); total != 4 {
			t.Errorf("total = %d, want 4", total)
		}
		res = h.do(http.MethodGet, "/api/v1/tickets?created_from=1999-01-01&created_to=1999-12-31", techToken, nil)
		if total := int(res.num("total")); total != 0 {
			t.Errorf("total = %d, want 0 for an out-of-range window", total)
		}
	})

	t.Run("bad filter values are rejected", func(t *testing.T) {
		// Every value is URL-encoded so the request itself stays well-formed
		// and the assertion is about the filter, not the transport.
		for _, q := range []string{
			"?status=frozen",
			"?priority=urgent",
			"?sort_by=" + urlEscape("id; DROP TABLE tickets"),
			"?sort_dir=sideways",
			"?page=abc",
			"?limit=abc",
			"?created_from=yesterday",
			"?q=" + urlEscape(`title:"unclosed`),
		} {
			res := h.do(http.MethodGet, "/api/v1/tickets"+q, techToken, nil)
			if res.Status != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400; body = %s", q, res.Status, truncate(res.Raw, 200))
			}
		}
		// An injection attempt must not have damaged anything.
		all := h.do(http.MethodGet, "/api/v1/tickets", techToken, nil)
		if total := int(all.num("total")); total != 4 {
			t.Errorf("total = %d after an injection attempt, want 4", total)
		}
	})

	t.Run("out-of-range paging is clamped rather than refused", func(t *testing.T) {
		// Asking for too many rows, or a page before the start, is a client
		// preference rather than a fault: clamping beats an error.
		for _, q := range []string{"?limit=0", "?limit=9999", "?page=-1", "?page=99999"} {
			res := h.do(http.MethodGet, "/api/v1/tickets"+q, techToken, nil)
			if res.Status != http.StatusOK {
				t.Errorf("%s = %d, want 200; body = %s", q, res.Status, truncate(res.Raw, 200))
			}
		}
	})

	t.Run("every advertised sort field works", func(t *testing.T) {
		meta := h.do(http.MethodGet, "/api/v1/meta", "", nil)
		fields, _ := meta.Body["sort_fields"].([]any)
		if len(fields) == 0 {
			t.Fatalf("meta advertised no sort fields: %s", truncate(meta.Raw, 300))
		}
		for _, raw := range fields {
			field, _ := raw.(string)
			for _, dir := range []string{"asc", "desc"} {
				res := h.do(http.MethodGet,
					"/api/v1/tickets?sort_by="+field+"&sort_dir="+dir, techToken, nil)
				if res.Status != http.StatusOK {
					t.Errorf("sort_by=%s&sort_dir=%s = %d, want 200; body = %s",
						field, dir, res.Status, truncate(res.Raw, 200))
				}
			}
		}
	})

	t.Run("status counts accompany the page", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets", techToken, nil)
		counts, ok := res.Body["counts"].(map[string]any)
		if !ok {
			t.Fatalf("expected a counts object, got %s", res.Raw)
		}
		sum := 0
		for _, v := range counts {
			sum += int(v.(float64))
		}
		if sum != 4 {
			t.Errorf("status counts sum to %d, want 4; counts = %v", sum, counts)
		}
	})
}

func TestPaginationIsStableAndComplete(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()
	techToken := h.tokenFor(tech, models.RoleTechnician)

	const total = 7
	for i := 0; i < total; i++ {
		h.createTicketAs(user, "Paged ticket "+string(rune('A'+i)), "general", models.PriorityMedium)
	}

	seen := map[float64]bool{}
	page := 0
	for {
		res := h.do(http.MethodGet, "/api/v1/tickets?limit=3&page="+itoa(int64(page)), techToken, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("page %d = %d; body = %s", page, res.Status, res.Raw)
		}
		if got := int(res.num("total")); got != total {
			t.Fatalf("total = %d, want %d", got, total)
		}
		items := res.items()
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			id := item["id"].(float64)
			if seen[id] {
				t.Fatalf("ticket %v appeared on two pages", id)
			}
			seen[id] = true
		}
		if len(items) < 3 {
			break
		}
		page++
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("saw %d distinct tickets across pages, want %d", len(seen), total)
	}
}

func TestFTS5SearchFindsTicketsAndAttachesSnippets(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()
	techToken := h.tokenFor(tech, models.RoleTechnician)

	h.createTicketAs(user, "Docking station drops the external monitor", "hardware", models.PriorityMedium)
	h.createTicketAs(user, "Payroll export misses the final column", "software", models.PriorityHigh)
	h.createTicketAs(user, "Reception printer jams when duplex printing", "hardware", models.PriorityLow)

	t.Run("matches on description", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets/search?q=printer", techToken, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
		}
		if total := int(res.num("total")); total != 1 {
			t.Fatalf("total = %d, want 1", total)
		}
		items := res.items()
		if len(items) != 1 {
			t.Fatalf("got %d items, want 1", len(items))
		}
		// The snippet must come from FTS5 so the UI can show the match context.
		snippet, _ := items[0]["snippet"].(string)
		if snippet == "" {
			t.Errorf("expected a highlighted snippet, got none: %s", res.Raw)
		}
	})

	t.Run("matches on title", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets/search?q=docking", techToken, nil)
		if total := int(res.num("total")); total != 1 {
			t.Errorf("total = %d, want 1", total)
		}
	})

	t.Run("inflections are stemmed", func(t *testing.T) {
		// The porter tokenizer reduces "printing" to "print", so a search for
		// the root finds the inflected form in the indexed text.
		res := h.do(http.MethodGet, "/api/v1/tickets/search?q=print", techToken, nil)
		if total := int(res.num("total")); total == 0 {
			t.Error("expected the stem of \"printing\" to match the text")
		}
	})

	t.Run("no match returns an empty page, not an error", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets/search?q=zzzznotpresent", techToken, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
		}
		if total := int(res.num("total")); total != 0 {
			t.Errorf("total = %d, want 0", total)
		}
	})

	t.Run("fts5 syntax in the query is rejected, never a 500", func(t *testing.T) {
		// Each of these is either a parse error or an operator. Passing them
		// through would make MATCH fail, and an unhandled query error must not
		// reach the client as a 500.
		for _, q := range []string{
			`title:"x`, `a OR`, `a AND`, `NOT print`, `(print`, `print*`,
			`print -duplex`, `*`, `^anchor`, `col:x`, `NEAR(a b)`, `print"`,
		} {
			res := h.do(http.MethodGet, "/api/v1/tickets/search?q="+urlEscape(q), techToken, nil)
			if res.Status != http.StatusBadRequest {
				t.Errorf("query %q = %d, want 400; body = %s", q, res.Status, truncate(res.Raw, 200))
			}
		}
	})

	t.Run("multi-word queries are treated as an AND of terms", func(t *testing.T) {
		both := h.do(http.MethodGet, "/api/v1/tickets/search?q="+urlEscape("docking monitor"), techToken, nil)
		if total := int(both.num("total")); total != 1 {
			t.Errorf("both terms present: total = %d, want 1", total)
		}
		one := h.do(http.MethodGet, "/api/v1/tickets/search?q="+urlEscape("docking payroll"), techToken, nil)
		if total := int(one.num("total")); total != 0 {
			t.Errorf("terms split across tickets: total = %d, want 0", total)
		}
	})

	t.Run("an over-long query is rejected", func(t *testing.T) {
		long := strings.Repeat("word ", 60)
		res := h.do(http.MethodGet, "/api/v1/tickets/search?q="+urlEscape(long), techToken, nil)
		if res.Status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.Status)
		}
	})

	t.Run("empty query is rejected", func(t *testing.T) {
		res := h.do(http.MethodGet, "/api/v1/tickets/search?q=%20", techToken, nil)
		if res.Status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.Status)
		}
	})
}

// TestSearchIndexStaysCurrentWithEdits proves the FTS5 triggers keep the index in
// sync, rather than only searching what existed at migration time.
func TestSearchIndexStaysCurrentWithEdits(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()
	techToken := h.tokenFor(tech, models.RoleTechnician)

	ticket := h.createTicketAs(user, "Original subject line", "general", models.PriorityMedium)

	// Re-categorising is a supported edit, and category is one of the indexed
	// columns. The new value must become findable and the old one must not.
	if res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(ticket.ID), techToken,
		map[string]any{"category": "database"}); res.Status != http.StatusOK {
		t.Fatalf("recategorise = %d; body = %s", res.Status, res.Raw)
	}

	found := h.do(http.MethodGet, "/api/v1/tickets/search?q=database", techToken, nil)
	if total := int(found.num("total")); total != 1 {
		t.Errorf("new category is not searchable: total = %d, want 1; body = %s", total, found.Raw)
	}

	stale := h.do(http.MethodGet, "/api/v1/tickets/search?q=general", techToken, nil)
	if total := int(stale.num("total")); total != 0 {
		t.Errorf("the old category is still in the index: total = %d, want 0", total)
	}

	// Resolution notes are indexed too, so recording a fix makes it searchable.
	if res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(ticket.ID), techToken,
		map[string]any{"resolution_notes": "Cleared a stale entitlement record"}); res.Status != http.StatusOK {
		t.Fatalf("set resolution notes = %d; body = %s", res.Status, res.Raw)
	}
	notes := h.do(http.MethodGet, "/api/v1/tickets/search?q=entitlement", techToken, nil)
	if total := int(notes.num("total")); total != 1 {
		t.Errorf("resolution notes are not searchable: total = %d, want 1", total)
	}
}

func TestRoutingAssignsToTheLeastLoadedTechnician(t *testing.T) {
	h := newHarness(t)
	_, techA, techB, user, _ := h.seed()

	techAUser, _ := h.store.Users.ByUsername(context.Background(), techA)
	techBUser, _ := h.store.Users.ByUsername(context.Background(), techB)

	// One ticket for tech B, so tech A must receive the next one.
	existing := h.createTicketAs(user, "Pre-existing load for technician B", "general", models.PriorityMedium)
	h.assignTo(existing.ID, techBUser.ID)

	next := h.createTicketAs(user, "Should land on the quiet technician", "general", models.PriorityMedium)
	if next.AssigneeID == nil {
		t.Fatal("the router must assign a ticket automatically")
	}
	if *next.AssigneeID != techAUser.ID {
		t.Errorf("assigned to user %d, want the less-loaded technician %d", *next.AssigneeID, techAUser.ID)
	}

	// Load is now balanced, so a third ticket goes to the lower id on a tie.
	third := h.createTicketAs(user, "Tie-break check", "general", models.PriorityMedium)
	if third.AssigneeID == nil {
		t.Fatal("expected an assignment")
	}
	if *third.AssigneeID != techAUser.ID {
		t.Errorf("tie-break assigned to %d, want %d (lowest id wins on equal load)",
			*third.AssigneeID, techAUser.ID)
	}
}

// TestUrgentTicketsAvoidOverloadedTechnicians covers the routing refinement: for
// high and critical work, an overdue-free technician is preferred over a
// less-loaded one who is already slipping.
func TestUrgentTicketsAvoidOverloadedTechnicians(t *testing.T) {
	h := newHarness(t)
	_, techA, techB, user, _ := h.seed()

	techAUser, _ := h.store.Users.ByUsername(context.Background(), techA)
	techBUser, _ := h.store.Users.ByUsername(context.Background(), techB)

	// Give tech A a lower raw load but with an already-overdue ticket.
	overdue := h.createTicketAs(user, "Old overdue work for technician A", "general", models.PriorityMedium)
	h.assignTo(overdue.ID, techAUser.ID)
	makeOverdue(t, h, overdue.ID, 30*time.Minute)

	urgent := h.createTicketAs(user, "Production is down", "network", models.PriorityCritical)
	if urgent.AssigneeID == nil {
		t.Fatal("expected an assignment")
	}
	if *urgent.AssigneeID != techBUser.ID {
		t.Errorf("critical ticket assigned to %d, want the technician with no overdue work %d",
			*urgent.AssigneeID, techBUser.ID)
	}
}

func TestRoutingLeavesTicketsUnassignedWithoutTechnicians(t *testing.T) {
	h := newHarness(t)
	// No staff at all: an administrator could take the work, but with only an
	// end user on the system there is nobody to route to.
	h.createUser("requester", models.RoleUser)

	ticket := h.createTicketAs("requester", "Nobody is on duty", "general", models.PriorityMedium)
	if ticket.AssigneeID != nil {
		t.Errorf("assigned to %d, want nobody: the router must degrade gracefully with an empty pool",
			*ticket.AssigneeID)
	}

	// The unassigned queue must be reachable so the ticket is not stranded.
	token := h.tokenFor("requester", models.RoleUser)
	res := h.do(http.MethodGet, "/api/v1/tickets?assignee=none", token, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("unassigned queue = %d; body = %s", res.Status, res.Raw)
	}
	if total := int(res.num("total")); total != 1 {
		t.Errorf("unassigned queue total = %d, want 1", total)
	}

	// An administrator sees the same ticket in the unassigned queue.
	admin := h.createUser("admin", models.RoleAdmin)
	adminRes := h.do(http.MethodGet, "/api/v1/tickets?unassigned=true",
		h.tokenFor(admin.Username, models.RoleAdmin), nil)
	if total := int(adminRes.num("total")); total != 1 {
		t.Errorf("admin unassigned queue total = %d, want 1", total)
	}
}

// TestAdministratorsAreNotAutoRoutedWork documents the routing pool boundary:
// new tickets go to the technician pool, never to an administrator.
//
// Administrators are escalation destinations rather than front-line handlers, so
// letting the router hand them routine tickets would defeat least-load routing
// and bury them in work they are meant to supervise.
func TestAdministratorsAreNotAutoRoutedWork(t *testing.T) {
	h := newHarness(t)
	admin := h.createUser("admin", models.RoleAdmin)

	ticket := h.createTicketAsStaff("No technicians on shift", "general", models.PriorityMedium)
	if ticket.AssigneeID != nil {
		t.Errorf("assigned to %d, want nobody: administrators are not in the routing pool",
			*ticket.AssigneeID)
	}
	_ = admin

	// An administrator can still pick the ticket up by hand.
	shut := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(ticket.ID),
		h.tokenFor("admin", models.RoleAdmin), map[string]any{"assignee_id": 1})
	if shut.Status != http.StatusOK {
		t.Errorf("manual assignment = %d, want 200; body = %s", shut.Status, truncate(shut.Raw, 200))
	}
}

func TestTechnicianDirectoryIsSortedByLoad(t *testing.T) {
	h := newHarness(t)
	_, techA, techB, user, _ := h.seed()
	techAUser, _ := h.store.Users.ByUsername(context.Background(), techA)
	techBUser, _ := h.store.Users.ByUsername(context.Background(), techB)

	// Two tickets for tech B, one for tech A.
	for i := 0; i < 2; i++ {
		tk := h.createTicketAs(user, "Load for technician B", "general", models.PriorityMedium)
		h.assignTo(tk.ID, techBUser.ID)
	}
	tk := h.createTicketAs(user, "Load for technician A", "general", models.PriorityMedium)
	h.assignTo(tk.ID, techAUser.ID)

	res := h.do(http.MethodGet, "/api/v1/technicians",
		h.tokenFor(adminUsername(h), models.RoleAdmin), nil)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
	}
	items := res.items()
	if len(items) != 2 {
		t.Fatalf("got %d technicians, want 2", len(items))
	}
	first, _ := items[0]["active_tickets"].(float64)
	second, _ := items[1]["active_tickets"].(float64)
	if first > second {
		t.Errorf("directory is not sorted by load: %v then %v", first, second)
	}
	// The busy technician must also carry the larger count, not just appear
	// later by accident of the tie-break.
	if first != 1 || second != 2 {
		t.Errorf("active_tickets = %v then %v, want 1 then 2 (least loaded first)", first, second)
	}
}

func TestMetaDescribesTheBackendVocabulary(t *testing.T) {
	h := newHarness(t)
	res := h.do(http.MethodGet, "/api/v1/meta", "", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("meta must be public; status = %d, body = %s", res.Status, res.Raw)
	}

	statuses, ok := res.Body["statuses"].([]any)
	if !ok || len(statuses) == 0 {
		t.Fatalf("expected a statuses array, got %s", res.Raw)
	}
	first := statuses[0].(map[string]any)
	for _, field := range []string{"value", "label", "terminal", "next"} {
		if _, present := first[field]; !present {
			t.Errorf("status entry is missing %q", field)
		}
	}

	// The frontend renders its action buttons from this payload, so it has to
	// agree with the server's state machine exactly.
	open := findStatus(t, statuses, "open")
	next, _ := open["next"].([]any)
	for _, candidate := range next {
		if !models.StatusOpen.CanTransitionTo(models.TicketStatus(candidate.(string))) {
			t.Errorf("meta advertises open -> %v, which the state machine forbids", candidate)
		}
	}
	// Conversely, an edge the state machine allows must be offered, or the UI
	// hides a valid action.
	offered := map[string]bool{}
	for _, n := range next {
		offered[n.(string)] = true
	}
	if !offered["resolved"] {
		t.Error("meta omits open -> resolved, so the UI cannot offer Resolve")
	}

	closed := findStatus(t, statuses, "closed")
	closedNext, _ := closed["next"].([]any)
	if len(closedNext) != 1 || closedNext[0] != "reopened" {
		t.Errorf("closed.next = %v, want [reopened]", closedNext)
	}

	priorities, ok := res.Body["priorities"].([]any)
	if !ok || len(priorities) != 4 {
		t.Fatalf("expected 4 priorities, got %s", res.Raw)
	}

	categories, ok := res.Body["categories"].([]any)
	if !ok || len(categories) == 0 {
		t.Fatalf("expected the category list, got %s", res.Raw)
	}
}

// TestSLAEngineFlagsBreachesAndEscalates covers breach detection, the level-1
// priority bump, and the guarantee that terminal tickets are never touched.
func TestSLAEngineFlagsBreachesAndEscalates(t *testing.T) {
	h := newHarness(t)
	_, techA, _, _, _ := h.seed()

	techAUser, _ := h.store.Users.ByUsername(context.Background(), techA)
	// Filed by staff so "high" survives the end-user priority cap.
	ticket := h.createTicketAsStaff("Security incident on the finance file share",
		"security", models.PriorityHigh)
	h.assignTo(ticket.ID, techAUser.ID)

	// High priority has a 60-minute escalation interval, so a ticket that has
	// only just missed its deadline earns level 1, not the full escalation.
	makeOverdue(t, h, ticket.ID, time.Minute)

	h.runEngine()
	waitForBreach(t, h)

	after, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !after.ResolutionBreached {
		t.Error("resolution_breached was not set on the overdue ticket")
	}
	if !after.ResponseBreached {
		t.Error("response_breached was not set on the overdue ticket")
	}
	if after.EscalationLevel != 1 {
		t.Errorf("escalation level = %d, want 1 for a first-time breach", after.EscalationLevel)
	}
	// Level 1 raises priority one band.
	if after.Priority != models.PriorityCritical {
		t.Errorf("priority = %q, want critical after level-1 escalation", after.Priority)
	}

	// The escalation must be on the timeline, not just in a column.
	events := h.do(http.MethodGet, "/api/v1/tickets/"+itoa(ticket.ID)+"/events",
		h.tokenFor(techA, models.RoleTechnician), nil)
	seen := map[string]bool{}
	for _, e := range events.items() {
		seen[e["type"].(string)] = true
	}
	for _, want := range []string{
		string(store.EventResponseBreach),
		string(store.EventResolutionBreach),
		string(store.EventEscalation),
		string(store.EventPriorityChanged),
	} {
		if !seen[want] {
			t.Errorf("timeline has no %q event; got %v", want, seen)
		}
	}

	// The engine must leave terminal tickets alone.
	closedTicket := h.createTicketAsStaff("Closed long ago", "general", models.PriorityLow)
	for _, step := range []string{"resolved", "closed"} {
		if res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(closedTicket.ID),
			h.tokenFor(techA, models.RoleTechnician), map[string]any{"status": step}); res.Status != http.StatusOK {
			t.Fatalf("%s: %d %s", step, res.Status, res.Raw)
		}
	}
	closedBefore, _ := h.store.Tickets.ByID(context.Background(), closedTicket.ID)
	waitForScan(t, h, 3)
	closedAfter, _ := h.store.Tickets.ByID(context.Background(), closedTicket.ID)
	if closedAfter.ResolutionBreached != closedBefore.ResolutionBreached {
		t.Error("the engine flagged a closed ticket")
	}
	if closedAfter.EscalationLevel != closedBefore.EscalationLevel {
		t.Error("the engine escalated a closed ticket")
	}
}

// TestEscalationIsGradedByTimeOverdue is the core escalation rule: a breach
// raises priority immediately, but pulling in an administrator waits until the
// ticket has been overdue for a further escalation interval.
//
// Without the interval a single breach would page an administrator in the same
// instant as the first warning, leaving nobody time to react.
func TestEscalationIsGradedByTimeOverdue(t *testing.T) {
	h := newHarness(t)
	adminName, _, _, _, _ := h.seed()
	admin, err := h.store.Users.ByUsername(context.Background(), adminName)
	if err != nil {
		t.Fatalf("load admin: %v", err)
	}

	// Critical priority: 30-minute escalation interval, two levels permitted.
	// Filed as staff because an end user's request for critical would be capped
	// to medium, and this test is specifically about the critical policy's
	// grading. See createTicketAsStaff.
	ticket := h.createTicketAsStaff("Payment gateway unreachable", "network", models.PriorityCritical)
	makeOverdue(t, h, ticket.ID, 2*time.Minute)

	h.runEngine()
	waitForBreach(t, h)

	after, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.EscalationLevel != 1 {
		t.Errorf("escalation level = %d after 2 minutes overdue, want 1 (interval is 30m)",
			after.EscalationLevel)
	}
	// Level 1 must not have pulled an administrator in yet.
	if after.AssigneeID != nil && *after.AssigneeID == admin.ID {
		t.Error("level 1 reassigned to an administrator; that is the level-2 action")
	}

	// Push the deadline further into the past to cross the escalation interval.
	makeOverdue(t, h, ticket.ID, 45*time.Minute)
	waitForScan(t, h, 2)

	escalated, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if escalated.EscalationLevel != 2 {
		t.Fatalf("escalation level = %d after 45 minutes overdue, want 2", escalated.EscalationLevel)
	}
	if escalated.AssigneeID == nil || *escalated.AssigneeID != admin.ID {
		t.Error("level 2 must reassign the ticket to an administrator")
	}
}

// TestSLAScanIsIdempotent ensures repeated passes do not re-breach or
// re-escalate the same ticket, which would spam the technician's queue and
// inflate the engine's counters.
func TestSLAScanIsIdempotent(t *testing.T) {
	h := newHarness(t)
	_, _, _, user, _ := h.seed()

	ticket := h.createTicketAs(user, "Overdue and unattended", "general", models.PriorityCritical)
	makeOverdue(t, h, ticket.ID, 2*time.Minute)

	h.runEngine()
	waitForBreach(t, h)

	first, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The engine scans every minute; force several passes to prove the state is
	// stable rather than merely slow to change.
	waitForScan(t, h, 4)

	second, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if first.EscalationLevel != second.EscalationLevel {
		t.Errorf("escalation level changed on a repeat scan: %d then %d",
			first.EscalationLevel, second.EscalationLevel)
	}
	if first.Priority != second.Priority {
		t.Errorf("priority drifted on a repeat scan: %q then %q", first.Priority, second.Priority)
	}
	if stats := h.server.sla.Stats(); stats.Escalations != 1 {
		t.Errorf("escalations = %d, want exactly 1 for a single breach", stats.Escalations)
	}
}

// TestPolicyWithoutEscalationNeverEscalates covers the low-priority policy,
// which sets escalation_minutes to 0 to opt out of escalation entirely.
func TestPolicyWithoutEscalationNeverEscalates(t *testing.T) {
	h := newHarness(t)
	_, _, _, user, _ := h.seed()

	ticket := h.createTicketAs(user, "Request a second chair", "hardware", models.PriorityLow)
	makeOverdue(t, h, ticket.ID, 30*24*time.Hour)

	h.runEngine()
	waitForBreach(t, h)
	waitForScan(t, h, 3)

	after, err := h.store.Tickets.ByID(context.Background(), ticket.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !after.ResolutionBreached {
		t.Fatal("the low policy should still breach on resolution")
	}
	if after.EscalationLevel != 0 {
		t.Errorf("escalation level = %d, want 0: the policy opts out of escalation",
			after.EscalationLevel)
	}
}

func TestAnalyticsEndpointsExposeMTTRAndWorkload(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()
	techUser, _ := h.store.Users.ByUsername(context.Background(), tech)
	techToken := h.tokenFor(tech, models.RoleTechnician)

	// Two tickets resolved at different ages so MTTR has a real mean.
	for i, title := range []string{"Fixed: printer jam", "Fixed: login loop"} {
		tk := h.createTicketAs(user, title, "hardware", models.PriorityMedium)
		h.assignTo(tk.ID, techUser.ID)
		if res := h.do(http.MethodPatch, "/api/v1/tickets/"+itoa(tk.ID), techToken,
			map[string]any{"status": "resolved"}); res.Status != http.StatusOK {
			t.Fatalf("resolve %d: %d %s", i, res.Status, res.Raw)
		}
	}
	open := h.createTicketAs(user, "Still open", "network", models.PriorityLow)

	mttr := h.do(http.MethodGet, "/api/v1/analytics/mttr", h.tokenFor("admin", models.RoleAdmin), nil)
	if mttr.Status != http.StatusOK {
		t.Fatalf("mttr = %d; body = %s", mttr.Status, mttr.Raw)
	}
	result, ok := mttr.Body["mttr"].(map[string]any)
	if !ok {
		t.Fatalf("expected an mttr object, got %s", mttr.Raw)
	}
	if n, _ := result["resolved_count"].(float64); int(n) != 2 {
		t.Errorf("resolved_count = %v, want 2", result["resolved_count"])
	}
	if avg, _ := result["avg_resolution_hours"].(float64); avg < 0 {
		t.Errorf("avg_resolution_hours = %v, want a non-negative duration", result["avg_resolution_hours"])
	}

	workload := h.do(http.MethodGet, "/api/v1/analytics/workload", h.tokenFor("admin", models.RoleAdmin), nil)
	if workload.Status != http.StatusOK {
		t.Fatalf("workload = %d; body = %s", workload.Status, workload.Raw)
	}
	techs, ok := workload.Body["technicians"].([]any)
	if !ok || len(techs) == 0 {
		t.Fatalf("expected a technician list, got %s", workload.Raw)
	}
	found := false
	for _, entry := range techs {
		m := entry.(map[string]any)
		if m["user_id"].(float64) == float64(techUser.ID) {
			found = true
			// Resolved work still counts against the technician until the
			// requester confirms and it closes, matching the open-ticket
			// filters elsewhere in the API.
			if active, _ := m["active_tickets"].(float64); int(active) != 2 {
				t.Errorf("technician active_tickets = %v, want 2 (resolved is not closed)",
					m["active_tickets"])
			}
			if resolved, _ := m["resolved_30d"].(float64); int(resolved) != 2 {
				t.Errorf("technician resolved_30d = %v, want 2", m["resolved_30d"])
			}
		}
	}
	if !found {
		t.Error("the technician is missing from the workload report")
	}

	// The open ticket must show up as load somewhere in the report.
	if total, _ := workload.Body["active_total"].(float64); int(total) < 1 {
		t.Errorf("active_total = %v, want at least the open ticket", workload.Body["active_total"])
	}

	dashboard := h.do(http.MethodGet, "/api/v1/analytics/dashboard", h.tokenFor("admin", models.RoleAdmin), nil)
	if dashboard.Status != http.StatusOK {
		t.Fatalf("dashboard = %d; body = %s", dashboard.Status, dashboard.Raw)
	}
	for _, key := range []string{"stats", "mttr", "throughput", "workload", "by_category", "by_priority"} {
		if _, present := dashboard.Body[key]; !present {
			t.Errorf("dashboard response is missing %q", key)
		}
	}

	_ = open
}

func TestDashboardReportsSaneValuesOnAnEmptySystem(t *testing.T) {
	h := newHarness(t)
	h.seed()

	res := h.do(http.MethodGet, "/api/v1/analytics/dashboard", h.tokenFor("admin", models.RoleAdmin), nil)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", res.Status, res.Raw)
	}
	stats, ok := res.Body["stats"].(map[string]any)
	if !ok {
		t.Fatalf("expected stats, got %s", res.Raw)
	}
	// An empty system must render zeros and nulls, never a 500 from a NULL sum.
	if v, _ := stats["total_tickets"].(float64); v != 0 {
		t.Errorf("total_tickets = %v, want 0", v)
	}
	for _, key := range []string{
		"avg_overdue_hours", "avg_first_response_hours", "sla_compliance_pct",
	} {
		if v, present := stats[key]; present && v != nil {
			if f, ok := v.(float64); ok && f < 0 {
				t.Errorf("%s = %v, want null rather than a negative average", key, v)
			}
		}
	}
}

func TestHealthEndpointIsPublic(t *testing.T) {
	h := newHarness(t)
	res := h.do(http.MethodGet, "/health", "", nil)
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", res.Status, res.Raw)
	}
	if res.str("status") != "ok" {
		t.Errorf("status = %q, want ok", res.str("status"))
	}
	if res.str("db") != "connected" {
		t.Errorf("db = %q, want connected", res.str("db"))
	}
	// Internal details must not leak configuration.
	if _, leaked := res.Body["jwt_secret"]; leaked {
		t.Error("health response leaked the JWT secret")
	}
}

func TestNotificationLifecycle(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()

	ticket := h.createTicketAs(user, "Please reset my directory password", "access", models.PriorityMedium)
	techToken := h.tokenFor(tech, models.RoleTechnician)

	// The creation notification lands in the requester's own inbox.
	notifs := h.do(http.MethodGet, "/api/v1/notifications", h.tokenFor(user, models.RoleUser), nil)
	if notifs.Status != http.StatusOK {
		t.Fatalf("notifications = %d; body = %s", notifs.Status, notifs.Raw)
	}

	unread := h.do(http.MethodGet, "/api/v1/notifications/unread-count", h.tokenFor(user, models.RoleUser), nil)
	n := int(unread.num("unread"))
	if n == 0 {
		t.Fatal("expected at least one unread notification")
	}

	items := notifs.items()
	first, ok := items[0]["id"].(float64)
	if !ok {
		t.Fatalf("notification has no id: %s", notifs.Raw)
	}

	marked := h.do(http.MethodPost, "/api/v1/notifications/"+itoa(int64(first))+"/read",
		h.tokenFor(user, models.RoleUser), nil)
	if marked.Status != http.StatusOK {
		t.Fatalf("mark read = %d; body = %s", marked.Status, marked.Raw)
	}

	after := h.do(http.MethodGet, "/api/v1/notifications/unread-count", h.tokenFor(user, models.RoleUser), nil)
	if got := int(after.num("unread")); got != n-1 {
		t.Errorf("unread = %d, want %d after marking one read", got, n-1)
	}

	// One user must not be able to mark another user's notification read.
	foreign := h.do(http.MethodPost, "/api/v1/notifications/"+itoa(int64(first))+"/read", techToken, nil)
	if foreign.Status != http.StatusNotFound && foreign.Status != http.StatusForbidden {
		t.Errorf("marking another user's notification = %d, want 404 or 403; body = %s",
			foreign.Status, foreign.Raw)
	}

	all := h.do(http.MethodPost, "/api/v1/notifications/read-all", h.tokenFor(user, models.RoleUser), nil)
	if all.Status != http.StatusOK {
		t.Fatalf("mark all read = %d; body = %s", all.Status, all.Raw)
	}
	final := h.do(http.MethodGet, "/api/v1/notifications/unread-count", h.tokenFor(user, models.RoleUser), nil)
	if got := int(final.num("unread")); got != 0 {
		t.Errorf("unread = %d, want 0 after mark-all-read", got)
	}

	_ = ticket
}

func TestAuditTrailRecordsMutations(t *testing.T) {
	h := newHarness(t)
	admin := h.createUser("admin", models.RoleAdmin)
	adminToken := h.tokenFor(admin.Username, models.RoleAdmin)

	// Creating a user must be auditable.
	created := h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{
		"username": "auditee", "email": "auditee@test.local", "password": "Passw0rd123",
	})
	if created.Status != http.StatusCreated {
		t.Fatalf("create user = %d; body = %s", created.Status, created.Raw)
	}

	trail := h.do(http.MethodGet, "/api/v1/audit", adminToken, nil)
	if trail.Status != http.StatusOK {
		t.Fatalf("audit = %d; body = %s", trail.Status, trail.Raw)
	}
	found := false
	for _, entry := range trail.items() {
		if entry["action"] == "user.create" {
			found = true
		}
	}
	if !found {
		t.Errorf("no user.create entry in the audit trail: %s", trail.Raw)
	}
}

func TestSLAUpdateRejectsAnImpossiblePolicy(t *testing.T) {
	h := newHarness(t)
	admin := h.createUser("admin", models.RoleAdmin)
	token := h.tokenFor(admin.Username, models.RoleAdmin)

	// The high policy is 60m/480m. Setting a response target longer than the
	// resolution target would create a policy that can never be met.
	bad := h.do(http.MethodPatch, "/api/v1/sla-policies/3", token, map[string]any{
		"response_minutes": 600, "resolution_minutes": 60,
	})
	if bad.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", bad.Status, bad.Raw)
	}

	// The policy must be unchanged.
	policies := h.do(http.MethodGet, "/api/v1/sla-policies", token, nil)
	for _, p := range policies.items() {
		if p["priority"] == "high" {
			if resp, _ := p["response_minutes"].(float64); int(resp) != 60 {
				t.Errorf("response_minutes = %v, want the original 60", p["response_minutes"])
			}
		}
	}
}

func TestServiceValidateCreateRules(t *testing.T) {
	// The validation rules are unit-tested directly so the error paths are
	// pinned down without going through HTTP.
	long := make([]byte, 301)
	for i := range long {
		long[i] = 'x'
	}

	cases := []struct {
		name string
		req  service.CreateRequest
		ok   bool
	}{
		{"valid", service.CreateRequest{Title: "Broken printer", Description: "It jams.", Category: "hardware"}, true},
		{"empty title", service.CreateRequest{Title: "  ", Description: "x", Category: "hardware"}, false},
		{"title too long", service.CreateRequest{Title: string(long), Description: "x", Category: "hardware"}, false},
		{"unknown category", service.CreateRequest{Title: "t", Description: "x", Category: "teapot"}, false},
		{"empty description", service.CreateRequest{Title: "t", Description: "", Category: "hardware"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := service.ValidateCreate(&tc.req)
			if tc.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

func TestStatusLabelsAndTransitionsAreComplete(t *testing.T) {
	// Every status must have a label and a transition list, or the UI renders
	// blanks and offers no actions.
	for _, s := range models.AllStatuses {
		if s.Label() == "" || s.Label() == string(s) {
			t.Errorf("status %q has no distinct label", s)
		}
		if len(s.NextStatuses()) == 0 {
			t.Errorf("status %q has no outgoing transitions", s)
		}
		// A status must never be allowed to transition to itself, which would
		// make the audit trail record no-ops.
		for _, next := range s.NextStatuses() {
			if next == s {
				t.Errorf("status %q lists itself as a transition target", s)
			}
		}
	}
	for _, p := range models.AllPriorities {
		if p.Label() == "" {
			t.Errorf("priority %q has no label", p)
		}
	}
	// Ranks must be unique so the sort order is a total order.
	seen := map[int]models.Priority{}
	for _, p := range models.AllPriorities {
		if other, dup := seen[p.Rank()]; dup {
			t.Errorf("priorities %q and %q share rank %d", p, other, p.Rank())
		}
		seen[p.Rank()] = p
	}
}
