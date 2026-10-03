package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// nowStamp returns the current UTC time in the same textual format the schema's
// strftime('%Y-%m-%dT%H:%M:%SZ','now') default produces, so values written by
// Go and by SQL defaults compare and sort identically.
func nowStamp() string {
	return time.Now().UTC().Format(models.TimeLayout)
}

// TicketRepo provides access to the tickets table.
type TicketRepo struct{ db *sql.DB }

// AtRiskMinutes is how close a resolution deadline has to be before a ticket is
// flagged as at risk on read. It matches the SLA engine's default warning window
// so the UI badge and the engine's warning agree.
const AtRiskMinutes = 60

// ticketSelect joins in requester, assignee and policy display names plus the
// comment count, so a single query serves both list and detail responses.
const ticketSelect = `
SELECT
    t.id, t.ticket_number, t.title, t.description, t.category, t.priority, t.status,
    t.requester_id, COALESCE(req.full_name, req.username, 'Unknown') AS requester_name,
    t.assignee_id, asg.display_name,
    t.sla_policy_id, pol.name,
    t.response_due_at, t.resolution_due_at, t.first_response_at, t.resolution_notes,
    t.resolved_at, t.closed_at,
    t.response_breached, t.resolution_breached, t.escalation_level,
    (SELECT COUNT(*) FROM ticket_comments c WHERE c.ticket_id = t.id) AS comment_count,
    t.created_at, t.updated_at
`

// ticketFrom supplies the FROM and join clauses for ticketSelect.
//
// The assignee side is a derived table rather than a direct join because it
// yields a NULL display_name for unassigned tickets via COALESCE, which the
// model expects as a *string. Joining users twice under the same alias would be
// a syntax error, so the derived table carries the alias instead.
const ticketFrom = `
FROM tickets t
JOIN users req            ON req.id = t.requester_id
LEFT JOIN (
    SELECT u.id AS id, COALESCE(NULLIF(u.full_name,''), u.username) AS display_name
    FROM users u
) asg ON asg.id = t.assignee_id
LEFT JOIN sla_policies pol ON pol.id = t.sla_policy_id`

// Create inserts a ticket. Ticket numbers are generated inside the transaction
// that creates the ticket to avoid a read-then-write race between concurrent
// creators.
func (r *TicketRepo) Create(ctx context.Context, in NewTicket) (models.Ticket, error) {
	var id int64
	err := InTx(ctx, r.db, func(tx *sql.Tx) error {
		num, err := r.nextTicketNumber(ctx, tx)
		if err != nil {
			return err
		}
		// Backdating is only for the development seeder; the service layer never
		// sets these, so live tickets always carry a real creation stamp.
		created := nowStamp()
		if in.CreatedAt != nil && !in.CreatedAt.Time.IsZero() {
			created = in.CreatedAt.Time.UTC().Format(models.TimeLayout)
		}
		updated := created
		if in.UpdatedAt != nil && !in.UpdatedAt.Time.IsZero() {
			updated = in.UpdatedAt.Time.UTC().Format(models.TimeLayout)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO tickets (
				ticket_number, title, description, category, priority, status,
				requester_id, assignee_id, sla_policy_id,
				response_due_at, resolution_due_at,
				first_response_at, resolved_at, closed_at,
				response_breached, resolution_breached,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			num, in.Title, in.Description, in.Category, in.Priority, in.Status,
			in.RequesterID, in.AssigneeID, in.SLAPolicyID,
			models.NullTime(in.ResponseDueAt), models.NullTime(in.ResolutionDueAt),
			models.NullTime(in.FirstResponseAt), models.NullTime(in.ResolvedAt),
			models.NullTime(in.ClosedAt),
			boolToInt(in.ResponseBreached), boolToInt(in.ResolutionBreached),
			created, updated)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return nil
	})
	if err != nil {
		return models.Ticket{}, wrapValidation(err, "ticket")
	}
	return r.ByID(ctx, id)
}

// NewTicket is the insert input for Create.
//
// CreatedAt, UpdatedAt and the lifecycle stamps exist so the development seeder
// can produce a realistic backdated history. Production code leaves them nil and
// lets the defaults apply.
type NewTicket struct {
	Title              string
	Description        string
	Category           string
	Priority           models.Priority
	Status             models.TicketStatus
	RequesterID        int64
	AssigneeID         *int64
	SLAPolicyID        *int64
	ResponseDueAt      *models.Time
	ResolutionDueAt    *models.Time
	FirstResponseAt    *models.Time
	ResolvedAt         *models.Time
	ClosedAt           *models.Time
	ResponseBreached   bool
	ResolutionBreached bool
	CreatedAt          *models.Time
	UpdatedAt          *models.Time
}

// nextTicketNumber produces the next sequential human-readable ticket number of
// the form TKT-000001. It must run inside the writer transaction.
func (r *TicketRepo) nextTicketNumber(ctx context.Context, tx *sql.Tx) (string, error) {
	var maxNum sql.NullInt64
	// CAST strips the 'TKT-' prefix; the numeric ordering matches creation order
	// and does not depend on string collation.
	err := tx.QueryRowContext(ctx,
		`SELECT MAX(CAST(substr(ticket_number, 5) AS INTEGER)) FROM tickets`).Scan(&maxNum)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("TKT-%06d", maxNum.Int64+1), nil
}

// ByID loads one ticket with all derived fields populated.
func (r *TicketRepo) ByID(ctx context.Context, id int64) (models.Ticket, error) {
	row := r.db.QueryRowContext(ctx, ticketSelect+ticketFrom+` WHERE t.id = ?`, id)
	t, err := scanTicket(row)
	if err != nil {
		return models.Ticket{}, err
	}
	t.DeriveSLA(time.Now(), AtRiskMinutes)
	return t, nil
}

// ByNumber loads one ticket by its human-readable number.
func (r *TicketRepo) ByNumber(ctx context.Context, number string) (models.Ticket, error) {
	row := r.db.QueryRowContext(ctx, ticketSelect+ticketFrom+` WHERE t.ticket_number = ?`, strings.ToUpper(number))
	t, err := scanTicket(row)
	if err != nil {
		return models.Ticket{}, err
	}
	t.DeriveSLA(time.Now(), AtRiskMinutes)
	return t, nil
}

// TicketFilter captures every supported listing query.
type TicketFilter struct {
	RequesterID  *int64
	AssigneeID   *int64
	Unassigned   bool
	Statuses     []models.TicketStatus
	Priorities   []models.Priority
	Categories   []string
	Search       string
	Breached     *bool
	Escalated    *bool
	OpenOnly     bool
	CreatedFrom  *models.Time
	CreatedTo    *models.Time
	ResolvedFrom *models.Time
	ResolvedTo   *models.Time
	SortBy       string
	SortDir      string
	Page         int
	Limit        int
}

// List returns a filtered, sorted, paginated page of tickets plus the total
// matching count so the UI can render pagination.
func (r *TicketRepo) List(ctx context.Context, f TicketFilter) ([]models.Ticket, int, error) {
	where, args := f.buildWhere()

	var total int
	countQ := `SELECT COUNT(*) FROM tickets t WHERE ` + where
	if err := r.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, wrapValidation(err, "the search expression")
	}

	orderBy := f.orderBy()

	q := ticketSelect + ticketFrom + ` WHERE ` + where + ` ORDER BY ` + orderBy
	if f.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, f.Page*f.Limit)
	}

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, wrapValidation(err, "the search expression")
	}
	defer rows.Close()

	now := time.Now()
	tickets := []models.Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, 0, err
		}
		t.DeriveSLA(now, AtRiskMinutes)
		tickets = append(tickets, t)
	}
	return tickets, total, rows.Err()
}

func (f TicketFilter) buildWhere() (string, []any) {
	where := []string{"1=1"}
	args := []any{}

	if f.RequesterID != nil {
		where = append(where, "t.requester_id = ?")
		args = append(args, *f.RequesterID)
	}
	if f.AssigneeID != nil {
		where = append(where, "t.assignee_id = ?")
		args = append(args, *f.AssigneeID)
	}
	if f.Unassigned {
		where = append(where, "t.assignee_id IS NULL")
	}
	if len(f.Statuses) > 0 {
		where = append(where, inClause("t.status", len(f.Statuses)))
		args = append(args, toAnySlice(f.Statuses)...)
	}
	if len(f.Priorities) > 0 {
		where = append(where, inClause("t.priority", len(f.Priorities)))
		args = append(args, toAnySlice(f.Priorities)...)
	}
	if len(f.Categories) > 0 {
		where = append(where, inClause("t.category", len(f.Categories)))
		args = append(args, anySlice(f.Categories)...)
	}
	if f.Breached != nil {
		col := "t.response_breached"
		if *f.Breached {
			// "breached" means either SLA dimension was missed.
			where = append(where, "(t.response_breached = 1 OR t.resolution_breached = 1)")
		} else {
			where = append(where, col+" = 0 AND t.resolution_breached = 0")
		}
	}
	if f.Escalated != nil {
		where = append(where, "t.escalation_level > 0")
	}
	if f.OpenOnly {
		// 'closed' is the only fully terminal status; 'resolved' still counts as
		// open work because it awaits requester confirmation.
		where = append(where, "t.status != 'closed'")
	}
	if f.CreatedFrom != nil {
		where = append(where, "t.created_at >= ?")
		args = append(args, models.NullTime(f.CreatedFrom))
	}
	if f.CreatedTo != nil {
		where = append(where, "t.created_at <= ?")
		args = append(args, models.NullTime(f.CreatedTo))
	}
	if f.ResolvedFrom != nil {
		where = append(where, "t.resolved_at >= ?")
		args = append(args, models.NullTime(f.ResolvedFrom))
	}
	if f.ResolvedTo != nil {
		where = append(where, "t.resolved_at <= ?")
		args = append(args, models.NullTime(f.ResolvedTo))
	}
	// Free-text search goes through the FTS5 index rather than LIKE, so it
	// matches on word stems ("authenticate" matches "authentication").
	if q := strings.TrimSpace(f.Search); q != "" {
		where = append(where, `t.id IN (SELECT rowid FROM tickets_fts WHERE tickets_fts MATCH ?)`)
		args = append(args, q)
	}

	return strings.Join(where, " AND "), args
}

// sortColumns is the whitelist of sort keys.
//
// User input never reaches the SQL text; it is mapped through this table. It is
// exported so the API layer can advertise the same set at GET /meta and reject
// an unknown key instead of silently falling back to created_at, which would
// leave a user staring at an unsorted table that claims to be sorted.
//
// Three of these are expressions rather than columns: priority is a stored enum
// name, so sorting the text would order critical, high, low, medium
// alphabetically rather than by severity; and the two display names come from
// joined users rows under SELECT aliases.
var sortColumns = map[string]string{
	"created":        "t.created_at",
	"updated":        "t.updated_at",
	"priority":       "CASE t.priority WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END",
	"status":         "t.status",
	"due":            "t.resolution_due_at",
	"number":         "t.ticket_number",
	"comment_count":  "comment_count",
	"requester_name": "requester_name",
	"assignee_name":  "COALESCE(asg.display_name, '')",
}

// SortFields lists the accepted sort keys in a stable order for the UI.
func SortFields() []string {
	keys := make([]string, 0, len(sortColumns))
	for k := range sortColumns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ErrUnknownSortField marks a sort key outside the whitelist.
var ErrUnknownSortField = fmt.Errorf("%w: unknown sort field", models.ErrInvalidInput)

// ValidateSort checks a sort key and direction against the whitelist.
func (f TicketFilter) ValidateSort() error {
	if f.SortBy != "" {
		if _, ok := sortColumns[f.SortBy]; !ok {
			return fmt.Errorf("%w %q (expected one of %s)",
				ErrUnknownSortField, f.SortBy, strings.Join(SortFields(), ", "))
		}
	}
	if f.SortDir != "" && !strings.EqualFold(f.SortDir, "asc") && !strings.EqualFold(f.SortDir, "desc") {
		return fmt.Errorf("%w: sort_dir must be asc or desc, got %q", ErrUnknownSortField, f.SortDir)
	}
	return nil
}

// orderBy renders the ORDER BY clause from the whitelist.
func (f TicketFilter) orderBy() string {
	col, ok := sortColumns[f.SortBy]
	if !ok {
		col = sortColumns["created"]
	}
	dir := "DESC"
	if strings.EqualFold(f.SortDir, "asc") {
		dir = "ASC"
	}
	// Ticket number breaks ties so pagination is stable: without a total order,
	// rows sharing a sort key can repeat or vanish across pages.
	return col + " " + dir + ", t.id " + dir
}

func inClause(col string, n int) string {
	ph := make([]string, n)
	for i := range ph {
		ph[i] = "?"
	}
	return col + " IN (" + strings.Join(ph, ", ") + ")"
}

func toAnySlice[T ~string](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// UpdateTicket is a partial update. A nil pointer means "leave unchanged";
// the Clear* flags set the corresponding column back to NULL.
type UpdateTicket struct {
	Status          *models.TicketStatus
	Priority        *models.Priority
	Category        *string
	AssigneeID      *int64
	SLAPolicyID     *int64
	ResolutionNotes *string
	FirstResponseAt *models.Time
	ResolvedAt      *models.Time
	ClosedAt        *models.Time
	ResponseDueAt   *models.Time
	ResolutionDueAt *models.Time

	ClearAssignee   bool
	ClearResolvedAt bool
	ClearClosedAt   bool
	ClearEscalation bool
}

// Update applies a partial ticket update and returns the refreshed row.
// updated_at is bumped on every mutation.
func (r *TicketRepo) Update(ctx context.Context, id int64, in UpdateTicket) (models.Ticket, error) {
	sets := []string{"updated_at = ?"}
	args := []any{nowStamp()}

	if in.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *in.Status)
	}
	if in.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *in.Priority)
	}
	if in.Category != nil {
		sets = append(sets, "category = ?")
		args = append(args, *in.Category)
	}
	if in.ClearAssignee {
		sets = append(sets, "assignee_id = NULL")
	}
	if in.AssigneeID != nil {
		sets = append(sets, "assignee_id = ?")
		args = append(args, *in.AssigneeID)
	}
	if in.SLAPolicyID != nil {
		sets = append(sets, "sla_policy_id = ?")
		args = append(args, *in.SLAPolicyID)
	}
	if in.ResolutionNotes != nil {
		sets = append(sets, "resolution_notes = ?")
		args = append(args, *in.ResolutionNotes)
	}
	if in.FirstResponseAt != nil {
		sets = append(sets, "first_response_at = ?")
		args = append(args, in.FirstResponseAt)
	}
	if in.ClearResolvedAt {
		sets = append(sets, "resolved_at = NULL")
	}
	if in.ResolvedAt != nil {
		sets = append(sets, "resolved_at = ?")
		args = append(args, in.ResolvedAt)
	}
	if in.ClearClosedAt {
		sets = append(sets, "closed_at = NULL")
	}
	if in.ClosedAt != nil {
		sets = append(sets, "closed_at = ?")
		args = append(args, in.ClosedAt)
	}
	if in.ResponseDueAt != nil {
		sets = append(sets, "response_due_at = ?")
		args = append(args, in.ResponseDueAt)
	}
	if in.ResolutionDueAt != nil {
		sets = append(sets, "resolution_due_at = ?")
		args = append(args, in.ResolutionDueAt)
	}
	if in.ClearEscalation {
		sets = append(sets, "escalation_level = 0", "escalated_at = NULL")
	}

	args = append(args, id)
	res, err := r.db.ExecContext(ctx,
		`UPDATE tickets SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return models.Ticket{}, wrapValidation(err, "ticket")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return models.Ticket{}, models.ErrNotFound
	}
	return r.ByID(ctx, id)
}

// DB exposes the pool so services can run their own focused queries without
// adding a repository method for every ad-hoc aggregation.
func (r *TicketRepo) DB() *sql.DB { return r.db }

// Delete removes a ticket and cascades to comments and events.
func (r *TicketRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM tickets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return models.ErrNotFound
	}
	return nil
}

// CountsByStatus returns the number of tickets per status for the dashboard.
func (r *TicketRepo) CountsByStatus(ctx context.Context, requesterID *int64) (map[string]int, error) {
	q := `SELECT status, COUNT(*) FROM tickets`
	var args []any
	if requesterID != nil {
		q += ` WHERE requester_id = ?`
		args = append(args, *requesterID)
	}
	q += ` GROUP BY status`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for _, s := range models.AllStatuses {
		out[string(s)] = 0
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// ActiveLoad counts non-closed tickets per assignee. This is the primary input
// to the automated routing algorithm.
func (r *TicketRepo) ActiveLoad(ctx context.Context) (map[int64]int, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT assignee_id, COUNT(*)
		FROM tickets
		WHERE assignee_id IS NOT NULL AND status NOT IN ('closed')
		GROUP BY assignee_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	load := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		load[id] = n
	}
	return load, rows.Err()
}

// SearchSuggestions returns distinct categories, useful for filter dropdowns.
func (r *TicketRepo) Categories(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT category FROM tickets ORDER BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ScanCandidates is the set of tickets the SLA worker should evaluate on this
// pass. Only open work with an SLA deadline is returned, and breach counters are
// reset to false so a past breach is re-detected on the next tick.
func (r *TicketRepo) ScanCandidates(ctx context.Context, limit int) ([]models.Ticket, error) {
	rows, err := r.db.QueryContext(ctx, ticketSelect+ticketFrom+`
		WHERE t.status NOT IN ('closed')
		  AND (t.response_due_at IS NOT NULL OR t.resolution_due_at IS NOT NULL)
		ORDER BY
		    CASE WHEN t.resolution_due_at IS NULL THEN 1 ELSE 0 END,
		    t.resolution_due_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkBreached flags a breach for one SLA dimension. It returns false when the
// flag was already set, so the caller only emits a notification once.
func (r *TicketRepo) MarkBreached(ctx context.Context, id int64, dimension string, breached bool) (bool, error) {
	col := "resolution_breached"
	if dimension == "response" {
		col = "response_breached"
	}
	var changed bool
	err := InTx(ctx, r.db, func(tx *sql.Tx) error {
		var current int
		if err := tx.QueryRowContext(ctx,
			`SELECT `+col+` FROM tickets WHERE id = ?`, id).Scan(&current); err != nil {
			return IsNotFound(err)
		}
		// Once a breach is recorded it stays recorded, even if the clock moves.
		if current == 1 || !breached {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tickets SET `+col+` = 1, updated_at = ? WHERE id = ?`,
			nowStamp(), id); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// RaiseEscalation lifts the escalation counter to targetLevel and returns the
// new level, or 0 when the ticket is already at or above it.
//
// It raises rather than increments deliberately: the caller derives targetLevel
// from how long the ticket has been overdue, and a ticket that has not got any
// further past its deadline since the last pass must not climb another level.
func (r *TicketRepo) RaiseEscalation(ctx context.Context, id int64, targetLevel int) (int, error) {
	level := 0
	err := InTx(ctx, r.db, func(tx *sql.Tx) error {
		var current int
		if err := tx.QueryRowContext(ctx,
			`SELECT escalation_level FROM tickets WHERE id = ?`, id).Scan(&current); err != nil {
			return IsNotFound(err)
		}
		if current >= targetLevel {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tickets
			SET escalation_level = ?, escalated_at = ?, updated_at = ?
			WHERE id = ?`, targetLevel, nowStamp(), nowStamp(), id); err != nil {
			return err
		}
		level = targetLevel
		return nil
	})
	return level, err
}

// ResetEscalation clears the escalation counter, used when a ticket is
// legitimately touched again after a breach.
func (r *TicketRepo) ResetEscalation(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE tickets SET escalation_level = 0, escalated_at = NULL, updated_at = ?
		WHERE id = ?`, nowStamp(), id)
	return err
}

// SnippetsFor returns an FTS5-highlighted excerpt per ticket id for the given
// query. It takes the already-filtered result ids so the snippets always
// correspond to the rows the caller is about to return, and runs as a single
// extra query rather than one per ticket.
//
// Column 1 is the description; column 0 is the title. snippet() picks the
// densest match window automatically.
func (r *TicketRepo) SnippetsFor(ctx context.Context, query string, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if query == "" || len(ids) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(ids))
	args := []any{query}
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}

	q := `SELECT f.rowid,
	             COALESCE(NULLIF(snippet(tickets_fts, 1, '<mark>', '</mark>', ' … ', 12), ''),
	                      snippet(tickets_fts, 0, '<mark>', '</mark>', ' … ', 12))
	      FROM tickets_fts f
	      WHERE tickets_fts MATCH ? AND f.rowid IN (` + strings.Join(placeholders, ", ") + `)`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		// A snippet failure must not fail the search itself.
		return out, nil
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id      int64
			snippet sql.NullString
		)
		if err := rows.Scan(&id, &snippet); err != nil {
			return out, err
		}
		if snippet.Valid {
			out[id] = snippet.String
		}
	}
	return out, rows.Err()
}

// Count returns the total ticket count.
func (r *TicketRepo) Count(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tickets`).Scan(&n)
	return n, err
}

func scanTicket(s scanner) (models.Ticket, error) {
	var (
		t                     models.Ticket
		status, priority      string
		assigneeID            sql.NullInt64
		assigneeName          sql.NullString
		policyID              sql.NullInt64
		policyName            sql.NullString
		respBreach, resBreach int
	)
	err := s.Scan(
		&t.ID, &t.TicketNumber, &t.Title, &t.Description, &t.Category, &priority, &status,
		&t.RequesterID, &t.RequesterName,
		&assigneeID, &assigneeName,
		&policyID, &policyName,
		&t.ResponseDueAt, &t.ResolutionDueAt, &t.FirstResponseAt, &t.ResolutionNotes,
		&t.ResolvedAt, &t.ClosedAt,
		&respBreach, &resBreach, &t.EscalationLevel,
		&t.CommentCount, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return models.Ticket{}, IsNotFound(err)
	}
	t.Status = models.TicketStatus(status)
	t.Priority = models.Priority(priority)
	t.ResponseBreached = respBreach == 1
	t.ResolutionBreached = resBreach == 1
	if assigneeID.Valid {
		v := assigneeID.Int64
		t.AssigneeID = &v
	}
	if assigneeName.Valid {
		v := assigneeName.String
		t.AssigneeName = &v
	}
	if policyID.Valid {
		v := policyID.Int64
		t.SLAPolicyID = &v
	}
	if policyName.Valid {
		v := policyName.String
		t.SLAPolicyName = &v
	}
	return t, nil
}

// ParseTimeFilter converts a query-string date (YYYY-MM-DD or RFC3339) into a
// UTC timestamp used by the range filters. A date-only value expands to the end
// of that day so `created_to=2026-01-31` includes the whole day.
func ParseTimeFilter(raw string) (*models.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	// "to:" prefix is an explicit end-of-day marker for date-only values.
	endOfDay := false
	if strings.HasPrefix(raw, "to:") {
		raw = strings.TrimPrefix(raw, "to:")
		endOfDay = true
	}
	for _, layout := range []string{"2006-01-02", models.TimeLayout, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			if layout == "2006-01-02" && endOfDay {
				parsed = parsed.Add(24*time.Hour - time.Second)
			}
			return models.NewTime(parsed.UTC()), nil
		}
	}
	return nil, fmt.Errorf("%w: cannot parse %q as a date (expected YYYY-MM-DD or RFC3339)", models.ErrInvalidInput, raw)
}
