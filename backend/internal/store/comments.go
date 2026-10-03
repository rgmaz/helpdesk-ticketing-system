package store

import (
	"context"
	"database/sql"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// CommentRepo provides access to ticket comment threads.
type CommentRepo struct{ db *sql.DB }

// Add appends a comment to a ticket.
func (r *CommentRepo) Add(ctx context.Context, ticketID, authorID int64, body string, internal bool) (models.Comment, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO ticket_comments (ticket_id, author_id, body, is_internal, created_at)
		VALUES (?, ?, ?, ?, ?)`, ticketID, authorID, body, boolToInt(internal), nowStamp())
	if err != nil {
		return models.Comment{}, wrapValidation(err, "comment")
	}
	id, _ := res.LastInsertId()
	return r.ByID(ctx, id)
}

// ByID loads one comment.
func (r *CommentRepo) ByID(ctx context.Context, id int64) (models.Comment, error) {
	row := r.db.QueryRowContext(ctx, commentSelect+` WHERE c.id = ?`, id)
	return scanComment(row)
}

const commentSelect = `
SELECT c.id, c.ticket_id, c.author_id,
       COALESCE(NULLIF(u.full_name,''), u.username) AS author_name,
       r.name AS author_role, c.body, c.is_internal, c.created_at
FROM ticket_comments c
JOIN users u      ON u.id = c.author_id
JOIN roles r      ON r.id = u.role_id`

// List returns the comment thread for a ticket in chronological order.
// includeInternal must be false for end users, who never see staff-only notes.
func (r *CommentRepo) List(ctx context.Context, ticketID int64, includeInternal bool) ([]models.Comment, error) {
	q := commentSelect + ` WHERE c.ticket_id = ?`
	args := []any{ticketID}
	if !includeInternal {
		q += ` AND c.is_internal = 0`
	}
	q += ` ORDER BY c.created_at ASC, c.id ASC`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.Comment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Count returns the number of comments on a ticket, used to paginate a long
// thread without loading it entirely.
func (r *CommentRepo) Count(ctx context.Context, ticketID int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticket_comments WHERE ticket_id = ?`, ticketID).Scan(&n)
	return n, err
}

// Latest returns the most recent public comment, used for "last update"
// summaries in list views.
func (r *CommentRepo) Latest(ctx context.Context, ticketID int64) (models.Comment, error) {
	row := r.db.QueryRowContext(ctx, commentSelect+
		` WHERE c.ticket_id = ? ORDER BY c.created_at DESC, c.id DESC LIMIT 1`, ticketID)
	return scanComment(row)
}

// CountInternal reports how many staff-only notes exist, letting the UI show a
// badge without exposing their content.
func (r *CommentRepo) CountInternal(ctx context.Context, ticketID int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ticket_comments WHERE ticket_id = ? AND is_internal = 1`, ticketID).Scan(&n)
	return n, err
}

func scanComment(s scanner) (models.Comment, error) {
	var (
		c        models.Comment
		internal int
	)
	err := s.Scan(&c.ID, &c.TicketID, &c.AuthorID, &c.AuthorName, &c.AuthorRole,
		&c.Body, &internal, &c.CreatedAt)
	if err != nil {
		return models.Comment{}, IsNotFound(err)
	}
	c.Internal = internal == 1
	return c, nil
}

// EventRepo records the immutable ticket timeline.
type EventRepo struct{ db *sql.DB }

// Add appends a timeline event.
func (r *EventRepo) Add(ctx context.Context, ev NewEvent) error {
	created := nowStamp()
	// Only the development seeder backdates events, so a live system always
	// records when the action actually happened.
	if ev.CreatedAt != nil && !ev.CreatedAt.Time.IsZero() {
		created = ev.CreatedAt.Time.UTC().Format(models.TimeLayout)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO ticket_events (ticket_id, actor_id, event_type, from_value, to_value, message, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.TicketID, ev.ActorID, ev.Type, ev.FromValue, ev.ToValue, ev.Message, created)
	return err
}

// NewEvent is the input for Add. A nil actor means the system acted (for
// example, the SLA engine), which is rendered differently in the timeline.
type NewEvent struct {
	TicketID  int64
	ActorID   *int64
	Type      string
	FromValue *string
	ToValue   *string
	Message   string
	CreatedAt *models.Time
}

// List returns a ticket's timeline, newest first.
func (r *EventRepo) List(ctx context.Context, ticketID int64, limit int) ([]models.TicketEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.ticket_id, COALESCE(e.actor_id, 0),
		       COALESCE(NULLIF(u.full_name,''), u.username, 'System'),
		       e.event_type, e.from_value, e.to_value, e.message, e.created_at
		FROM ticket_events e
		LEFT JOIN users u ON u.id = e.actor_id
		WHERE e.ticket_id = ?
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT ?`, ticketID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.TicketEvent{}
	for rows.Next() {
		var e models.TicketEvent
		if err := rows.Scan(&e.ID, &e.TicketID, &e.ActorID, &e.ActorName,
			&e.Type, &e.FromValue, &e.ToValue, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListRecent returns the newest events across all tickets for the activity feed.
func (r *EventRepo) ListRecent(ctx context.Context, limit int) ([]models.TicketEvent, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.ticket_id, COALESCE(e.actor_id, 0),
		       COALESCE(NULLIF(u.full_name,''), u.username, 'System'),
		       e.event_type, e.from_value, e.to_value, e.message, e.created_at
		FROM ticket_events e
		LEFT JOIN users u ON u.id = e.actor_id
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.TicketEvent{}
	for rows.Next() {
		var e models.TicketEvent
		if err := rows.Scan(&e.ID, &e.TicketID, &e.ActorID, &e.ActorName,
			&e.Type, &e.FromValue, &e.ToValue, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DistinctTypes lists every event type present, used to validate inputs.
func (r *EventRepo) DistinctTypes(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT event_type FROM ticket_events ORDER BY event_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LatestForTicket returns the newest event per ticket in one pass, avoiding an
// N+1 query when rendering a list.
func (r *EventRepo) LatestForTicket(ctx context.Context, ticketID int64) (models.TicketEvent, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT e.id, e.ticket_id, COALESCE(e.actor_id, 0),
		       COALESCE(NULLIF(u.full_name,''), u.username, 'System'),
		       e.event_type, e.from_value, e.to_value, e.message, e.created_at
		FROM ticket_events e
		LEFT JOIN users u ON u.id = e.actor_id
		WHERE e.ticket_id = ?
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT 1`, ticketID)
	var e models.TicketEvent
	if err := row.Scan(&e.ID, &e.TicketID, &e.ActorID, &e.ActorName,
		&e.Type, &e.FromValue, &e.ToValue, &e.Message, &e.CreatedAt); err != nil {
		return models.TicketEvent{}, IsNotFound(err)
	}
	return e, nil
}
