package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// SLAEventType enumerates the automated notifications the SLA engine emits.
type SLAEventType string

const (
	EventResponseWarning   SLAEventType = "sla_response_warning"
	EventResponseBreach    SLAEventType = "sla_response_breach"
	EventResolutionWarning SLAEventType = "sla_resolution_warning"
	EventResolutionBreach  SLAEventType = "sla_resolution_breach"
	EventEscalation        SLAEventType = "sla_escalation"
	EventAutoAssigned      SLAEventType = "auto_assigned"
	EventStatusChange      SLAEventType = "status_changed"
	EventPriorityChanged   SLAEventType = "priority_changed"
	EventAssigned          SLAEventType = "assigned"
	EventCommentAdded      SLAEventType = "comment_added"
	EventTicketCreated     SLAEventType = "ticket_created"
	EventTicketResolved    SLAEventType = "ticket_resolved"
	EventTicketClosed      SLAEventType = "ticket_closed"
	EventTicketReopened    SLAEventType = "ticket_reopened"
	EventTicketUpdated     SLAEventType = "ticket_updated"
)

// SLARepo reads SLA policies and records SLA-driven events/notifications.
type SLARepo struct{ db *sql.DB }

// ForPriority returns the active policy governing a priority band.
func (r *SLARepo) ForPriority(ctx context.Context, p models.Priority) (models.SLAPolicy, error) {
	row := r.db.QueryRowContext(ctx, policySelect+` WHERE priority = ? AND active = 1`, p)
	return scanPolicy(row)
}

// ByID returns one policy.
func (r *SLARepo) ByID(ctx context.Context, id int64) (models.SLAPolicy, error) {
	row := r.db.QueryRowContext(ctx, policySelect+` WHERE id = ?`, id)
	return scanPolicy(row)
}

// All returns every policy ordered by descending urgency.
func (r *SLARepo) All(ctx context.Context, activeOnly bool) ([]models.SLAPolicy, error) {
	q := policySelect
	if activeOnly {
		q += ` WHERE active = 1`
	}
	q += ` ORDER BY CASE priority WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END DESC`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.SLAPolicy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const policySelect = `
SELECT id, name, priority, response_minutes, resolution_minutes,
       escalation_minutes, auto_assign, active, description, created_at
FROM sla_policies`

func scanPolicy(s scanner) (models.SLAPolicy, error) {
	var (
		p       models.SLAPolicy
		autoAsg int
		active  int
	)
	if err := s.Scan(&p.ID, &p.Name, &p.Priority, &p.ResponseMinutes, &p.ResolutionMinutes,
		&p.EscalationMinutes, &autoAsg, &active, &p.Description, &p.CreatedAt); err != nil {
		return models.SLAPolicy{}, IsNotFound(err)
	}
	p.AutoAssign = autoAsg == 1
	p.Active = active == 1
	return p, nil
}

// Update modifies policy targets.
func (r *SLARepo) Update(ctx context.Context, id int64, in UpdatePolicy) error {
	q := `UPDATE sla_policies SET `
	args := []any{}
	if in.ResponseMinutes != nil {
		q += `response_minutes = ?, `
		args = append(args, *in.ResponseMinutes)
	}
	if in.ResolutionMinutes != nil {
		q += `resolution_minutes = ?, `
		args = append(args, *in.ResolutionMinutes)
	}
	if in.EscalationMinutes != nil {
		q += `escalation_minutes = ?, `
		args = append(args, *in.EscalationMinutes)
	}
	if in.AutoAssign != nil {
		q += `auto_assign = ?, `
		args = append(args, boolToInt(*in.AutoAssign))
	}
	if in.Active != nil {
		q += `active = ?, `
		args = append(args, boolToInt(*in.Active))
	}
	if in.Description != nil {
		q += `description = ?, `
		args = append(args, *in.Description)
	}
	if in.Name != nil {
		q += `name = ?, `
		args = append(args, *in.Name)
	}
	q += `updated_at = ? WHERE id = ?`
	args = append(args, nowStamp(), id)

	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return models.ErrNotFound
	}
	return nil
}

// UpdatePolicy is a partial policy update; nil fields are unchanged.
type UpdatePolicy struct {
	Name              *string
	Description       *string
	ResponseMinutes   *int
	ResolutionMinutes *int
	EscalationMinutes *int
	AutoAssign        *bool
	Active            *bool
}

// AuditRepo writes the immutable audit trail.
type AuditRepo struct{ db *sql.DB }

// Log appends an audit record. Audit failures are surfaced to the caller but
// should not abort the business operation that already succeeded.
func (r *AuditRepo) Log(ctx context.Context, entry AuditEntry) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_logs (actor_id, ticket_id, action, from_value, to_value, details, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.ActorID, entry.TicketID, entry.Action, entry.FromValue, entry.ToValue,
		entry.Details, nowStamp())
	if err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	return nil
}

// AuditEntry is one audit row.
type AuditEntry struct {
	ActorID   *int64
	TicketID  *int64
	Action    string
	FromValue *string
	ToValue   *string
	Details   *string
}

// List returns audit records newest-first, optionally scoped to one ticket.
func (r *AuditRepo) List(ctx context.Context, ticketID *int64, limit, offset int) ([]models.AuditLog, int, error) {
	where := "1=1"
	args := []any{}
	if ticketID != nil {
		where = `a.ticket_id = ?`
		args = append(args, *ticketID)
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_logs a WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, a.actor_id, COALESCE(NULLIF(u.full_name,''), u.username),
		       a.ticket_id, a.action, a.from_value, a.to_value, a.details, a.created_at
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.actor_id
		WHERE `+where+`
		ORDER BY a.id DESC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []models.AuditLog{}
	for rows.Next() {
		var a models.AuditLog
		if err := rows.Scan(&a.ID, &a.ActorID, &a.ActorName, &a.TicketID,
			&a.Action, &a.FromValue, &a.ToValue, &a.Details, &a.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// NotifyRepo persists per-user notifications.
type NotifyRepo struct{ db *sql.DB }

// Create inserts a notification, optionally scoped to a ticket.
func (r *NotifyRepo) Create(ctx context.Context, userID int64, ticketID *int64, kind, title, body, level string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO notifications (user_id, ticket_id, kind, title, body, level, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, ticketID, kind, title, body, level, nowStamp())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// CreateMany inserts one notification per user in a single transaction. It is
// used for broadcasts such as "a critical ticket breached its SLA".
func (r *NotifyRepo) CreateMany(ctx context.Context, userIDs []int64, ticketID *int64, kind, title, body, level string) error {
	if len(userIDs) == 0 {
		return nil
	}
	return InTx(ctx, r.db, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO notifications (user_id, ticket_id, kind, title, body, level, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, uid := range userIDs {
			if _, err := stmt.ExecContext(ctx, uid, ticketID, kind, title, body, level, nowStamp()); err != nil {
				return err
			}
		}
		return nil
	})
}

// List returns a page of a user's notifications newest-first, plus the total
// number matching the filter. The unread count is a separate query (UnreadCount)
// because the navbar badge needs it regardless of the current filter.
func (r *NotifyRepo) List(ctx context.Context, userID int64, unreadOnly bool, limit, offset int) ([]models.Notification, int, error) {
	where := `user_id = ?`
	args := []any{userID}
	if unreadOnly {
		where += ` AND read_at IS NULL`
	}

	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	if limit <= 0 {
		limit = 25
	}
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, ticket_id, kind, title, body, level, read_at, created_at
		FROM notifications
		WHERE `+where+`
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []models.Notification{}
	for rows.Next() {
		var n models.Notification
		if err := rows.Scan(&n.ID, &n.UserID, &n.TicketID, &n.Kind, &n.Title,
			&n.Body, &n.Level, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	return out, total, rows.Err()
}

// MarkRead marks one notification as read, scoped to its owner.
func (r *NotifyRepo) MarkRead(ctx context.Context, userID, id int64) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE notifications SET read_at = ?
		WHERE id = ? AND user_id = ? AND read_at IS NULL`,
		nowStamp(), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already read or not owned; distinguish by checking existence.
		var exists int
		if err := r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM notifications WHERE id = ? AND user_id = ?`, id, userID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return models.ErrNotFound
		}
	}
	return nil
}

// MarkAllRead clears the unread badge for a user and reports how many were updated.
func (r *NotifyRepo) MarkAllRead(ctx context.Context, userID int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL`,
		nowStamp(), userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UnreadCount returns the number of unread notifications for a user.
func (r *NotifyRepo) UnreadCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL`,
		userID).Scan(&n)
	return n, err
}

// DeleteOld removes notifications older than the cutoff, keeping the table small.
func (r *NotifyRepo) DeleteOld(ctx context.Context, cutoff string) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM notifications WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
