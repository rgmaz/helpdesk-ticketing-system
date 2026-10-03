package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// createTicketRequest is the wire format for POST /tickets.
type createTicketRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Priority    string `json:"priority"`
	AssigneeID  *int64 `json:"assignee_id"`
}

// handleCreateTicket files a new ticket, applying the SLA policy and routing.
func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var body createTicketRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	priority, err := models.ParsePriority(body.Priority)
	if err != nil {
		if body.Priority == "" {
			priority = models.PriorityMedium
		} else {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_priority")
			return
		}
	}

	req := service.CreateRequest{
		Title:       body.Title,
		Description: body.Description,
		Category:    strings.ToLower(strings.TrimSpace(body.Category)),
		Priority:    priority,
	}
	// Only staff may assign at creation time; end users cannot target a
	// technician, and the router picks automatically instead.
	if body.AssigneeID != nil && actor.Role.IsStaff() {
		req.AssigneeID = body.AssigneeID
	}

	if err := service.ValidateCreate(&req); err != nil {
		httpx.Fail(w, err)
		return
	}

	ticket, err := s.tickets.Create(r.Context(), actor, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, map[string]any{"ticket": ticket})
}

// ticketFilterResponse wraps the list with derived SLA state per row.
type ticketListResponse struct {
	httpx.ListBody
	Counts map[string]int `json:"counts"`
}

// handleListTickets returns a filtered, paginated ticket list.
//
// End users are transparently scoped to their own tickets; staff see
// everything. An admin may pass ?scope=mine to narrow to their own.
func (s *Server) handleListTickets(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	f, err := ticketFilterFromRequest(r, actor)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	page, err := httpx.QueryIntStrict(r, "page", 0, 0, 100000)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	limit, err := httpx.QueryIntStrict(r, "limit", 25, 1, 100)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	f.Page, f.Limit = page, limit

	tickets, total, err := s.store.Tickets.List(r.Context(), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	counts, err := s.store.Tickets.CountsByStatus(r.Context(), scopedRequester(f, actor))
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	httpx.OK(w, ticketListResponse{
		ListBody: httpx.ListBody{
			Items: tickets,
			Total: total,
			Page:  page + 1,
			Limit: limit,
			Pages: httpx.Pages(total, limit),
		},
		Counts: counts,
	})
}

// scopedRequester returns the requester filter to use for the counts summary.
func scopedRequester(f store.TicketFilter, actor service.Actor) *int64 {
	if f.RequesterID != nil {
		return f.RequesterID
	}
	if !actor.Role.IsStaff() {
		return &actor.ID
	}
	return nil
}

// ticketFilterFromRequest parses and validates the query-string filters.
func ticketFilterFromRequest(r *http.Request, actor service.Actor) (store.TicketFilter, error) {
	var f store.TicketFilter
	q := r.URL.Query()

	// Row-level scoping. Non-staff are always pinned to their own tickets,
	// ignoring any requester filter they supply.
	if actor.Role.IsStaff() {
		if raw := strings.TrimSpace(q.Get("requester_id")); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return f, fmt.Errorf("%w: requester_id must be a positive integer", models.ErrInvalidInput)
			}
			f.RequesterID = &id
		} else if raw := strings.TrimSpace(q.Get("requester")); raw != "" {
			// Convenience filter: ?requester=me
			if raw == "me" {
				f.RequesterID = &actor.ID
			}
		}
	} else {
		f.RequesterID = &actor.ID
	}

	if actor.Role.IsStaff() {
		if raw := strings.TrimSpace(q.Get("assignee_id")); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return f, fmt.Errorf("%w: assignee_id must be a positive integer", models.ErrInvalidInput)
			}
			f.AssigneeID = &id
		} else if raw := strings.TrimSpace(q.Get("assignee")); raw != "" {
			switch raw {
			case "me":
				f.AssigneeID = &actor.ID
			case "none", "unassigned":
				f.Unassigned = true
			}
		}
	}

	if v, present := httpx.QueryBool(r, "unassigned"); present && v {
		f.Unassigned = true
	}

	// Comma-separated status/priority/category lists.
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			st, err := models.ParseStatus(part)
			if err != nil {
				return f, fmt.Errorf("%w: %s", models.ErrInvalidInput, err)
			}
			f.Statuses = append(f.Statuses, st)
		}
	}
	if raw := strings.TrimSpace(q.Get("priority")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			p, err := models.ParsePriority(part)
			if err != nil {
				return f, fmt.Errorf("%w: %s", models.ErrInvalidInput, err)
			}
			f.Priorities = append(f.Priorities, p)
		}
	}
	if raw := strings.TrimSpace(q.Get("category")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			f.Categories = append(f.Categories, strings.ToLower(strings.TrimSpace(part)))
		}
	}

	f.Search = strings.TrimSpace(q.Get("q"))
	if f.Search != "" {
		if err := validateSearchQuery(f.Search); err != nil {
			return f, err
		}
	}
	if v, present := httpx.QueryBool(r, "breached"); present {
		f.Breached = &v
	}
	if v, present := httpx.QueryBool(r, "escalated"); present {
		f.Escalated = &v
	}
	if v, present := httpx.QueryBool(r, "open"); present && v {
		f.OpenOnly = true
	}

	var err error
	if f.CreatedFrom, err = store.ParseTimeFilter(q.Get("created_from")); err != nil {
		return f, err
	}
	if f.CreatedTo, err = store.ParseTimeFilter(q.Get("created_to")); err != nil {
		return f, err
	}
	if f.ResolvedFrom, err = store.ParseTimeFilter(q.Get("resolved_from")); err != nil {
		return f, err
	}
	if f.ResolvedTo, err = store.ParseTimeFilter(q.Get("resolved_to")); err != nil {
		return f, err
	}

	f.SortBy = strings.TrimSpace(q.Get("sort_by"))
	f.SortDir = strings.TrimSpace(q.Get("sort_dir"))
	// An unknown sort key is rejected rather than defaulted: silently returning
	// created_at order would leave the user thinking the table was sorted by the
	// column they clicked.
	if err := f.ValidateSort(); err != nil {
		return f, err
	}
	return f, nil
}

// validateSearchQuery rejects input that FTS5 would treat as syntax rather than
// as search terms.
//
// MATCH expressions have their own grammar: a bare operator, an unbalanced quote
// or parenthesis is a parse error, and "a OR" matches nothing useful. Rather
// than letting those reach the engine and surface as a 500, the endpoint accepts
// plain words only and says so. Prefix search is deliberately not offered: the
// porter tokenizer stems inflections, so "print" already finds "printing", and
// "print*" would cost an index scan per keystroke.
func validateSearchQuery(q string) error {
	const maxQueryLen = 200
	if len(q) > maxQueryLen {
		return fmt.Errorf("%w: search query must be at most %d characters", models.ErrInvalidInput, maxQueryLen)
	}

	// Characters FTS5 assigns meaning: phrase/column filters, the prefix and
	// NEAR operators, grouping, exclusion and quoting.
	if strings.ContainsAny(q, `:"^()*-{}[],`) {
		return fmt.Errorf("%w: search supports plain words only; remove quotes, "+
			"punctuation and operators such as * or -", models.ErrInvalidInput)
	}

	// Bareword boolean operators are keywords to FTS5, so "NOT print" would be a
	// negation rather than a search for the word "not".
	for _, word := range strings.Fields(q) {
		switch strings.ToUpper(word) {
		case "AND", "OR", "NOT", "NEAR":
			return fmt.Errorf("%w: AND, OR, NOT and NEAR are reserved; "+
				"search supports plain words only", models.ErrInvalidInput)
		}
	}
	return nil
}

// handleSearchTickets is a dedicated full-text search endpoint backed by FTS5.
//
// It returns the same shape as the list endpoint so the UI can swap between
// filtered browsing and free-text search without changing renderers.
func (s *Server) handleSearchTickets(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	if raw == "" {
		httpx.Error(w, http.StatusBadRequest, "q is required", "invalid_input")
		return
	}
	if err := validateSearchQuery(raw); err != nil {
		httpx.Fail(w, err)
		return
	}

	f, err := ticketFilterFromRequest(r, actor)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	// The dedicated endpoint's query wins over any incidental ?q.
	f.Search = raw
	limit, err := httpx.QueryIntStrict(r, "limit", 25, 1, 100)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	page, err := httpx.QueryIntStrict(r, "page", 0, 0, 10000)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	f.Limit, f.Page = limit, page

	tickets, total, err := s.store.Tickets.List(r.Context(), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	// Attach an FTS5-highlighted context for each hit so the UI can show why a
	// result matched without a second round-trip. One extra query for the page,
	// not one per ticket.
	ids := make([]int64, len(tickets))
	for i := range tickets {
		ids[i] = tickets[i].ID
	}
	snippets, err := s.store.Tickets.SnippetsFor(r.Context(), raw, ids)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	for i := range tickets {
		tickets[i].Snippet = snippets[tickets[i].ID]
	}

	httpx.OK(w, httpx.ListBody{
		Items: tickets, Total: total, Page: page + 1, Limit: limit,
		Pages: httpx.Pages(total, limit),
		Extra: map[string]any{"query": raw, "engine": "fts5"},
	})
}

// handleGetTicket returns one ticket with its thread, timeline, and SLA state.
func (s *Server) handleGetTicket(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticket, err := s.store.Tickets.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !s.tickets.CanView(actor, ticket) {
		httpx.Fail(w, fmt.Errorf("%w: this ticket belongs to another requester", models.ErrForbidden))
		return
	}

	comments, err := s.store.Comments.List(r.Context(), id, actor.Role.IsStaff())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	events, err := s.store.Events.List(r.Context(), id, 100)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	// Staff also see the private SLA state and the raw policy so the UI can
	// render countdown timers; end users only see their ticket's own progress.
	response := map[string]any{
		"ticket":   ticket,
		"comments": comments,
		"events":   events,
		"sla": map[string]any{
			"state":               ticket.SLAStateOf(time.Now(), 30),
			"response_breached":   ticket.ResponseBreached,
			"resolution_breached": ticket.ResolutionBreached,
			"escalation_level":    ticket.EscalationLevel,
		},
		"available_transitions": ticket.Status.NextStatuses(),
		"can_delete":            actor.Role.IsAdmin(),
	}
	if actor.Role.IsStaff() {
		internalCount, err := s.store.Comments.CountInternal(r.Context(), id)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		response["internal_note_count"] = internalCount
	}
	httpx.OK(w, response)
}

// updateTicketRequest is the wire format for PATCH /tickets/{id}.
// Pointer fields distinguish "absent" from "set to zero/empty".
type updateTicketRequest struct {
	Status          *string `json:"status"`
	Priority        *string `json:"priority"`
	Category        *string `json:"category"`
	AssigneeID      *int64  `json:"assignee_id"`
	Unassign        bool    `json:"unassign"`
	ResolutionNotes *string `json:"resolution_notes"`
}

// handleUpdateTicket applies a lifecycle or field change.
func (s *Server) handleUpdateTicket(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var body updateTicketRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	var req service.UpdateRequest
	if body.Status != nil {
		st, err := models.ParseStatus(*body.Status)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_status")
			return
		}
		req.Status = &st
	}
	if body.Priority != nil {
		p, err := models.ParsePriority(*body.Priority)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_priority")
			return
		}
		req.Priority = &p
	}
	if body.Category != nil {
		cat := strings.ToLower(strings.TrimSpace(*body.Category))
		if !validCategory(cat) {
			httpx.Error(w, http.StatusBadRequest,
				"category must be one of "+strings.Join(service.Categories, ", "), "invalid_input")
			return
		}
		req.Category = &cat
	}
	if body.AssigneeID != nil && body.Unassign {
		httpx.Error(w, http.StatusBadRequest, "provide either assignee_id or unassign, not both", "invalid_input")
		return
	}
	if body.AssigneeID != nil {
		if *body.AssigneeID <= 0 {
			httpx.Error(w, http.StatusBadRequest, "assignee_id must be a positive integer", "invalid_input")
			return
		}
		req.AssigneeID = body.AssigneeID
	}
	req.Unassign = body.Unassign
	if body.ResolutionNotes != nil {
		notes := strings.TrimSpace(*body.ResolutionNotes)
		if len(notes) > 10000 {
			httpx.Error(w, http.StatusBadRequest, "resolution_notes must be at most 10000 characters", "invalid_input")
			return
		}
		req.ResolutionNotes = &notes
	}

	ticket, err := s.tickets.Update(r.Context(), actor, id, req)
	if err != nil {
		if errors.Is(err, models.ErrInvalidStatus) {
			httpx.Error(w, http.StatusUnprocessableEntity, err.Error(), "invalid_status_transition")
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"ticket": ticket})
}

// handleDeleteTicket removes a ticket (admin only, enforced by route).
func (s *Server) handleDeleteTicket(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := s.tickets.Delete(r.Context(), actor, id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

// --- comments ---

type addCommentRequest struct {
	Body     string `json:"body"`
	Internal bool   `json:"internal"`
}

// handleListComments returns the ticket thread, filtered by role.
func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticket, err := s.store.Tickets.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !s.tickets.CanView(actor, ticket) {
		httpx.Fail(w, fmt.Errorf("%w: this ticket belongs to another requester", models.ErrForbidden))
		return
	}

	comments, err := s.store.Comments.List(r.Context(), id, actor.Role.IsStaff())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{Items: comments, Total: len(comments), Page: 1, Limit: len(comments), Pages: 1})
}

// handleAddComment appends to the thread.
func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var body addCommentRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	switch {
	case body.Body == "":
		httpx.Error(w, http.StatusBadRequest, "body is required", "invalid_input")
		return
	case len(body.Body) > 20000:
		httpx.Error(w, http.StatusBadRequest, "comment must be at most 20000 characters", "invalid_input")
		return
	}

	comment, err := s.tickets.AddComment(r.Context(), actor, id, body.Body, body.Internal)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, map[string]any{"comment": comment})
}

// handleListEvents returns the immutable ticket timeline.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticket, err := s.store.Tickets.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !s.tickets.CanView(actor, ticket) {
		httpx.Fail(w, fmt.Errorf("%w: this ticket belongs to another requester", models.ErrForbidden))
		return
	}
	limit := httpx.QueryInt(r, "limit", 100, 1, 500)
	events, err := s.store.Events.List(r.Context(), id, limit)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{Items: events, Total: len(events), Page: 1, Limit: limit, Pages: 1})
}

// handleTicketAudit returns audit entries for one ticket.
func (s *Server) handleTicketAudit(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "ticket_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	ticket, err := s.store.Tickets.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !s.tickets.CanView(actor, ticket) {
		httpx.Fail(w, fmt.Errorf("%w: this ticket belongs to another requester", models.ErrForbidden))
		return
	}
	entries, total, err := s.store.Audit.List(r.Context(), &id, 100, 0)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{Items: entries, Total: total, Page: 1, Limit: 100, Pages: httpx.Pages(total, 100)})
}

func validCategory(c string) bool {
	for _, v := range service.Categories {
		if v == c {
			return true
		}
	}
	return false
}
