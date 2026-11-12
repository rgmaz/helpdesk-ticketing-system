package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/ai"
	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// triageRequest is the wire format for POST /ai/triage.
type triageRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Priority    string `json:"priority"`
}

// handleTriage classifies text the user is composing and suggests duplicates.
//
// This runs before a ticket exists, so it is rate-limited and cached, and it
// degrades to an empty result rather than failing the form when the provider is
// slow or unavailable.
func (s *Server) handleTriage(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var body triageRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	body.Title = strings.TrimSpace(body.Title)
	body.Description = strings.TrimSpace(body.Description)
	if len(body.Title) > 200 {
		httpx.Error(w, http.StatusBadRequest, "title must be at most 200 characters", "invalid_input")
		return
	}
	if len(body.Description) > 10000 {
		httpx.Error(w, http.StatusBadRequest, "description must be at most 10000 characters", "invalid_input")
		return
	}

	category := strings.ToLower(strings.TrimSpace(body.Category))
	if category != "" && !validCategory(category) {
		httpx.Error(w, http.StatusBadRequest,
			"category must be one of "+strings.Join(service.Categories, ", "), "invalid_input")
		return
	}

	priority := models.PriorityMedium
	if raw := strings.TrimSpace(body.Priority); raw != "" {
		p, err := models.ParsePriority(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_priority")
			return
		}
		priority = p
	}

	result, err := s.ai.Preflight(r.Context(), actor, ai.TriageInput{
		Title: body.Title, Description: body.Description,
		Category: category, Priority: priority,
	})
	if err != nil {
		switch {
		case errors.Is(err, ai.ErrDisabled):
			httpx.Error(w, http.StatusServiceUnavailable, "the AI assistant is disabled", "ai_disabled")
		case errors.Is(err, ai.ErrRateLimited):
			httpx.Error(w, http.StatusTooManyRequests, "slow down: too many triage requests", "ai_rate_limited")
		default:
			httpx.Fail(w, err)
		}
		return
	}
	httpx.OK(w, result)
}

// handleListAISuggestions returns the newest suggestion of each kind for a
// ticket. Staff only: these drive triage actions the requester cannot take.
func (s *Server) handleListAISuggestions(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticketID, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticket, err := s.store.Tickets.ByID(r.Context(), ticketID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !s.tickets.CanView(actor, ticket) {
		httpx.Fail(w, fmt.Errorf("%w: this ticket belongs to another requester", models.ErrForbidden))
		return
	}

	list, err := s.ai.ListSuggestions(r.Context(), ticketID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{
		Items: list, Total: len(list), Page: 1, Limit: len(list), Pages: 1,
		Extra: map[string]any{
			"enabled":        s.ai.Enabled(),
			"prompt_version": s.ai.PromptVersion(),
			"model":          s.ai.Model(),
		},
	})
}

// handleAcceptAISuggestion applies a suggestion through the normal service
// layer. The AI package never writes to a ticket itself, so transitions, SLA
// clocks, events and audit rows all follow the existing rules.
func (s *Server) handleAcceptAISuggestion(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticketID, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	suggestionID, err := httpx.PathInt64(r, "suggestion_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	suggestion, err := s.ai.Suggestion(r.Context(), ticketID, suggestionID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if suggestion.AcceptedAt != nil || suggestion.RejectedAt != nil {
		httpx.Error(w, http.StatusConflict, "this suggestion has already been decided", "conflict")
		return
	}

	switch suggestion.Kind {
	case "category":
		category := strings.ToLower(strings.TrimSpace(suggestion.Value))
		if !validCategory(category) {
			httpx.Error(w, http.StatusUnprocessableEntity, "suggestion contains an unknown category", "invalid_input")
			return
		}
		if _, err := s.tickets.Update(r.Context(), actor, ticketID, service.UpdateRequest{Category: &category}); err != nil {
			httpx.Fail(w, err)
			return
		}

	case "priority":
		priority, err := models.ParsePriority(suggestion.Value)
		if err != nil {
			httpx.Error(w, http.StatusUnprocessableEntity, "suggestion contains an unknown priority", "invalid_input")
			return
		}
		if _, err := s.tickets.Update(r.Context(), actor, ticketID, service.UpdateRequest{Priority: &priority}); err != nil {
			httpx.Fail(w, err)
			return
		}

	case "duplicate":
		// Link-only: record an internal note pointing at the canonical ticket.
		// Closing the duplicate automatically is a policy decision that needs
		// acceptance data behind it, so it is deliberately not done here.
		number := duplicateNumber(suggestion)
		body := fmt.Sprintf("AI flagged this as a possible duplicate of %s. Review the linked ticket before closing this one.", number)
		if _, err := s.tickets.AddComment(r.Context(), actor, ticketID, body, true); err != nil {
			httpx.Fail(w, err)
			return
		}

	default:
		httpx.Error(w, http.StatusUnprocessableEntity, "unknown suggestion kind", "invalid_input")
		return
	}

	if err := s.ai.MarkSuggestion(r.Context(), suggestionID, actor.ID, true); err != nil {
		httpx.Fail(w, err)
		return
	}
	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID:  &actor.ID,
		TicketID: &ticketID,
		Action:   "ai.suggestion.accepted",
		Details:  strPtrf("accepted %s suggestion %q", suggestion.Kind, suggestion.Value),
	})

	ticket, err := s.store.Tickets.ByID(r.Context(), ticketID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"ticket": ticket, "suggestion": suggestion})
}

// handleRejectAISuggestion records feedback without changing the ticket. It is
// the signal the acceptance-rate metric is built from.
func (s *Server) handleRejectAISuggestion(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticketID, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	suggestionID, err := httpx.PathInt64(r, "suggestion_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	suggestion, err := s.ai.Suggestion(r.Context(), ticketID, suggestionID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := s.ai.MarkSuggestion(r.Context(), suggestionID, actor.ID, false); err != nil {
		httpx.Fail(w, err)
		return
	}
	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID:  &actor.ID,
		TicketID: &ticketID,
		Action:   "ai.suggestion.rejected",
		Details:  strPtrf("rejected %s suggestion %q", suggestion.Kind, suggestion.Value),
	})
	httpx.OK(w, map[string]any{"id": suggestionID, "rejected": true})
}

// handleAIHealth is the administrative view: whether the feature is on, the
// active model and prompt version, and queue/acceptance counters.
func (s *Server) handleAIHealth(w http.ResponseWriter, r *http.Request) {
	stats, err := s.ai.Stats(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"enabled":        s.ai.Enabled(),
		"model":          s.ai.Model(),
		"prompt_version": s.ai.PromptVersion(),
		"spent_usd":      s.ai.SpentUSD(),
		"stats":          stats,
	})
}

func duplicateNumber(s store.AISuggestion) string {
	if len(s.Citations) > 0 && s.Citations[0].TicketNumber != "" {
		return s.Citations[0].TicketNumber
	}
	if id, err := strconv.ParseInt(s.Value, 10, 64); err == nil {
		return fmt.Sprintf("ticket #%d", id)
	}
	return "the linked ticket"
}
