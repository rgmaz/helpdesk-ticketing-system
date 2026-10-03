package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/notify"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// SLAEngine is the background worker that enforces service level agreements.
//
// It runs a single goroutine on a ticker. A single worker is deliberate: SLA
// transitions are state changes on the same rows, and serialising them avoids
// two ticks racing to emit duplicate breach notifications.
type SLAEngine struct {
	store      *store.Store
	hub        *notify.Hub
	interval   time.Duration
	warnBefore time.Duration

	mu      sync.Mutex
	lastRun time.Time
	running bool

	// Metrics exposed by the health endpoint.
	ticks       uint64
	breaches    uint64
	escalations uint64
	errors      uint64
	lastError   string
}

// NewSLAEngine builds the worker.
func NewSLAEngine(s *store.Store, hub *notify.Hub, interval, warnBefore time.Duration) *SLAEngine {
	return &SLAEngine{
		store:      s,
		hub:        hub,
		interval:   interval,
		warnBefore: warnBefore,
	}
}

// Run drives the ticker loop and blocks until ctx is cancelled.
//
// The name says "Run" rather than "Start" because it does not return after
// launching anything: callers must invoke it in their own goroutine. A single
// instance runs per process, which is what keeps a breach from being detected,
// notified and escalated twice.
func (e *SLAEngine) Run(ctx context.Context) {
	// Run once at boot so breaches that accrued while the process was down are
	// detected immediately rather than after a full interval.
	e.tick(ctx)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	slog.Info("sla engine started", "interval", e.interval, "warn_before", e.warnBefore)

	for {
		select {
		case <-ctx.Done():
			slog.Info("sla engine stopped")
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// SLAStats is a snapshot of engine health.
type SLAStats struct {
	IntervalSeconds float64 `json:"interval_seconds"`
	Ticks           uint64  `json:"ticks"`
	Breaches        uint64  `json:"breaches_detected"`
	Escalations     uint64  `json:"escalations"`
	Errors          uint64  `json:"errors"`
	LastError       string  `json:"last_error,omitempty"`
	LastRun         *string `json:"last_run,omitempty"`
	NextRunEstimate *string `json:"next_run_estimate,omitempty"`
	WarnBeforeMins  float64 `json:"warn_before_minutes"`
}

// Stats returns a snapshot of engine counters.
func (e *SLAEngine) Stats() SLAStats {
	e.mu.Lock()
	defer e.mu.Unlock()

	s := SLAStats{
		IntervalSeconds: e.interval.Seconds(),
		Ticks:           e.ticks,
		Breaches:        e.breaches,
		Escalations:     e.escalations,
		Errors:          e.errors,
		LastError:       e.lastError,
		WarnBeforeMins:  e.warnBefore.Minutes(),
	}
	if !e.lastRun.IsZero() {
		last := e.lastRun.Format(models.TimeLayout)
		next := e.lastRun.Add(e.interval).Format(models.TimeLayout)
		s.LastRun, s.NextRunEstimate = &last, &next
	}
	return s
}

// tick performs one scan pass.
func (e *SLAEngine) tick(ctx context.Context) {
	start := time.Now()

	candidates, err := e.store.Tickets.ScanCandidates(ctx, 500)
	if err != nil {
		e.recordError(err)
		slog.Error("sla scan failed", "error", err)
		return
	}

	now := time.Now().UTC()
	var breaches, escalations int

	// Each ticket is evaluated independently; one failure must not abort the pass.
	for _, t := range candidates {
		func() {
			defer func() {
				if r := recover(); r != nil {
					e.recordError(fmt.Errorf("panic evaluating ticket %d: %v", t.ID, r))
					slog.Error("sla evaluation panic", "ticket", t.TicketNumber, "panic", r)
				}
			}()
			b, esc := e.evaluate(ctx, t, now)
			breaches += b
			escalations += esc
		}()
	}

	e.mu.Lock()
	e.ticks++
	e.breaches += uint64(breaches)
	e.escalations += uint64(escalations)
	e.lastRun = now
	if breaches > 0 || escalations > 0 {
		slog.Info("sla scan complete",
			"tickets", len(candidates), "breaches", breaches, "escalations", escalations,
			"duration_ms", time.Since(start).Milliseconds())
	}
	e.mu.Unlock()
}

func (e *SLAEngine) recordError(err error) {
	e.mu.Lock()
	e.errors++
	e.lastError = err.Error()
	e.mu.Unlock()
}

// evaluate applies every SLA rule to one ticket and returns how many breaches
// and escalations it produced.
func (e *SLAEngine) evaluate(ctx context.Context, t models.Ticket, now time.Time) (int, int) {
	var breaches, escalations int

	policy := e.policyFor(t)

	// --- first response clock ---
	// The response clock runs from creation until a staff member first replies.
	if t.ResponseDueAt != nil && !t.ResponseDueAt.Time.IsZero() && t.FirstResponseAt == nil {
		deadline := t.ResponseDueAt.Time
		switch {
		case now.After(deadline):
			if changed, err := e.store.Tickets.MarkBreached(ctx, t.ID, "response", true); err != nil {
				e.recordError(err)
			} else if changed {
				breaches++
				e.notifyBreach(ctx, t, "response", deadline, policy)
			}
		case now.Add(e.warnBefore).After(deadline):
			// Warn once, only while the ticket is still within the final window
			// and has not already been flagged for this dimension.
			if !t.ResponseBreached && !alreadyWarned(ctx, e.store, t.ID, store.EventResponseWarning) {
				e.notifyWarning(ctx, t, "response", deadline, policy)
			}
		}
	}

	// --- resolution clock ---
	// The resolution clock runs until resolution. A pending ticket still burns
	// the clock: waiting on a third party does not pause the SLA.
	if t.ResolutionDueAt != nil && !t.ResolutionDueAt.Time.IsZero() && !t.Status.IsTerminal() {
		deadline := t.ResolutionDueAt.Time
		alreadyResolved := t.ResolvedAt != nil && !t.ResolvedAt.Time.IsZero()

		switch {
		case !alreadyResolved && now.After(deadline):
			if changed, err := e.store.Tickets.MarkBreached(ctx, t.ID, "resolution", true); err != nil {
				e.recordError(err)
			} else if changed {
				breaches++
				e.notifyBreach(ctx, t, "resolution", deadline, policy)
			}
			// Escalation is graded on how long the deadline has been missed, so
			// it fires at most once per interval rather than once per scan.
			if esc := e.maybeEscalate(ctx, t, policy, deadline, now); esc {
				escalations++
			}
		case !alreadyResolved && now.Add(e.warnBefore).After(deadline):
			if !t.ResolutionBreached && !alreadyWarned(ctx, e.store, t.ID, store.EventResolutionWarning) {
				e.notifyWarning(ctx, t, "resolution", deadline, policy)
			}
		case alreadyResolved:
			// Met or missed? Record the verdict once, at resolution time.
			if resolvedAt := t.ResolvedAt.Time; resolvedAt.After(deadline) {
				if !t.ResolutionBreached {
					if changed, err := e.store.Tickets.MarkBreached(ctx, t.ID, "resolution", true); err != nil {
						e.recordError(err)
					} else if changed {
						breaches++
						e.notifyBreach(ctx, t, "resolution", deadline, policy)
					}
				}
			}
		}
	}

	return breaches, escalations
}

// policyFor resolves the governing policy, tolerating tickets whose policy was
// deactivated or deleted.
func (e *SLAEngine) policyFor(t models.Ticket) models.SLAPolicy {
	if t.SLAPolicyID == nil {
		// Fall back to the policy matching the ticket's priority.
		if p, err := e.store.SLA.ForPriority(context.Background(), t.Priority); err == nil {
			return p
		}
		return models.SLAPolicy{}
	}
	p, err := e.store.SLA.ByID(context.Background(), *t.SLAPolicyID)
	if err != nil {
		return models.SLAPolicy{}
	}
	return p
}

// maybeEscalate raises the escalation level after a breach, up to the policy cap.
//
// The grade is driven by how long the ticket has been overdue, not by how many
// times the engine has run. Without that, a ticket breaching at 09:00 would
// race from level 1 to level 2 within the same second, paging an administrator
// before anyone had a chance to react to the first notification.
//
// Level 1 fires the moment the resolution SLA is missed; each further step
// requires one escalation interval of continued overrun.
func (e *SLAEngine) maybeEscalate(ctx context.Context, t models.Ticket,
	policy models.SLAPolicy, deadline, now time.Time) bool {
	maxLevel := policy.MaxEscalations()
	if maxLevel <= 0 {
		return false
	}

	target := 1
	if interval := policy.EscalationInterval(); interval > 0 && !now.Before(deadline.Add(interval)) {
		target = 2
	}
	if target > maxLevel {
		target = maxLevel
	}

	level, err := e.store.Tickets.RaiseEscalation(ctx, t.ID, target)
	if err != nil || level == 0 {
		if err != nil {
			e.recordError(err)
		}
		return false
	}

	msg := fmt.Sprintf("Ticket %s breached its resolution SLA and was escalated to level %d",
		t.TicketNumber, level)
	_ = e.store.Events.Add(ctx, store.NewEvent{
		TicketID: t.ID, Type: string(store.EventEscalation),
		ToValue: strPtr(fmt.Sprintf("level_%d", level)), Message: msg,
	})

	switch level {
	case 1:
		// Level 1: raise priority one band, never above critical.
		if t.Priority.Rank() < models.PriorityCritical.Rank() {
			newP, _ := models.ParsePriority(nextPriority(t.Priority))
			_, _ = e.store.Tickets.Update(ctx, t.ID, store.UpdateTicket{Priority: &newP})
			_ = e.store.Events.Add(ctx, store.NewEvent{
				TicketID: t.ID, Type: string(store.EventPriorityChanged),
				FromValue: strPtr(string(t.Priority)), ToValue: strPtr(string(newP)),
				Message: fmt.Sprintf("Priority raised %s → %s after SLA breach", t.Priority, newP),
			})
		}
		e.hub.Publish(notify.Target{AllStaff: true, Users: requesterOf(ctx, e.store, t.ID)}, notify.Event{
			Type:     string(store.EventEscalation),
			Title:    fmt.Sprintf("SLA breach: %s", t.TicketNumber),
			Body:     fmt.Sprintf("%s — escalation level %d", t.Title, level),
			Level:    "warning",
			TicketID: &t.ID,
			Payload:  map[string]any{"level": level, "priority": t.Priority},
		})

	case 2:
		// Level 2: pull in an administrator and broadcast loudly.
		if admin, err := e.leastLoadedAdmin(ctx); err == nil && admin.ID > 0 {
			id := admin.ID
			_, _ = e.store.Tickets.Update(ctx, t.ID, store.UpdateTicket{AssigneeID: &id})
			_ = e.store.Events.Add(ctx, store.NewEvent{
				TicketID: t.ID, Type: string(store.EventAssigned),
				FromValue: strPtr(deref(t.AssigneeName)), ToValue: strPtr(admin.DisplayName()),
				Message: fmt.Sprintf("Escalated to administrator %s", admin.DisplayName()),
			})
		}
		e.hub.Publish(notify.Target{Roles: []string{"admin"}}, notify.Event{
			Type:     string(store.EventEscalation),
			Title:    fmt.Sprintf("Critical escalation: %s", t.TicketNumber),
			Body:     fmt.Sprintf("%s — escalation level %d, action required", t.Title, level),
			Level:    "error",
			TicketID: &t.ID,
			Payload:  map[string]any{"level": level},
		})
	}
	return true
}

// leastLoadedAdmin finds the administrator carrying the least overdue work.
func (e *SLAEngine) leastLoadedAdmin(ctx context.Context) (models.User, error) {
	admins, err := e.store.Users.AllStaff(ctx)
	if err != nil {
		return models.User{}, err
	}
	load, err := e.store.Tickets.ActiveLoad(ctx)
	if err != nil {
		return models.User{}, err
	}

	best := models.User{}
	bestLoad := -1
	for _, u := range admins {
		if u.Role != models.RoleAdmin {
			continue
		}
		if n, ok := load[u.ID]; !ok || n < bestLoad || bestLoad < 0 {
			if ok && n >= bestLoad && bestLoad >= 0 && u.ID >= best.ID {
				continue
			}
			best, bestLoad = u, load[u.ID]
		}
	}
	return best, nil
}

func (e *SLAEngine) notifyBreach(ctx context.Context, t models.Ticket, dimension string, deadline time.Time, policy models.SLAPolicy) {
	level := "warning"
	if t.Priority.AtLeast(models.PriorityCritical) {
		level = "error"
	}
	title := fmt.Sprintf("SLA breach: %s", t.TicketNumber)
	body := fmt.Sprintf("%s missed its %s target by %s",
		t.Title, dimension, humanizeDuration(time.Since(deadline)))

	kind := store.EventResolutionBreach
	if dimension == "response" {
		kind = store.EventResponseBreach
	}
	_ = e.store.Events.Add(ctx, store.NewEvent{
		TicketID: t.ID, Type: string(kind), ToValue: strPtr("breached"),
		Message: body,
	})
	_ = e.store.Audit.Log(ctx, store.AuditEntry{
		TicketID: &t.ID, Action: "sla.breach." + dimension,
		ToValue: strPtr("breached"), Details: strPtr(body),
	})

	// The requester should know a commitment was missed; staff need it to act.
	recipients := []int64{t.RequesterID}
	if t.AssigneeID != nil {
		recipients = append(recipients, *t.AssigneeID)
	}
	e.hub.Publish(notify.Target{Users: recipients, AllStaff: true}, notify.Event{
		Type:     string(kind),
		Title:    title,
		Body:     body,
		Level:    level,
		TicketID: &t.ID,
		Payload:  map[string]any{"dimension": dimension, "policy": policy.Name},
	})
	_ = e.store.Notify.CreateMany(ctx, recipients, &t.ID, string(kind), title, body, level)
}

func (e *SLAEngine) notifyWarning(ctx context.Context, t models.Ticket, dimension string, deadline time.Time, policy models.SLAPolicy) {
	body := fmt.Sprintf("%s is due in %s for %s",
		t.TicketNumber, humanizeDuration(deadline.Sub(time.Now())), dimension)

	kind := store.EventResolutionWarning
	if dimension == "response" {
		kind = store.EventResponseWarning
	}
	_ = e.store.Events.Add(ctx, store.NewEvent{
		TicketID: t.ID, Type: string(kind), Message: body,
	})

	var recipients []int64
	if t.AssigneeID != nil {
		recipients = append(recipients, *t.AssigneeID)
	}
	e.hub.Publish(notify.Target{Users: recipients, Roles: []string{"admin"}}, notify.Event{
		Type:     string(kind),
		Title:    fmt.Sprintf("SLA warning: %s", t.TicketNumber),
		Body:     body,
		Level:    "info",
		TicketID: &t.ID,
		Payload:  map[string]any{"dimension": dimension},
	})
}

// alreadyWarned checks whether a warning for this ticket and dimension has
// already been recorded, which keeps the once-per-window behaviour without
// adding a column to the hot-path scan query.
func alreadyWarned(ctx context.Context, s *store.Store, ticketID int64, kind store.SLAEventType) bool {
	events, err := s.Events.List(ctx, ticketID, 50)
	if err != nil {
		return true // fail closed: assume warned rather than spam
	}
	for _, e := range events {
		if e.Type == string(kind) {
			return true
		}
	}
	return false
}

func requesterOf(ctx context.Context, s *store.Store, ticketID int64) []int64 {
	t, err := s.Tickets.ByID(ctx, ticketID)
	if err != nil {
		return nil
	}
	return []int64{t.RequesterID}
}

func nextPriority(p models.Priority) string {
	switch p {
	case models.PriorityLow:
		return string(models.PriorityMedium)
	case models.PriorityMedium:
		return string(models.PriorityHigh)
	default:
		return string(models.PriorityCritical)
	}
}

func deref(s *string) string {
	if s == nil {
		return "unassigned"
	}
	return *s
}

// humanizeDuration renders a duration in the largest sensible unit, e.g.
// "3h 20m" or "2d 4h".
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		days, hours := int(d.Hours())/24, int(d.Hours())%24
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, hours)
	}
}
