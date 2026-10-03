package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// Store bundles all repositories over a single connection pool.
type Store struct {
	db        *sql.DB
	Users     *UserRepo
	Tickets   *TicketRepo
	Comments  *CommentRepo
	Events    *EventRepo
	SLA       *SLARepo
	Audit     *AuditRepo
	Notify    *NotifyRepo
	Analytics *AnalyticsRepo
}

// New wires the repositories.
func New(db *sql.DB) *Store {
	return &Store{
		db:        db,
		Users:     &UserRepo{db: db},
		Tickets:   &TicketRepo{db: db},
		Comments:  &CommentRepo{db: db},
		Events:    &EventRepo{db: db},
		SLA:       &SLARepo{db: db},
		Audit:     &AuditRepo{db: db},
		Notify:    &NotifyRepo{db: db},
		Analytics: &AnalyticsRepo{db: db},
	}
}

// DB exposes the underlying pool for transactions and health checks.
func (s *Store) DB() *sql.DB { return s.db }

// querier is satisfied by both *sql.DB and *sql.Tx, letting repositories run
// either standalone or inside a caller's transaction.
type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var _ querier = (*sql.DB)(nil)
var _ querier = (*sql.Tx)(nil)

// InTx runs fn inside a transaction, rolling back on error or panic. It is a
// free function because repositories hold *sql.DB, not the *db.DB wrapper.
func InTx(ctx context.Context, database *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure,
// identified by its stable error codes.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: unique")
}

// IsNotFound normalises sql.ErrNoRows into the domain sentinel.
func IsNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return models.ErrNotFound
	}
	return err
}

// isFTS5SyntaxError reports whether err came from FTS5 rejecting a MATCH
// expression.
//
// The caller passes free text straight into MATCH, so a stray operator such as
// "a OR" is a routine client input rather than a fault. Without this mapping it
// would surface as an unhandled error and a 500.
func isFTS5SyntaxError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "fts5") ||
		strings.Contains(msg, "malformed match") ||
		strings.Contains(msg, "syntax error")
}

// wrapValidation turns a SQLite constraint failure into a domain error so the
// HTTP layer reports the right status instead of a 500.
//
// Uniqueness violations map to ErrConflict: the request was well-formed but
// clashes with an existing row, which is a different client fix from malformed
// input (ErrInvalidInput). Other constraint failures are validation failures.
func wrapValidation(err error, what string) error {
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %s already exists", models.ErrConflict, what)
	}
	if isFTS5SyntaxError(err) {
		return fmt.Errorf("%w: the search expression could not be parsed", models.ErrInvalidInput)
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "check constraint failed") {
		return fmt.Errorf("%w: %s failed a database constraint", models.ErrInvalidInput, what)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return models.ErrNotFound
	}
	return err
}
