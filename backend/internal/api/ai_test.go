package api

import (
	"context"
	"testing"

	"github.com/ryan/ticketing-system/backend/internal/ai"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// fakeProvider returns a fixed completion so the API tests do not need a network
// or an API key.
type fakeProvider struct {
	content string
	err     error
	calls   int
}

func (f *fakeProvider) Chat(context.Context, ai.ChatRequest) (ai.ChatResponse, error) {
	f.calls++
	if f.err != nil {
		return ai.ChatResponse{}, f.err
	}
	return ai.ChatResponse{
		Content: f.content, Model: "test-model",
		PromptTokens: 12, CompletionTokens: 6,
	}, nil
}

func (f *fakeProvider) Name() string { return "test-model" }

func TestTriageDisabledReturns503(t *testing.T) {
	h := newHarness(t)
	_, _, _, userA, _ := h.seed()
	token := h.tokenFor(userA, models.RoleUser)

	res := h.do("POST", "/api/v1/ai/triage", token, map[string]any{
		"title": "VPN keeps dropping", "description": "It disconnects every few minutes",
	})
	if res.Status != 503 {
		t.Fatalf("expected 503 when AI is disabled, got %d: %s", res.Status, res.Raw)
	}
	if res.str("code") != "ai_disabled" {
		t.Fatalf("expected ai_disabled code, got %q", res.str("code"))
	}
}

func TestTriagePreflightReturnsSuggestions(t *testing.T) {
	provider := &fakeProvider{content: `{
		"category":{"value":"network","confidence":0.91,"rationale":"connectivity"},
		"priority":{"value":"high","confidence":0.77,"rationale":"service outage"},
		"duplicates":[]
	}`}
	h := newHarnessWithProvider(t, provider)
	_, _, _, userA, _ := h.seed()
	token := h.tokenFor(userA, models.RoleUser)

	res := h.do("POST", "/api/v1/ai/triage", token, map[string]any{
		"title": "VPN keeps dropping", "description": "It disconnects every few minutes",
		"category": "network", "priority": "medium",
	})
	if res.Status != 200 {
		t.Fatalf("expected 200, got %d: %s", res.Status, res.Raw)
	}
	suggestions, ok := res.Body["suggestions"].(map[string]any)
	if !ok {
		t.Fatalf("missing suggestions object: %s", res.Raw)
	}
	category, ok := suggestions["category"].(map[string]any)
	if !ok || category["value"] != "network" {
		t.Fatalf("expected a network category suggestion: %s", res.Raw)
	}
	if provider.calls != 1 {
		t.Fatalf("expected one provider call, got %d", provider.calls)
	}
}

func TestTriagePreflightDegradesWhenProviderFails(t *testing.T) {
	h := newHarnessWithProvider(t, &fakeProvider{err: ai.ErrUnavailable})
	_, _, _, userA, _ := h.seed()
	token := h.tokenFor(userA, models.RoleUser)

	res := h.do("POST", "/api/v1/ai/triage", token, map[string]any{
		"title": "Printer is offline", "description": "The office printer reports offline",
	})
	if res.Status != 200 {
		t.Fatalf("provider failure must degrade, got %d: %s", res.Status, res.Raw)
	}
	if degraded, _ := res.Body["degraded"].(bool); !degraded {
		t.Fatalf("expected degraded=true, got %s", res.Raw)
	}
}

func TestSuggestionEndpointsAreStaffOnly(t *testing.T) {
	h := newHarnessWithProvider(t, &fakeProvider{content: `{"category":{"value":"network","confidence":0.5}}`})
	_, _, _, userA, _ := h.seed()
	ticket := h.createTicketAs(userA, "VPN issue for access", "network", models.PriorityMedium)
	token := h.tokenFor(userA, models.RoleUser)

	res := h.do("GET", "/api/v1/tickets/"+itoa(ticket.ID)+"/ai/suggestions", token, nil)
	if res.Status != 403 {
		t.Fatalf("requesters must not read triage suggestions, got %d: %s", res.Status, res.Raw)
	}
}

func TestAcceptCategorySuggestionUsesServiceLayer(t *testing.T) {
	h := newHarness(t)
	_, techA, _, userA, _ := h.seed()
	ticket := h.createTicketAs(userA, "Cannot reach shared drive", "general", models.PriorityMedium)

	ctx := context.Background()
	jobID, err := h.store.AI.EnqueueJob(ctx, store.NewAIJob{
		TicketID: ticket.ID, Kind: "triage", InputHash: "test-hash", PromptVersion: ai.PromptVersion,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if err := h.store.AI.AddSuggestions(ctx, jobID, []store.NewAISuggestion{{
		TicketID: ticket.ID, Kind: "category", Value: "network", Confidence: 0.9,
		Rationale: "shared drive is a network resource", Model: "test-model", PromptVersion: ai.PromptVersion,
	}}); err != nil {
		t.Fatalf("add suggestion: %v", err)
	}
	list, err := h.store.AI.ListSuggestions(ctx, ticket.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list suggestions: %v (%d rows)", err, len(list))
	}

	token := h.tokenFor(techA, models.RoleTechnician)
	res := h.do("POST", "/api/v1/tickets/"+itoa(ticket.ID)+"/ai/suggestions/"+itoa(list[0].ID)+"/accept", token, nil)
	if res.Status != 200 {
		t.Fatalf("accept failed: %d %s", res.Status, res.Raw)
	}
	updated, err := h.store.Tickets.ByID(ctx, ticket.ID)
	if err != nil {
		t.Fatalf("reload ticket: %v", err)
	}
	if updated.Category != "network" {
		t.Fatalf("expected category network, got %q", updated.Category)
	}

	// The decision is audited through the normal trail.
	entries, total, err := h.store.Audit.List(ctx, &ticket.ID, 50, 0)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "ai.suggestion.accepted" {
			found = true
		}
	}
	if !found || total == 0 {
		t.Fatalf("expected an ai.suggestion.accepted audit row, got %+v", entries)
	}

	// A second decision is a conflict, not a silent overwrite.
	again := h.do("POST", "/api/v1/tickets/"+itoa(ticket.ID)+"/ai/suggestions/"+itoa(list[0].ID)+"/accept", token, nil)
	if again.Status != 409 {
		t.Fatalf("expected 409 on a decided suggestion, got %d: %s", again.Status, again.Raw)
	}
}

func TestAcceptDuplicateSuggestionAddsInternalNoteOnly(t *testing.T) {
	h := newHarness(t)
	_, techA, _, userA, _ := h.seed()
	ctx := context.Background()

	canonical := h.createTicketAs(userA, "Outlook will not open mailboxes", "email", models.PriorityMedium)
	duplicate := h.createTicketAs(userA, "Cannot open Outlook mailbox", "email", models.PriorityMedium)

	jobID, err := h.store.AI.EnqueueJob(ctx, store.NewAIJob{
		TicketID: duplicate.ID, Kind: "triage", InputHash: "dup-hash", PromptVersion: ai.PromptVersion,
	})
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if err := h.store.AI.AddSuggestions(ctx, jobID, []store.NewAISuggestion{{
		TicketID: duplicate.ID, Kind: "duplicate", Value: itoa(canonical.ID), Confidence: 0.88,
		Rationale: "same mailbox symptom", Model: "test-model", PromptVersion: ai.PromptVersion,
		Citations: []store.AICitation{{TicketID: canonical.ID, TicketNumber: canonical.TicketNumber}},
	}}); err != nil {
		t.Fatalf("add suggestion: %v", err)
	}
	list, _ := h.store.AI.ListSuggestions(ctx, duplicate.ID)
	if len(list) != 1 {
		t.Fatalf("expected one suggestion, got %d", len(list))
	}

	token := h.tokenFor(techA, models.RoleTechnician)
	res := h.do("POST", "/api/v1/tickets/"+itoa(duplicate.ID)+"/ai/suggestions/"+itoa(list[0].ID)+"/accept", token, nil)
	if res.Status != 200 {
		t.Fatalf("accept failed: %d %s", res.Status, res.Raw)
	}

	public, err := h.store.Comments.List(ctx, duplicate.ID, false)
	if err != nil {
		t.Fatalf("list public comments: %v", err)
	}
	if len(public) != 0 {
		t.Fatalf("duplicate link must be internal, got %d public comments", len(public))
	}
	all, err := h.store.Comments.List(ctx, duplicate.ID, true)
	if err != nil {
		t.Fatalf("list internal comments: %v", err)
	}
	if len(all) != 1 || !all[0].Internal {
		t.Fatalf("expected one internal note, got %+v", all)
	}
}

func TestDuplicateCandidatesAreScopedToRequester(t *testing.T) {
	h := newHarness(t)
	_, _, _, userA, userB := h.seed()
	ctx := context.Background()

	owned := h.createTicketAs(userA, "VPN drops after laptop wakes", "network", models.PriorityMedium)

	a, err := h.store.Users.ByUsername(ctx, userA)
	if err != nil {
		t.Fatalf("load userA: %v", err)
	}
	b, err := h.store.Users.ByUsername(ctx, userB)
	if err != nil {
		t.Fatalf("load userB: %v", err)
	}

	own, err := h.store.AI.Candidates(ctx, `"vpn"`, &a.ID, 10)
	if err != nil {
		t.Fatalf("candidates for owner: %v", err)
	}
	if len(own) == 0 || own[0].TicketID != owned.ID {
		t.Fatalf("owner should see their ticket, got %+v", own)
	}

	other, err := h.store.AI.Candidates(ctx, `"vpn"`, &b.ID, 10)
	if err != nil {
		t.Fatalf("candidates for other requester: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("another requester must not see the ticket, got %+v", other)
	}

	staff, err := h.store.AI.Candidates(ctx, `"vpn"`, nil, 10)
	if err != nil {
		t.Fatalf("staff candidates: %v", err)
	}
	if len(staff) == 0 {
		t.Fatal("staff should see the whole corpus")
	}
}
