package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/notify"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// Categories are the supported request types. Keeping this closed prevents
// typos from fragmenting the reporting breakdowns.
var Categories = []string{
	"hardware", "software", "network", "access", "email",
	"security", "database", "integration", "general",
}

func validCategory(c string) bool {
	for _, v := range Categories {
		if v == c {
			return true
		}
	}
	return false
}

// Actor is the identity performing an action, resolved from the bearer token.
type Actor struct {
	ID       int64
	Username string
	Role     models.RoleName
}

// Tickets holds the business rules for the ticket lifecycle.
type Tickets struct {
	store  *store.Store
	hub    *notify.Hub
	router *Router
	// now is injectable so tests can control the clock.
	now func() time.Time
}

// NewTickets wires the ticket service.
func NewTickets(s *store.Store, hub *notify.Hub, router *Router) *Tickets {
	return &Tickets{store: s, hub: hub, router: router, now: time.Now}
}

func (t *Tickets) clock() time.Time { return t.now().UTC() }

// CreateRequest is the validated input for ticket creation.
type CreateRequest struct {
	Title       string
	Description string
	Category    string
	Priority    models.Priority
	AssigneeID  *int64
}

// Create applies the SLA policy, runs automated routing, writes the ticket and
// records the resulting timeline events.
//
// Ordering matters: the row must exist before routing and notifications so every
// downstream record can reference its id.
func (t *Tickets) Create(ctx context.Context, actor Actor, req CreateRequest) (models.Ticket, error) {
	now := t.clock()

	policy, err := t.store.SLA.ForPriority(ctx, req.Priority)
	if err != nil {
		// Without a policy the ticket is still valid, just unenforced.
		if !errors.Is(err, models.ErrNotFound) {
			return models.Ticket{}, err
		}
		slog.Warn("no active SLA policy for priority", "priority", req.Priority)
	}

	deadlines := ComputeDeadlines(policy, now)
	if deadlines.ResponseDueAt == nil {
		deadlines.ResponseDueAt = models.NewTime(now)
		deadlines.ResolutionDueAt = models.NewTime(now)
	}

	// End users may not choose their own priority above the default; staff may.
	priority := req.Priority
	if !actor.Role.IsStaff() && priority.Rank() > models.PriorityMedium.Rank() {
		priority = models.PriorityMedium
	}

	ticket, err := t.store.Tickets.Create(ctx, store.NewTicket{
		Title:           strings.TrimSpace(req.Title),
		Description:     strings.TrimSpace(req.Description),
		Category:        req.Category,
		Priority:        priority,
		Status:          models.StatusOpen,
		RequesterID:     actor.ID,
		SLAPolicyID:     policyIDOrNil(policy),
		ResponseDueAt:   deadlines.ResponseDueAt,
		ResolutionDueAt: deadlines.ResolutionDueAt,
	})
	if err != nil {
		return models.Ticket{}, err
	}

	if err := t.record(ctx, ticket.ID, actorIDOrNil(actor.ID), store.EventTicketCreated,
		nil, strPtr(string(models.StatusOpen)),
		fmt.Sprintf("%s created ticket %s", actor.Username, ticket.TicketNumber)); err != nil {
		return models.Ticket{}, err
	}

	// Automated routing: honour an explicit staff assignment, otherwise let the
	// router choose when the policy enables it.
	switch {
	case req.AssigneeID != nil && *req.AssigneeID > 0:
		if err := t.assign(ctx, actor, ticket, *req.AssigneeID, "manually assigned at creation"); err != nil {
			return models.Ticket{}, err
		}
	case policy.AutoAssign:
		decision, err := t.router.Assign(ctx, ticket)
		if err != nil {
			return models.Ticket{}, fmt.Errorf("route ticket: %w", err)
		}
		if decision.AssigneeID != nil {
			if err := t.assign(ctx, actor, ticket, *decision.AssigneeID,
				fmt.Sprintf("auto-assigned: %s", decision.Reason)); err != nil {
				return models.Ticket{}, err
			}
		}
	}

	ticket, err = t.store.Tickets.ByID(ctx, ticket.ID)
	if err != nil {
		return models.Ticket{}, err
	}
	t.announceCreated(ctx, ticket)
	return ticket, nil
}

func policyIDOrNil(p models.SLAPolicy) *int64 {
	if p.ID == 0 {
		return nil
	}
	id := p.ID
	return &id
}

// assign writes the assignee and records the event plus a notification.
func (t *Tickets) assign(ctx context.Context, actor Actor, ticket models.Ticket, assigneeID int64, reason string) error {
	assignee, err := t.store.Users.ByID(ctx, assigneeID)
	if err != nil {
		return err
	}
	if !assignee.Active || !assignee.Role.IsStaff() {
		return fmt.Errorf("%w: tickets can only be assigned to active staff accounts", models.ErrInvalidInput)
	}

	id := assigneeID
	updated, err := t.store.Tickets.Update(ctx, ticket.ID, store.UpdateTicket{AssigneeID: &id})
	if err != nil {
		return err
	}

	from := "unassigned"
	if ticket.AssigneeName != nil {
		from = *ticket.AssigneeName
	}
	to := assignee.DisplayName()

	eventType := store.EventAssigned
	if actor.ID == 0 || actor.ID == ticket.RequesterID {
		eventType = store.EventAutoAssigned
	}
	if err := t.record(ctx, ticket.ID, actorIDOrNil(actor.ID), eventType,
		strPtr(from), strPtr(to),
		fmt.Sprintf("%s assigned to %s (%s)", titleOr(actor, "System"), to, reason)); err != nil {
		return err
	}

	_, _ = t.store.Notify.Create(ctx, assigneeID, &ticket.ID, string(eventType),
		fmt.Sprintf("Ticket %s assigned to you", ticket.TicketNumber),
		fmt.Sprintf("%s — %s", updated.Title, reason), "info")

	t.hub.Publish(notify.Target{Users: []int64{assigneeID}, Roles: []string{"admin"}}, notify.Event{
		Type:     string(eventType),
		Title:    fmt.Sprintf("Ticket %s assigned to %s", ticket.TicketNumber, to),
		Body:     updated.Title,
		Level:    "info",
		TicketID: &ticket.ID,
		Payload:  map[string]any{"assignee_id": assigneeID, "assignee_name": to},
	})
	return nil
}

// UpdateRequest carries validated, partial changes to a ticket.
type UpdateRequest struct {
	Status          *models.TicketStatus
	Priority        *models.Priority
	Category        *string
	AssigneeID      *int64 // nil means leave alone
	Unassign        bool
	ResolutionNotes *string
}

// Update applies a lifecycle transition or field change, enforcing role rules
// and emitting the corresponding events and notifications.
func (t *Tickets) Update(ctx context.Context, actor Actor, ticketID int64, req UpdateRequest) (models.Ticket, error) {
	current, err := t.store.Tickets.ByID(ctx, ticketID)
	if err != nil {
		return models.Ticket{}, err
	}
	if err := t.assertVisible(actor, current); err != nil {
		return models.Ticket{}, err
	}

	now := t.clock()
	in := store.UpdateTicket{}
	notes := []string{}

	// --- status transition ---
	if req.Status != nil && *req.Status != current.Status {
		next := *req.Status
		if !current.Status.CanTransitionTo(next) {
			return models.Ticket{}, fmt.Errorf("%w: cannot move from %s to %s (allowed: %s)",
				models.ErrInvalidStatus, current.Status, next, joinStatuses(current.Status.NextStatuses()))
		}
		// Only staff may move a ticket into or out of resolution states; end
		// users may close their own resolved ticket (confirming the fix) or
		// reopen a closed one.
		if !actor.Role.IsStaff() && next != models.StatusClosed && next != models.StatusReopened {
			return models.Ticket{}, fmt.Errorf("%w: only staff can set a ticket to %s", models.ErrForbidden, next)
		}
		if next == models.StatusClosed && !current.Status.CanTransitionTo(models.StatusClosed) {
			return models.Ticket{}, fmt.Errorf("%w: %s must be resolved before closing", models.ErrInvalidStatus, current.Status)
		}

		in.Status = &next
		notes = append(notes, fmt.Sprintf("status %s → %s", current.Status, next))

		// Timestamp bookkeeping and clock control for each SLA dimension.
		switch next {
		case models.StatusInProgress:
			// Starting work stops the response clock.
			if current.FirstResponseAt == nil {
				in.FirstResponseAt = models.NewTime(now)
			}
		case models.StatusResolved:
			in.ResolvedAt = models.NewTime(now)
			if req.ResolutionNotes != nil && strings.TrimSpace(*req.ResolutionNotes) != "" {
				in.ResolutionNotes = req.ResolutionNotes
			}
			// A resolution without a prior response still counts as a response.
			if current.FirstResponseAt == nil {
				in.FirstResponseAt = models.NewTime(now)
			}
		case models.StatusClosed:
			in.ClosedAt = models.NewTime(now)
			if current.ResolvedAt == nil {
				// Closing straight from an open state still records a
				// resolution instant so MTTR and throughput stay accurate.
				in.ResolvedAt = models.NewTime(now)
			}
		case models.StatusReopened:
			// Reopening invalidates the prior resolution and closure instants,
			// then restarts the resolution clock from now.
			in.ClearResolvedAt = true
			in.ClearClosedAt = true
			if current.SLAPolicyID != nil {
				if policy, perr := t.store.SLA.ByID(ctx, *current.SLAPolicyID); perr == nil {
					d := ComputeDeadlines(policy, now)
					in.ResolutionDueAt = d.ResolutionDueAt
					in.ResponseDueAt = d.ResponseDueAt
				}
			}
			in.ClearEscalation = true
		}
	}

	// --- priority change ---
	if req.Priority != nil && *req.Priority != current.Priority {
		// Only staff may raise or lower urgency; end users may only lower it.
		if !actor.Role.IsStaff() && req.Priority.Rank() > current.Priority.Rank() {
			return models.Ticket{}, fmt.Errorf("%w: only staff can raise ticket priority", models.ErrForbidden)
		}
		notes = append(notes, fmt.Sprintf("priority %s → %s", current.Priority, *req.Priority))

		in.Priority = req.Priority
		// Re-deriving deadlines on every priority change keeps the SLA
		// guarantee consistent with urgency.
		if policy, perr := t.store.SLA.ForPriority(ctx, *req.Priority); perr == nil {
			d := ComputeDeadlines(policy, now)
			in.ResponseDueAt = d.ResponseDueAt
			in.ResolutionDueAt = d.ResolutionDueAt
			id := policy.ID
			in.SLAPolicyID = &id
		}
	}

	// --- category ---
	if req.Category != nil && *req.Category != current.Category {
		if !actor.Role.IsStaff() {
			return models.Ticket{}, fmt.Errorf("%w: only staff can recategorise a ticket", models.ErrForbidden)
		}
		in.Category = req.Category
		notes = append(notes, fmt.Sprintf("category %s → %s", current.Category, *req.Category))
	}

	// --- resolution notes ---
	if req.ResolutionNotes != nil && *req.ResolutionNotes != current.ResolutionNotes {
		if !actor.Role.IsStaff() {
			return models.Ticket{}, fmt.Errorf("%w: only staff can edit resolution notes", models.ErrForbidden)
		}
		in.ResolutionNotes = req.ResolutionNotes
		notes = append(notes, "resolution notes updated")
	}

	// --- assignment ---
	var newAssignee *models.User
	if req.Unassign {
		if !actor.Role.IsStaff() {
			return models.Ticket{}, fmt.Errorf("%w: only staff can unassign a ticket", models.ErrForbidden)
		}
		in.ClearAssignee = true
		notes = append(notes, "assignee removed")
	} else if req.AssigneeID != nil && (current.AssigneeID == nil || *current.AssigneeID != *req.AssigneeID) {
		if !actor.Role.IsStaff() {
			return models.Ticket{}, fmt.Errorf("%w: only staff can reassign a ticket", models.ErrForbidden)
		}
		in.AssigneeID = req.AssigneeID
		u, err := t.store.Users.ByID(ctx, *req.AssigneeID)
		if err != nil {
			return models.Ticket{}, err
		}
		newAssignee = &u
		from := "unassigned"
		if current.AssigneeName != nil {
			from = *current.AssigneeName
		}
		notes = append(notes, fmt.Sprintf("assignee %s → %s", from, u.DisplayName()))
	}

	if len(notes) == 0 && in.Status == nil && in.Priority == nil && in.Category == nil &&
		in.ResolutionNotes == nil && in.AssigneeID == nil && !in.ClearAssignee {
		// Nothing changed; return the current row untouched.
		return current, nil
	}

	updated, err := t.store.Tickets.Update(ctx, ticketID, in)
	if err != nil {
		return models.Ticket{}, err
	}

	t.emitUpdateEvents(ctx, actor, current, updated, notes, newAssignee)
	return updated, nil
}

// emitUpdateEvents records timeline/audit entries and pushes notifications.
func (t *Tickets) emitUpdateEvents(ctx context.Context, actor Actor, before, after models.Ticket, notes []string, newAssignee *models.User) {
	actorID := actorIDOrNil(actor.ID)

	if statusNote := notesHasPrefix(notes, "status "); statusNote != "" {
		eventType := store.EventStatusChange
		msg := fmt.Sprintf("%s changed status: %s", titleOr(actor, "System"), statusNote)
		_ = t.record(ctx, after.ID, actorID, eventType,
			strPtr(string(before.Status)), strPtr(string(after.Status)), msg)

		switch after.Status {
		case models.StatusResolved:
			_ = t.record(ctx, after.ID, nil, store.EventTicketResolved, nil, nil,
				fmt.Sprintf("Ticket %s resolved", after.TicketNumber))
		case models.StatusClosed:
			_ = t.record(ctx, after.ID, nil, store.EventTicketClosed, nil, nil,
				fmt.Sprintf("Ticket %s closed", after.TicketNumber))
		case models.StatusReopened:
			_ = t.record(ctx, after.ID, nil, store.EventTicketReopened, nil, nil,
				fmt.Sprintf("Ticket %s reopened", after.TicketNumber))
		}
	}
	if p := notesHasPrefix(notes, "priority "); p != "" {
		_ = t.record(ctx, after.ID, actorID, store.EventPriorityChanged,
			strPtr(string(before.Priority)), strPtr(string(after.Priority)),
			fmt.Sprintf("%s changed priority: %s", titleOr(actor, "System"), p))
	}
	if newAssignee != nil {
		from := "unassigned"
		if before.AssigneeName != nil {
			from = *before.AssigneeName
		}
		to := newAssignee.DisplayName()
		_ = t.record(ctx, after.ID, actorID, store.EventAssigned,
			strPtr(from), strPtr(to),
			fmt.Sprintf("%s reassigned ticket to %s", titleOr(actor, "System"), to))

		_, _ = t.store.Notify.Create(ctx, newAssignee.ID, &after.ID, string(store.EventAssigned),
			fmt.Sprintf("Ticket %s assigned to you", after.TicketNumber),
			after.Title, "info")

		t.hub.Publish(notify.Target{Users: []int64{newAssignee.ID}, Roles: []string{"admin"}}, notify.Event{
			Type:     string(store.EventAssigned),
			Title:    fmt.Sprintf("Ticket %s assigned to you", after.TicketNumber),
			Body:     after.Title,
			Level:    "info",
			TicketID: &after.ID,
		})
	}
	if notesHasPrefix(notes, "category ") != "" || notesHasPrefix(notes, "resolution notes") != "" {
		_ = t.record(ctx, after.ID, actorID, store.EventTicketUpdated, nil, nil,
			fmt.Sprintf("%s updated %s", titleOr(actor, "System"), strings.Join(notes, ", ")))
	}

	_ = t.store.Audit.Log(ctx, store.AuditEntry{
		ActorID:   actorID,
		TicketID:  &after.ID,
		Action:    "ticket.update",
		FromValue: strPtr(string(before.Status)),
		ToValue:   strPtr(string(after.Status)),
		Details:   strPtr(strings.Join(notes, "; ")),
	})

	// Notify the requester about any change they did not make themselves, and
	// broadcast to staff so open queues stay live.
	recipients := []int64{after.RequesterID}
	if after.AssigneeID != nil && *after.AssigneeID != actor.ID {
		recipients = append(recipients, *after.AssigneeID)
	}
	t.hub.Publish(notify.Target{Users: recipients, AllStaff: true}, notify.Event{
		Type:     "ticket_updated",
		Title:    fmt.Sprintf("%s updated", after.TicketNumber),
		Body:     after.Title,
		Level:    "info",
		TicketID: &after.ID,
		Payload: map[string]any{
			"status": after.Status, "priority": after.Priority, "notes": notes,
		},
	})
}

// AddComment appends to a ticket thread. Staff comments can be internal notes,
// which are never exposed to end users.
func (t *Tickets) AddComment(ctx context.Context, actor Actor, ticketID int64, body string, internal bool) (models.Comment, error) {
	ticket, err := t.store.Tickets.ByID(ctx, ticketID)
	if err != nil {
		return models.Comment{}, err
	}
	if err := t.assertVisible(actor, ticket); err != nil {
		return models.Comment{}, err
	}
	if internal && !actor.Role.IsStaff() {
		return models.Comment{}, fmt.Errorf("%w: only staff can add internal notes", models.ErrForbidden)
	}

	comment, err := t.store.Comments.Add(ctx, ticketID, actor.ID, strings.TrimSpace(body), internal)
	if err != nil {
		return models.Comment{}, err
	}

	_ = t.record(ctx, ticketID, actorIDOrNil(actor.ID), store.EventCommentAdded, nil, nil,
		fmt.Sprintf("%s added a comment", titleOr(actor, "System")))

	// A public comment from staff counts as the first response and stops the
	// response SLA clock. This is the natural moment it happens in practice.
	var update store.UpdateTicket
	if ticket.FirstResponseAt == nil && actor.Role.IsStaff() && !internal {
		update.FirstResponseAt = &models.Time{Time: t.clock()}
		if _, err := t.store.Tickets.Update(ctx, ticketID, update); err == nil {
			ticket.FirstResponseAt = update.FirstResponseAt
		}
	}

	if !internal {
		// Notify the other party: the requester, or the assignee/other staff.
		var target []int64
		if actor.Role.IsStaff() {
			target = []int64{ticket.RequesterID}
		} else {
			if ticket.AssigneeID != nil {
				target = append(target, *ticket.AssigneeID)
			}
			target = append(target, staffIDs(ctx, t.store)...)
		}
		t.hub.Publish(notify.Target{Users: target, Roles: []string{"admin"}}, notify.Event{
			Type:     "comment_added",
			Title:    fmt.Sprintf("New comment on %s", ticket.TicketNumber),
			Body:     truncate(comment.Body, 120),
			Level:    "info",
			TicketID: &ticket.ID,
			Payload:  map[string]any{"author": actor.Username},
		})
		_, _ = t.store.Notify.Create(ctx, ticket.RequesterID, &ticket.ID, "comment_added",
			fmt.Sprintf("New comment on %s", ticket.TicketNumber), truncate(comment.Body, 200), "info")
	}
	return comment, nil
}

// Delete removes a ticket. Admin only.
func (t *Tickets) Delete(ctx context.Context, actor Actor, ticketID int64) error {
	if !actor.Role.IsAdmin() {
		return fmt.Errorf("%w: only administrators can delete tickets", models.ErrForbidden)
	}
	ticket, err := t.store.Tickets.ByID(ctx, ticketID)
	if err != nil {
		return err
	}
	if err := t.store.Tickets.Delete(ctx, ticketID); err != nil {
		return err
	}
	_ = t.store.Audit.Log(ctx, store.AuditEntry{
		ActorID: actorIDOrNil(actor.ID),
		Action:  "ticket.delete",
		Details: strPtr(fmt.Sprintf("deleted %s (%s)", ticket.TicketNumber, ticket.Title)),
	})
	t.hub.Publish(notify.Target{AllStaff: true, Users: []int64{ticket.RequesterID}}, notify.Event{
		Type:     "ticket_deleted",
		Title:    fmt.Sprintf("%s deleted", ticket.TicketNumber),
		Body:     ticket.Title,
		Level:    "warning",
		TicketID: &ticket.ID,
	})
	return nil
}

// assertVisible enforces read access: end users see only their own tickets.
func (t *Tickets) assertVisible(actor Actor, ticket models.Ticket) error {
	if actor.Role.IsStaff() {
		return nil
	}
	if ticket.RequesterID != actor.ID {
		return fmt.Errorf("%w: this ticket belongs to another requester", models.ErrForbidden)
	}
	return nil
}

// CanView is the exported form of the visibility rule, used by list endpoints
// to decide whether a caller may see a given ticket.
func (t *Tickets) CanView(actor Actor, ticket models.Ticket) bool {
	return t.assertVisible(actor, ticket) == nil
}

// record writes a timeline event, logging rather than failing the request so a
// non-critical audit gap cannot roll back completed business logic.
func (t *Tickets) record(ctx context.Context, ticketID int64, actorID *int64, typ store.SLAEventType, from, to *string, msg string) error {
	if err := t.store.Events.Add(ctx, store.NewEvent{
		TicketID: ticketID, ActorID: actorID, Type: string(typ),
		FromValue: from, ToValue: to, Message: msg,
	}); err != nil {
		slog.Error("record ticket event", "ticket_id", ticketID, "type", typ, "error", err)
		return err
	}
	return nil
}

// announceCreated pushes the new ticket to staff and the requester.
func (t *Tickets) announceCreated(ctx context.Context, ticket models.Ticket) {
	ev := notify.Event{
		Type:     "ticket_created",
		Title:    fmt.Sprintf("New %s ticket %s", ticket.Priority, ticket.TicketNumber),
		Body:     ticket.Title,
		Level:    "info",
		TicketID: &ticket.ID,
		Payload: map[string]any{
			"category": ticket.Category,
			"priority": ticket.Priority,
			"status":   ticket.Status,
		},
	}
	if ticket.Priority.AtLeast(models.PriorityCritical) {
		ev.Level = "error"
	}
	t.hub.Publish(notify.Target{Users: []int64{ticket.RequesterID}, AllStaff: true}, ev)

	_, _ = t.store.Notify.Create(ctx, ticket.RequesterID, &ticket.ID, "ticket_created",
		fmt.Sprintf("Ticket %s created", ticket.TicketNumber), ticket.Title, "info")
}

// staffIDs returns every active staff account id, used for comment broadcasts.
func staffIDs(ctx context.Context, s *store.Store) []int64 {
	users, err := s.Users.AllStaff(ctx)
	if err != nil {
		slog.Error("load staff list", "error", err)
		return nil
	}
	out := make([]int64, 0, len(users))
	for _, u := range users {
		out = append(out, u.ID)
	}
	return out
}

// ValidateCreate normalises and checks a create request.
func ValidateCreate(req *CreateRequest) error {
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)

	if len(req.Title) < 5 {
		return fmt.Errorf("%w: title must be at least 5 characters", models.ErrInvalidInput)
	}
	if len(req.Title) > 200 {
		return fmt.Errorf("%w: title must be at most 200 characters", models.ErrInvalidInput)
	}
	if len(req.Description) > 10000 {
		return fmt.Errorf("%w: description must be at most 10000 characters", models.ErrInvalidInput)
	}
	if req.Category == "" {
		req.Category = "general"
	}
	if !validCategory(req.Category) {
		return fmt.Errorf("%w: category must be one of %s", models.ErrInvalidInput, strings.Join(Categories, ", "))
	}
	if req.Priority == "" {
		req.Priority = models.PriorityMedium
	}
	if _, err := models.ParsePriority(string(req.Priority)); err != nil {
		return fmt.Errorf("%w: %s", models.ErrInvalidInput, err)
	}
	return nil
}

// --- small helpers ---

func actorIDOrNil(id int64) *int64 {
	if id <= 0 {
		return nil
	}
	v := id
	return &v
}

func strPtr(s string) *string { return &s }

func titleOr(a Actor, fallback string) string {
	if a.Username != "" {
		return a.Username
	}
	return fallback
}

func joinStatuses(list []models.TicketStatus) string {
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// notesHasPrefix returns the first note starting with prefix, or "" if none.
func notesHasPrefix(notes []string, prefix string) string {
	for _, n := range notes {
		if strings.HasPrefix(n, prefix) {
			return n
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
