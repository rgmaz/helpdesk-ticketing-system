package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RoleName enumerates the RBAC roles. Roles live in a `roles` table but the
// canonical names are fixed in code so authorization logic cannot drift.
type RoleName string

const (
	RoleUser       RoleName = "user"
	RoleTechnician RoleName = "technician"
	RoleAdmin      RoleName = "admin"
)

func (r RoleName) Valid() bool {
	switch r {
	case RoleUser, RoleTechnician, RoleAdmin:
		return true
	}
	return false
}

func (r RoleName) IsStaff() bool { return r == RoleTechnician || r == RoleAdmin }
func (r RoleName) IsAdmin() bool { return r == RoleAdmin }

func ParseRole(s string) (RoleName, error) {
	r := RoleName(strings.ToLower(strings.TrimSpace(s)))
	if !r.Valid() {
		return "", fmt.Errorf("unknown role %q", s)
	}
	return r, nil
}

// TicketStatus is the ticket lifecycle state.
type TicketStatus string

const (
	StatusOpen       TicketStatus = "open"
	StatusInProgress TicketStatus = "in_progress"
	StatusPending    TicketStatus = "pending"
	StatusResolved   TicketStatus = "resolved"
	StatusClosed     TicketStatus = "closed"
	StatusReopened   TicketStatus = "reopened"
)

var AllStatuses = []TicketStatus{
	StatusOpen, StatusInProgress, StatusPending, StatusResolved, StatusClosed, StatusReopened,
}

func ParseStatus(s string) (TicketStatus, error) {
	st := TicketStatus(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range AllStatuses {
		if v == st {
			return v, nil
		}
	}
	return "", fmt.Errorf("unknown status %q", s)
}

// IsTerminal reports whether the ticket has reached a resting state that stops
// the SLA resolution clock.
func (s TicketStatus) IsTerminal() bool { return s == StatusClosed }

// IsOpen reports whether a ticket in this state is still live work: it keeps its
// SLA clocks running, counts against a technician's workload, and is returned by
// the open-ticket filters.
//
// Resolved is deliberately included. The fix has been applied but the requester
// has not yet confirmed, so the ticket stays subject to its resolution deadline
// and the technician still owns it. Only closure ends that, which is why this is
// the exact inverse of IsTerminal.
func (s TicketStatus) IsOpen() bool { return !s.IsTerminal() }

// CanTransition encodes the allowed ticket lifecycle moves. Transitions are
// deliberately explicit rather than a free-form status write so that the
// audit trail and SLA clock handling stay predictable.
var transitions = map[TicketStatus][]TicketStatus{
	StatusOpen:       {StatusInProgress, StatusPending, StatusResolved, StatusClosed},
	StatusInProgress: {StatusPending, StatusResolved, StatusClosed},
	StatusPending:    {StatusInProgress, StatusResolved, StatusClosed},
	StatusResolved:   {StatusClosed, StatusReopened},
	StatusClosed:     {StatusReopened},
	StatusReopened:   {StatusInProgress, StatusPending, StatusResolved, StatusClosed},
}

func (s TicketStatus) CanTransitionTo(next TicketStatus) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// NextStatuses lists legal targets, used by the API and the frontend to render
// only valid actions.
func (s TicketStatus) NextStatuses() []TicketStatus { return transitions[s] }

// Label is a human-readable form for the UI. Labels live next to the enum so a
// new status cannot ship without one.
func (s TicketStatus) Label() string {
	switch s {
	case StatusOpen:
		return "Open"
	case StatusInProgress:
		return "In progress"
	case StatusPending:
		return "Pending"
	case StatusResolved:
		return "Resolved"
	case StatusClosed:
		return "Closed"
	case StatusReopened:
		return "Reopened"
	}
	return string(s)
}

// Priority is the ticket severity.
type Priority string

const (
	PriorityLow      Priority = "low"
	PriorityMedium   Priority = "medium"
	PriorityHigh     Priority = "high"
	PriorityCritical Priority = "critical"
)

var AllPriorities = []Priority{PriorityLow, PriorityMedium, PriorityHigh, PriorityCritical}

func ParsePriority(s string) (Priority, error) {
	p := Priority(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range AllPriorities {
		if v == p {
			return v, nil
		}
	}
	return "", fmt.Errorf("unknown priority %q", s)
}

// Rank orders priorities for SLA selection and escalation comparisons.
// Higher rank means more urgent.
func (p Priority) Rank() int {
	switch p {
	case PriorityCritical:
		return 4
	case PriorityHigh:
		return 3
	case PriorityMedium:
		return 2
	case PriorityLow:
		return 1
	}
	return 0
}

func (p Priority) AtLeast(min Priority) bool { return p.Rank() >= min.Rank() }

// Label is a human-readable form for the UI.
func (p Priority) Label() string {
	switch p {
	case PriorityCritical:
		return "Critical"
	case PriorityHigh:
		return "High"
	case PriorityMedium:
		return "Medium"
	case PriorityLow:
		return "Low"
	}
	return string(p)
}

// PriorityFromRank returns the priority matching a rank, used by the escalation
// engine when elevating one band.
func PriorityFromRank(rank int) (Priority, error) {
	for _, p := range AllPriorities {
		if p.Rank() == rank {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown priority rank %d", rank)
}

// TimeLayout is how timestamps are serialised in JSON. RFC3339 with a fixed
// offset keeps the wire format stable across SQLite and Go.
const TimeLayout = "2006-01-02T15:04:05Z07:00"

// Time is a time.Time that marshals to RFC3339 and tolerates NULL columns.
type Time struct{ time.Time }

// NewTime wraps a time.Time.
func NewTime(t time.Time) *Time { return &Time{Time: t} }

func (t Time) MarshalJSON() ([]byte, error) {
	if t.Time.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + t.Time.Format(TimeLayout) + `"`), nil
}

func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	parsed, err := time.Parse(TimeLayout, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// Value implements driver.Valuer. Nil is stored for the zero value so nullable
// columns stay NULL rather than becoming an epoch timestamp.
func (t *Time) Value() (driver.Value, error) {
	if t == nil || t.Time.IsZero() {
		return nil, nil
	}
	return t.Time.UTC().Format(TimeLayout), nil
}

// Scan implements sql.Scanner, accepting both TEXT timestamps and NULL.
func (t *Time) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		t.Time = time.Time{}
		return nil
	case time.Time:
		t.Time = v
		return nil
	case string:
		if v == "" {
			t.Time = time.Time{}
			return nil
		}
		for _, layout := range []string{TimeLayout, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05", time.RFC3339Nano} {
			if parsed, err := time.Parse(layout, v); err == nil {
				t.Time = parsed
				return nil
			}
		}
		return fmt.Errorf("cannot parse %q as timestamp", v)
	default:
		return fmt.Errorf("cannot scan %T into Time", src)
	}
}

// NullTime is a helper for building nullable SQL parameters.
func NullTime(t *Time) any {
	if t == nil || t.Time.IsZero() {
		return nil
	}
	return t.Time.UTC().Format(TimeLayout)
}

// JSONMap is a generic JSON object column.
type JSONMap map[string]any

func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (m *JSONMap) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*m = nil
		return nil
	case []byte:
		return json.Unmarshal(v, m)
	case string:
		if v == "" {
			*m = nil
			return nil
		}
		return json.Unmarshal([]byte(v), m)
	default:
		return fmt.Errorf("cannot scan %T into JSONMap", src)
	}
}

func (m JSONMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(map[string]any(m))
}

// Role is an entry from the roles lookup table.
type Role struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// User is an account that can authenticate.
type User struct {
	ID           int64    `json:"id"`
	Username     string   `json:"username"`
	Email        string   `json:"email"`
	FullName     string   `json:"full_name"`
	Role         RoleName `json:"role"`
	PasswordHash string   `json:"-"`
	Active       bool     `json:"active"`
	CreatedAt    *Time    `json:"created_at"`
	UpdatedAt    *Time    `json:"updated_at"`
}

// DisplayName prefers the full name and falls back to the username.
func (u User) DisplayName() string {
	if strings.TrimSpace(u.FullName) != "" {
		return u.FullName
	}
	return u.Username
}

// SLAPolicy defines response and resolution targets for a priority band.
// Minutes are the storage unit; Response and Resolution expose durations for
// callers that need them.
type SLAPolicy struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Priority          string `json:"priority"`
	ResponseMinutes   int    `json:"response_minutes"`
	ResolutionMinutes int    `json:"resolution_minutes"`
	EscalationMinutes int    `json:"escalation_minutes"`
	AutoAssign        bool   `json:"auto_assign"`
	Active            bool   `json:"active"`
	Description       string `json:"description"`
	CreatedAt         *Time  `json:"created_at"`
}

// Response is the first-response target as a duration.
func (p SLAPolicy) Response() time.Duration { return time.Duration(p.ResponseMinutes) * time.Minute }

// Resolution is the resolution target as a duration.
func (p SLAPolicy) Resolution() time.Duration {
	return time.Duration(p.ResolutionMinutes) * time.Minute
}

// EscalationInterval is the delay between escalation steps.
func (p SLAPolicy) EscalationInterval() time.Duration {
	return time.Duration(p.EscalationMinutes) * time.Minute
}

// MaxEscalations returns how many escalation steps a policy permits. The stored
// escalation_minutes is the interval between steps, so two intervals of runway
// yields two steps. Escalation noise is capped at two steps so a long-running
// breach cannot spam administrators forever.
func (p SLAPolicy) MaxEscalations() int {
	if p.EscalationMinutes <= 0 || p.ResolutionMinutes <= 0 {
		return 0
	}
	if steps := p.ResolutionMinutes / p.EscalationMinutes; steps < 2 {
		return steps
	}
	return 2
}

// TicketEvent is one entry on a ticket's timeline. A nil ActorID means the
// action was performed by a background worker rather than a person.
type TicketEvent struct {
	ID        int64   `json:"id"`
	TicketID  int64   `json:"ticket_id"`
	ActorID   int64   `json:"actor_id"`
	ActorName string  `json:"actor_name"`
	Type      string  `json:"type"`
	FromValue *string `json:"from_value"`
	ToValue   *string `json:"to_value"`
	Message   string  `json:"message"`
	CreatedAt *Time   `json:"created_at"`
}

// Notification is a persisted message delivered to one user.
type Notification struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	TicketID  *int64 `json:"ticket_id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Level     string `json:"level"`
	ReadAt    *Time  `json:"read_at"`
	CreatedAt *Time  `json:"created_at"`
}

// Ticket is the central entity.
type Ticket struct {
	ID                 int64        `json:"id"`
	TicketNumber       string       `json:"ticket_number"`
	Title              string       `json:"title"`
	Description        string       `json:"description"`
	Category           string       `json:"category"`
	Priority           Priority     `json:"priority"`
	Status             TicketStatus `json:"status"`
	RequesterID        int64        `json:"requester_id"`
	RequesterName      string       `json:"requester_name"`
	AssigneeID         *int64       `json:"assignee_id"`
	AssigneeName       *string      `json:"assignee_name"`
	SLAPolicyID        *int64       `json:"sla_policy_id"`
	SLAPolicyName      *string      `json:"sla_policy_name"`
	ResponseDueAt      *Time        `json:"sla_response_due_at"`
	ResolutionDueAt    *Time        `json:"sla_resolution_due_at"`
	FirstResponseAt    *Time        `json:"first_response_at"`
	ResolutionNotes    string       `json:"resolution_notes"`
	ResolvedAt         *Time        `json:"resolved_at"`
	ClosedAt           *Time        `json:"closed_at"`
	ResponseBreached   bool         `json:"response_breached"`
	ResolutionBreached bool         `json:"resolution_breached"`
	EscalationLevel    int          `json:"escalation_level"`
	CommentCount       int          `json:"comment_count"`
	CreatedAt          *Time        `json:"created_at"`
	UpdatedAt          *Time        `json:"updated_at"`

	// Snippet is an FTS5-highlighted excerpt, populated only on search results.
	Snippet string `json:"snippet,omitempty"`

	// Derived, populated on read for list/detail responses so every renderer can
	// show SLA badges and countdowns without recomputing them.
	State             SLAState       `json:"sla_state"`
	ResponseRemaining *time.Duration `json:"sla_response_remaining_seconds,omitempty"`
	ResolutionRemain  *time.Duration `json:"sla_resolution_remaining_seconds,omitempty"`
}

// DeriveSLA populates the read-only SLA fields in one pass. Callers should use
// this rather than SLAStateOf and DeriveSLATimers separately so the two can never
// disagree about the same instant in time.
func (t *Ticket) DeriveSLA(now time.Time, atRiskMinutes int) {
	t.DeriveSLATimers(now)
	t.State = t.SLAStateOf(now, atRiskMinutes)
}

// SLAEvent records a breach, warning or escalation produced by the SLA engine.
type SLAEvent struct {
	ID        int64  `json:"id"`
	TicketID  int64  `json:"ticket_id"`
	TicketNum string `json:"ticket_number"`
	Type      string `json:"type"`
	Level     int    `json:"level"`
	Message   string `json:"message"`
	CreatedAt *Time  `json:"created_at"`
	EventAt   *Time  `json:"event_at"`
}

// AuditLog captures every state mutation for accountability.
type AuditLog struct {
	ID        int64   `json:"id"`
	ActorID   *int64  `json:"actor_id"`
	ActorName *string `json:"actor_name"`
	TicketID  *int64  `json:"ticket_id"`
	Action    string  `json:"action"`
	FromValue *string `json:"from_value"`
	ToValue   *string `json:"to_value"`
	Details   *string `json:"details"`
	CreatedAt *Time   `json:"created_at"`
}

// Comment is a message on a ticket thread.
type Comment struct {
	ID         int64  `json:"id"`
	TicketID   int64  `json:"ticket_id"`
	AuthorID   int64  `json:"author_id"`
	AuthorName string `json:"author_name"`
	AuthorRole string `json:"author_role"`
	Body       string `json:"body"`
	Internal   bool   `json:"internal"`
	CreatedAt  *Time  `json:"created_at"`
}

// TechnicianWorkload is one row of the assignment/workload report.
type TechnicianWorkload struct {
	UserID         int64    `json:"user_id"`
	Username       string   `json:"username"`
	FullName       string   `json:"full_name"`
	ActiveTickets  int      `json:"active_tickets"`
	OverdueTickets int      `json:"overdue_tickets"`
	Resolved30d    int      `json:"resolved_30d"`
	AvgResolutionH *float64 `json:"avg_resolution_hours_30d"`
}

// DeriveSLATimers computes the remaining time on each SLA clock relative to now.
// Both are nil once the corresponding clock has stopped, which keeps the UI from
// showing a running countdown on work that is already done.
func (t *Ticket) DeriveSLATimers(now time.Time) {
	t.ResponseRemaining = nil
	t.ResolutionRemain = nil
	if t.Status.IsTerminal() {
		return
	}
	if t.ResponseDueAt != nil && !t.ResponseDueAt.Time.IsZero() && t.FirstResponseAt == nil {
		d := t.ResponseDueAt.Time.Sub(now)
		t.ResponseRemaining = &d
	}
	if t.ResolutionDueAt != nil && !t.ResolutionDueAt.Time.IsZero() {
		d := t.ResolutionDueAt.Time.Sub(now)
		t.ResolutionRemain = &d
	}
}

// SLAState summarises ticket health for badges and sort keys.
type SLAState string

const (
	SLAOnTrack  SLAState = "on_track"
	SLAAtRisk   SLAState = "at_risk"
	SLABreached SLAState = "breached"
	SLAResolved SLAState = "met"
	SLAStopped  SLAState = "stopped"
)

// SLAStateOf classifies a ticket's SLA standing. atRiskMinutes is the threshold
// below which a ticket is flagged as approaching its deadline.
func (t *Ticket) SLAStateOf(now time.Time, atRiskMinutes int) SLAState {
	if t.Status.IsTerminal() {
		if t.ResolutionBreached {
			return SLABreached
		}
		return SLAStopped
	}
	if t.ResponseBreached || t.ResolutionBreached {
		return SLABreached
	}
	if t.ResolutionDueAt == nil || t.ResolutionDueAt.Time.IsZero() {
		return SLAStopped
	}
	remaining := t.ResolutionDueAt.Time.Sub(now)
	if remaining <= 0 {
		return SLABreached
	}
	if remaining <= time.Duration(atRiskMinutes)*time.Minute {
		return SLAAtRisk
	}
	return SLAOnTrack
}

// Errors shared by the service layer.
var (
	ErrNotFound      = errors.New("not found")
	ErrForbidden     = errors.New("forbidden")
	ErrConflict      = errors.New("conflict")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrInvalidInput  = errors.New("invalid input")
	ErrInvalidStatus = errors.New("invalid status transition")
)
