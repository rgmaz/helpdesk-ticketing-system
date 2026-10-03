package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// Router picks the technician best suited to take a new ticket.
type Router struct {
	users   *store.UserRepo
	tickets *store.TicketRepo
}

// NewRouter wires the assignment dependencies.
func NewRouter(users *store.UserRepo, tickets *store.TicketRepo) *Router {
	return &Router{users: users, tickets: tickets}
}

// Decision is the outcome of a routing attempt, kept for the audit trail.
type Decision struct {
	AssigneeID   *int64
	AssigneeName string
	Reason       string
	Candidates   int
	Load         int
}

// Assign chooses the active technician with the lightest active-ticket load,
// breaking ties deterministically.
//
// The policy is intentionally simple and explainable rather than clever:
//
//  1. Only active technicians are eligible.
//  2. Active load (non-closed tickets) is the primary sort key, ascending.
//  3. Ties break by the technician with the longest tenure (lowest id), so the
//     same input always produces the same assignment.
//
// Critical tickets additionally prefer technicians who are not already past due,
// so a production outage does not land on someone already underwater.
func (r *Router) Assign(ctx context.Context, t models.Ticket) (Decision, error) {
	techs, err := r.users.Technicians(ctx)
	if err != nil {
		return Decision{}, err
	}
	if len(techs) == 0 {
		// An unstaffed service desk is a normal condition: leave the ticket
		// unassigned so it shows up in the admin "unassigned" queue.
		return Decision{Reason: "no active technicians available"}, nil
	}

	load, err := r.tickets.ActiveLoad(ctx)
	if err != nil {
		return Decision{}, err
	}

	overdue, err := r.overdueLoad(ctx)
	if err != nil {
		return Decision{}, err
	}

	type candidate struct {
		user models.User
		load int
		late int
	}
	candidates := make([]candidate, 0, len(techs))
	for _, u := range techs {
		candidates = append(candidates, candidate{user: u, load: load[u.ID], late: overdue[u.ID]})
	}

	urgent := t.Priority.AtLeast(models.PriorityHigh)
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if urgent && a.late != b.late {
			// Fewer overdue tickets first, but only for urgent work.
			return a.late < b.late
		}
		if a.load != b.load {
			return a.load < b.load
		}
		// Deterministic tie-break: lowest id (earliest account).
		return a.user.ID < b.user.ID
	})

	best := candidates[0]
	reason := fmt.Sprintf("lowest active load (%d ticket%s)", best.load, plural(best.load))
	if urgent && best.late > 0 {
		reason = fmt.Sprintf("lowest overdue count (%d) among high-priority tickets", best.late)
	}

	slog.Info("auto-assigned ticket",
		"ticket", t.TicketNumber, "assignee", best.user.Username, "load", best.load, "reason", reason)

	id := best.user.ID
	return Decision{
		AssigneeID:   &id,
		AssigneeName: best.user.DisplayName(),
		Reason:       reason,
		Candidates:   len(candidates),
		Load:         best.load,
	}, nil
}

// overdueLoad counts per-technician tickets that are already past their
// resolution deadline.
func (r *Router) overdueLoad(ctx context.Context) (map[int64]int, error) {
	rows, err := r.tickets.DB().QueryContext(ctx, `
		SELECT assignee_id, COUNT(*)
		FROM tickets
		WHERE assignee_id IS NOT NULL
		  AND status NOT IN ('closed')
		  AND resolution_due_at IS NOT NULL
		  AND resolution_due_at < strftime('%Y-%m-%dT%H:%M:%SZ','now')
		GROUP BY assignee_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// SLAContext bundles the deadline computation for a newly created ticket.
type SLAContext struct {
	Policy          models.SLAPolicy
	ResponseDueAt   *models.Time
	ResolutionDueAt *models.Time
}

// ComputeDeadlines derives both SLA deadlines from the creation time and the
// policy governing the ticket's priority.
func ComputeDeadlines(policy models.SLAPolicy, createdAt time.Time) SLAContext {
	c := SLAContext{Policy: policy}
	if policy.ID == 0 {
		return c
	}
	c.ResponseDueAt = models.NewTime(createdAt.Add(policy.Response()))
	c.ResolutionDueAt = models.NewTime(createdAt.Add(policy.Resolution()))
	return c
}
