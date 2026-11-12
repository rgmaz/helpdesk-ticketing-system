package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// AIRepo holds the persistent state for the advisory AI layer: the job queue and
// the suggestions it produced. It deliberately has no write path to tickets,
// comments or events; applying a suggestion is the caller's job, through the
// normal service layer.
type AIRepo struct{ db *sql.DB }

// AIJob is one queued or completed analysis run.
type AIJob struct {
	ID               int64
	TicketID         *int64
	Kind             string
	InputHash        string
	PromptVersion    string
	State            string
	Attempts         int
	Model            string
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
}

// NewAIJob is the insert input for EnqueueJob.
type NewAIJob struct {
	TicketID      int64
	Kind          string
	InputHash     string
	PromptVersion string
}

// AICitation is a ticket the model cited when making a suggestion.
type AICitation struct {
	TicketID     int64  `json:"ticket_id"`
	TicketNumber string `json:"ticket_number"`
}

// AISuggestion is one advisory output. AcceptedAt/RejectedAt are nil while the
// suggestion is still undecided.
type AISuggestion struct {
	ID            int64        `json:"id"`
	JobID         int64        `json:"job_id"`
	TicketID      *int64       `json:"ticket_id"`
	Kind          string       `json:"kind"`
	Value         string       `json:"value"`
	Confidence    float64      `json:"confidence"`
	Rationale     string       `json:"rationale"`
	Citations     []AICitation `json:"citations,omitempty"`
	Model         string       `json:"model"`
	PromptVersion string       `json:"prompt_version"`
	AcceptedAt    *models.Time `json:"accepted_at"`
	RejectedAt    *models.Time `json:"rejected_at"`
	CreatedAt     *models.Time `json:"created_at"`
}

// NewAISuggestion is the insert input for AddSuggestions.
type NewAISuggestion struct {
	TicketID      int64
	Kind          string
	Value         string
	Confidence    float64
	Rationale     string
	Citations     []AICitation
	Model         string
	PromptVersion string
}

// AICandidate is a ticket returned by the FTS5 candidate search before the model
// reranks it.
type AICandidate struct {
	TicketID     int64
	TicketNumber string
	Title        string
	Status       string
	Snippet      string
}

// AIJobMetrics is the usage recorded when a job completes.
type AIJobMetrics struct {
	Model            string
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
}

// AIStats summarises queue health and acceptance, for the admin health endpoint.
type AIStats struct {
	Queued           int     `json:"jobs_queued"`
	Running          int     `json:"jobs_running"`
	Succeeded        int     `json:"jobs_succeeded"`
	Failed           int     `json:"jobs_failed"`
	Suggestions      int     `json:"suggestions_total"`
	Accepted         int     `json:"suggestions_accepted"`
	Rejected         int     `json:"suggestions_rejected"`
	AvgLatencyMS     float64 `json:"avg_latency_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	SpentUSD         float64 `json:"spent_usd"`
}

// EnqueueJob inserts an analysis request, or revives an existing one if the same
// ticket and input were already analysed under the same prompt version. The
// unique constraint makes a re-analysis of unchanged content free.
func (r *AIRepo) EnqueueJob(ctx context.Context, in NewAIJob) (int64, error) {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO ai_jobs (ticket_id, kind, input_hash, prompt_version, state)
		VALUES (?, ?, ?, ?, 'queued')
		ON CONFLICT(ticket_id, kind, input_hash, prompt_version) DO UPDATE SET
			state = CASE WHEN ai_jobs.state = 'failed' THEN 'queued' ELSE ai_jobs.state END,
			updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`,
		in.TicketID, in.Kind, in.InputHash, in.PromptVersion)
	if err != nil {
		return 0, err
	}

	var id int64
	err = r.db.QueryRowContext(ctx, `
		SELECT id FROM ai_jobs
		WHERE ticket_id = ? AND kind = ? AND input_hash = ? AND prompt_version = ?`,
		in.TicketID, in.Kind, in.InputHash, in.PromptVersion).Scan(&id)
	return id, err
}

// ClaimJob atomically leases the next queued job. A running job whose lease has
// expired is reclaimed, which is how a crashed worker's work is recovered.
func (r *AIRepo) ClaimJob(ctx context.Context, lease time.Duration) (AIJob, bool, error) {
	now := nowStamp()
	var (
		job   AIJob
		found bool
	)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AIJob{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var ticketID sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT id, ticket_id, kind, input_hash, prompt_version, state, attempts,
		       COALESCE(model,''), COALESCE(latency_ms,0),
		       COALESCE(prompt_tokens,0), COALESCE(completion_tokens,0), COALESCE(cost_usd,0)
		FROM ai_jobs
		WHERE state = 'queued'
		   OR (state = 'running' AND lease_until IS NOT NULL AND lease_until < ?)
		ORDER BY id
		LIMIT 1`, now).Scan(
		&job.ID, &ticketID, &job.Kind, &job.InputHash, &job.PromptVersion, &job.State,
		&job.Attempts, &job.Model, &job.LatencyMS,
		&job.PromptTokens, &job.CompletionTokens, &job.CostUSD)
	if errors.Is(err, sql.ErrNoRows) {
		return AIJob{}, false, nil
	}
	if err != nil {
		return AIJob{}, false, err
	}
	if ticketID.Valid {
		job.TicketID = &ticketID.Int64
	}

	leaseUntil := time.Now().UTC().Add(lease).Format(models.TimeLayout)
	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_jobs
		SET state = 'running', attempts = attempts + 1, lease_until = ?, updated_at = ?
		WHERE id = ?`, leaseUntil, now, job.ID); err != nil {
		return AIJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return AIJob{}, false, err
	}

	job.State = "running"
	job.Attempts++
	found = true
	return job, found, nil
}

// FinishJob records a successful run with its usage metrics.
func (r *AIRepo) FinishJob(ctx context.Context, id int64, m AIJobMetrics) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE ai_jobs
		SET state = 'succeeded', lease_until = NULL, last_error = NULL,
		    model = ?, latency_ms = ?, prompt_tokens = ?, completion_tokens = ?,
		    cost_usd = ?, updated_at = ?
		WHERE id = ?`,
		m.Model, m.LatencyMS, m.PromptTokens, m.CompletionTokens, m.CostUSD, nowStamp(), id)
	return err
}

// FailJob requeues a job until it has exhausted its attempts, then parks it as
// failed. The last error is kept for the admin health view.
func (r *AIRepo) FailJob(ctx context.Context, id int64, msg string, maxAttempts int) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE ai_jobs
		SET state = CASE WHEN attempts >= ? THEN 'failed' ELSE 'queued' END,
		    lease_until = NULL, last_error = ?, updated_at = ?
		WHERE id = ?`, maxAttempts, msg, nowStamp(), id)
	return err
}

// AddSuggestions writes every suggestion produced by one job in a single
// transaction, so a partially-written analysis is never visible.
func (r *AIRepo) AddSuggestions(ctx context.Context, jobID int64, list []NewAISuggestion) error {
	if len(list) == 0 {
		return nil
	}
	return InTx(ctx, r.db, func(tx *sql.Tx) error {
		for _, s := range list {
			citations := "[]"
			if len(s.Citations) > 0 {
				raw, err := json.Marshal(s.Citations)
				if err != nil {
					return err
				}
				citations = string(raw)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ai_suggestions
					(job_id, ticket_id, kind, value, confidence, rationale, citations, model, prompt_version)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				jobID, s.TicketID, s.Kind, s.Value, s.Confidence, s.Rationale,
				citations, s.Model, s.PromptVersion); err != nil {
				return err
			}
		}
		return nil
	})
}

// latestSuggestionsSQL picks the newest suggestion per kind for a ticket.
const latestSuggestionsSQL = `
	SELECT id, job_id, ticket_id, kind, value, confidence, rationale, citations,
	       model, prompt_version, accepted_at, rejected_at, created_at
	FROM (
		SELECT s.*, ROW_NUMBER() OVER (PARTITION BY kind ORDER BY id DESC) AS rn
		FROM ai_suggestions s
		WHERE s.ticket_id = ?
	)
	WHERE rn = 1
	ORDER BY kind`

// ListSuggestions returns the latest suggestion of each kind for a ticket.
func (r *AIRepo) ListSuggestions(ctx context.Context, ticketID int64) ([]AISuggestion, error) {
	rows, err := r.db.QueryContext(ctx, latestSuggestionsSQL, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AISuggestion{}
	for rows.Next() {
		s, err := scanAISuggestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SuggestionByID loads one suggestion, scoped to the ticket it belongs to so a
// caller cannot accept a suggestion against the wrong ticket by editing the URL.
func (r *AIRepo) SuggestionByID(ctx context.Context, ticketID, id int64) (AISuggestion, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, job_id, ticket_id, kind, value, confidence, rationale, citations,
		       model, prompt_version, accepted_at, rejected_at, created_at
		FROM ai_suggestions
		WHERE id = ? AND ticket_id = ?`, id, ticketID)
	return scanAISuggestion(row)
}

// MarkSuggestion records a human decision. An already-decided suggestion is a
// conflict rather than a silent overwrite, which keeps the acceptance metric
// honest.
func (r *AIRepo) MarkSuggestion(ctx context.Context, id, userID int64, accepted bool) error {
	col, by := "accepted_at", "accepted_by"
	if !accepted {
		col, by = "rejected_at", "rejected_by"
	}
	res, err := r.db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE ai_suggestions
		SET %s = ?, %s = ?
		WHERE id = ? AND accepted_at IS NULL AND rejected_at IS NULL`, col, by),
		nowStamp(), userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: this suggestion has already been decided", models.ErrConflict)
	}
	return nil
}

// Candidates finds similar tickets through the existing FTS5 index. Scoping is
// applied in SQL: a requester's preflight sees only their own tickets, so the
// duplicate check cannot leak the existence of somebody else's issue.
func (r *AIRepo) Candidates(ctx context.Context, match string, requesterID *int64, limit int) ([]AICandidate, error) {
	match = strings.TrimSpace(match)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	q := `
		SELECT t.id, t.ticket_number, t.title, t.status,
		       COALESCE(NULLIF(snippet(tickets_fts, 1, '', '', ' … ', 16), ''),
		                NULLIF(snippet(tickets_fts, 0, '', '', ' … ', 16), ''),
		                '') AS excerpt
		FROM tickets_fts f
		JOIN tickets t ON t.id = f.rowid
		WHERE tickets_fts MATCH ?`
	args := []any{match}
	if requesterID != nil {
		q += ` AND t.requester_id = ?`
		args = append(args, *requesterID)
	}
	q += ` ORDER BY bm25(tickets_fts) LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		// A malformed MATCH is a candidate-generation miss, not a fatal error:
		// the preflight degrades to "no duplicates shown".
		if isFTS5SyntaxError(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()

	out := []AICandidate{}
	for rows.Next() {
		var c AICandidate
		if err := rows.Scan(&c.TicketID, &c.TicketNumber, &c.Title, &c.Status, &c.Snippet); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Stats aggregates queue and acceptance counters for the admin endpoint.
func (r *AIRepo) Stats(ctx context.Context) (AIStats, error) {
	var s AIStats
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN state = 'queued' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'running' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'succeeded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM ai_jobs`).Scan(
		&s.Queued, &s.Running, &s.Succeeded, &s.Failed,
		&s.PromptTokens, &s.CompletionTokens, &s.SpentUSD)
	if err != nil {
		return s, err
	}

	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN accepted_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN rejected_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM ai_suggestions`).Scan(&s.Suggestions, &s.Accepted, &s.Rejected); err != nil {
		return s, err
	}

	if err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(AVG(latency_ms), 0) FROM ai_jobs WHERE state = 'succeeded'`).
		Scan(&s.AvgLatencyMS); err != nil {
		return s, err
	}
	return s, nil
}

type aiScanner interface {
	Scan(...any) error
}

func scanAISuggestion(s aiScanner) (AISuggestion, error) {
	var (
		out       AISuggestion
		ticketID  sql.NullInt64
		citations sql.NullString
		accepted  models.Time
		rejected  models.Time
		created   models.Time
	)
	var acceptedAt, rejectedAt sql.NullString

	err := s.Scan(
		&out.ID, &out.JobID, &ticketID, &out.Kind, &out.Value, &out.Confidence,
		&out.Rationale, &citations, &out.Model, &out.PromptVersion,
		&acceptedAt, &rejectedAt, &created)
	if err != nil {
		return AISuggestion{}, IsNotFound(err)
	}
	if ticketID.Valid {
		out.TicketID = &ticketID.Int64
	}
	if citations.Valid && citations.String != "" {
		_ = json.Unmarshal([]byte(citations.String), &out.Citations)
	}
	if acceptedAt.Valid {
		_ = accepted.Scan(acceptedAt.String)
		out.AcceptedAt = &accepted
	}
	if rejectedAt.Valid {
		_ = rejected.Scan(rejectedAt.String)
		out.RejectedAt = &rejected
	}
	out.CreatedAt = &created
	return out, nil
}
