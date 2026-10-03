package db

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is one ordered, idempotent schema step.
type migration struct {
	Version int
	Name    string
	SQL     string
}

// migrations is the append-only schema history. Never edit an applied entry:
// add a new one instead so existing databases upgrade in place.
var migrations = []migration{
	{
		Version: 1,
		Name:    "initial_schema",
		SQL: `
-- Roles are seeded in migration 2 but declared here for referential integrity.
CREATE TABLE IF NOT EXISTS roles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    full_name     TEXT NOT NULL DEFAULT '',
    role_id       INTEGER NOT NULL REFERENCES roles(id),
    active        INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_users_role ON users(role_id);

CREATE TABLE IF NOT EXISTS sla_policies (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    priority            TEXT NOT NULL UNIQUE,
    response_minutes    INTEGER NOT NULL,
    resolution_minutes  INTEGER NOT NULL,
    escalation_minutes  INTEGER NOT NULL DEFAULT 0,
    auto_assign         INTEGER NOT NULL DEFAULT 1,
    active              INTEGER NOT NULL DEFAULT 1,
    description         TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS tickets (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    ticket_number        TEXT NOT NULL UNIQUE,
    title                TEXT NOT NULL,
    description          TEXT NOT NULL DEFAULT '',
    category             TEXT NOT NULL DEFAULT 'general',
    priority             TEXT NOT NULL DEFAULT 'medium',
    status               TEXT NOT NULL DEFAULT 'open',
    requester_id         INTEGER NOT NULL REFERENCES users(id),
    assignee_id          INTEGER REFERENCES users(id),
    sla_policy_id        INTEGER REFERENCES sla_policies(id),
    response_due_at      TEXT,
    resolution_due_at    TEXT,
    first_response_at    TEXT,
    resolution_notes     TEXT NOT NULL DEFAULT '',
    resolved_at          TEXT,
    closed_at            TEXT,
    response_breached    INTEGER NOT NULL DEFAULT 0,
    resolution_breached  INTEGER NOT NULL DEFAULT 0,
    escalation_level     INTEGER NOT NULL DEFAULT 0,
    escalated_at         TEXT,
    created_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    CHECK (status IN ('open','in_progress','pending','resolved','closed','reopened')),
    CHECK (priority IN ('low','medium','high','critical'))
);
CREATE INDEX IF NOT EXISTS idx_tickets_status       ON tickets(status);
CREATE INDEX IF NOT EXISTS idx_tickets_priority     ON tickets(priority);
CREATE INDEX IF NOT EXISTS idx_tickets_assignee     ON tickets(assignee_id);
CREATE INDEX IF NOT EXISTS idx_tickets_requester    ON tickets(requester_id);
CREATE INDEX IF NOT EXISTS idx_tickets_created      ON tickets(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tickets_resolved     ON tickets(resolved_at);
-- Partial index drives the once-a-minute SLA scan without touching closed work.
CREATE INDEX IF NOT EXISTS idx_tickets_open_sla
    ON tickets(resolution_due_at)
    WHERE status NOT IN ('closed');

CREATE TABLE IF NOT EXISTS ticket_comments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ticket_id   INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    author_id   INTEGER NOT NULL REFERENCES users(id),
    body        TEXT NOT NULL,
    is_internal INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_comments_ticket ON ticket_comments(ticket_id, created_at);

CREATE TABLE IF NOT EXISTS ticket_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ticket_id   INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    actor_id    INTEGER REFERENCES users(id),
    event_type  TEXT NOT NULL,
    from_value  TEXT,
    to_value    TEXT,
    message     TEXT NOT NULL DEFAULT '',
    metadata    TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_events_ticket ON ticket_events(ticket_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_type   ON ticket_events(event_type, created_at DESC);

CREATE TABLE IF NOT EXISTS notifications (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ticket_id   INTEGER REFERENCES tickets(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL DEFAULT '',
    level       TEXT NOT NULL DEFAULT 'info',
    read_at     TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notifications_unread ON notifications(user_id) WHERE read_at IS NULL;
`,
	},
	{
		Version: 2,
		Name:    "seed_reference_data",
		SQL: `
INSERT OR IGNORE INTO roles (name, description) VALUES
    ('user',       'End user submitting and tracking support requests'),
    ('technician', 'Service desk agent resolving assigned tickets'),
    ('admin',      'Administrator with full system access and analytics');

-- One policy per priority band. Minutes are the storage unit; the service
-- layer converts to durations. Resolution targets are the classic 1/4/8/24h
-- support-desk matrix.
INSERT OR IGNORE INTO sla_policies
    (name, priority, response_minutes, resolution_minutes, escalation_minutes, auto_assign, description) VALUES
    ('Critical Response', 'critical', 15,   240,  30, 1, 'Production outage or severe security issue: 15m response, 4h resolution'),
    ('High Response',     'high',     60,   480,  60, 1, 'Major degradation with no workaround: 1h response, 8h resolution'),
    ('Medium Response',   'medium',   480,  2880, 120, 1, 'Standard request with a workaround available: 8h response, 48h resolution'),
    ('Low Response',      'low',      1440, 4320, 0,  1, 'Informational or scheduled request: 24h response, 72h resolution');
`,
	},
	{
		Version: 3,
		Name:    "fts5_search_index",
		SQL: `
-- External-content FTS5 table: SQLite stores only the inverted index and
-- reads column values back from the tickets table, so text is not duplicated.
CREATE VIRTUAL TABLE IF NOT EXISTS tickets_fts USING fts5(
    title,
    description,
    resolution_notes,
    category,
    ticket_number,
    content='tickets',
    content_rowid='id',
    tokenize = "porter unicode61 remove_diacritics 2"
);

-- Triggers keep the index synchronised with the base table for INSERT,
-- UPDATE and DELETE, including the special 'delete' command that external
-- content tables require on update/delete.
CREATE TRIGGER IF NOT EXISTS tickets_fts_ai AFTER INSERT ON tickets BEGIN
    INSERT INTO tickets_fts(rowid, title, description, resolution_notes, category, ticket_number)
    VALUES (new.id, new.title, new.description, new.resolution_notes, new.category, new.ticket_number);
END;

CREATE TRIGGER IF NOT EXISTS tickets_fts_ad AFTER DELETE ON tickets BEGIN
    INSERT INTO tickets_fts(tickets_fts, rowid, title, description, resolution_notes, category, ticket_number)
    VALUES ('delete', old.id, old.title, old.description, old.resolution_notes, old.category, old.ticket_number);
END;

CREATE TRIGGER IF NOT EXISTS tickets_fts_au AFTER UPDATE ON tickets BEGIN
    INSERT INTO tickets_fts(tickets_fts, rowid, title, description, resolution_notes, category, ticket_number)
    VALUES ('delete', old.id, old.title, old.description, old.resolution_notes, old.category, old.ticket_number);
    INSERT INTO tickets_fts(rowid, title, description, resolution_notes, category, ticket_number)
    VALUES (new.id, new.title, new.description, new.resolution_notes, new.category, new.ticket_number);
END;

-- Backfill the index from any rows that predate the FTS table. The 'rebuild'
-- command re-reads every row of the content table and rebuilds the inverted
-- index from scratch, which is the documented way to populate an external
-- content table (unlike 'delete'/'delete-all', it does not require one INSERT
-- per row).
INSERT INTO tickets_fts(tickets_fts) VALUES ('rebuild');
`,
	},
	{
		Version: 4,
		Name:    "notification_and_view_indexes",
		SQL: `
CREATE INDEX IF NOT EXISTS idx_tickets_assignee_active
    ON tickets(assignee_id, status) WHERE status NOT IN ('closed');
CREATE INDEX IF NOT EXISTS idx_tickets_sla_scan
    ON tickets(status, resolution_due_at) WHERE resolution_breached = 0;
CREATE INDEX IF NOT EXISTS idx_events_created ON ticket_events(created_at DESC);
`,
	},
	{
		Version: 5,
		Name:    "audit_log",
		SQL: `
-- The audit trail is append-only: who did what, to which ticket, and what the
-- previous and new values were. It is separate from ticket_events because
-- events describe ticket state while audit rows describe account actions
-- (logins, role changes, SLA policy edits) that have no ticket at all.
CREATE TABLE IF NOT EXISTS audit_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    ticket_id   INTEGER REFERENCES tickets(id) ON DELETE CASCADE,
    action      TEXT NOT NULL,
    from_value  TEXT,
    to_value    TEXT,
    details     TEXT,
    ip_address  TEXT,
    user_agent  TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
-- Indexed newest-first for the admin trail view, plus per-ticket lookup.
CREATE INDEX IF NOT EXISTS idx_audit_created  ON audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_actor    ON audit_logs(actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_ticket   ON audit_logs(ticket_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action   ON audit_logs(action, created_at DESC);
`,
	},
}

// migrate applies every unapplied migration inside a transaction, recording it
// in schema_migrations so re-runs are no-ops.
func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version    INTEGER PRIMARY KEY,
            name       TEXT NOT NULL,
            applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
        )`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := d.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fts := hasFTS5(ctx, d.DB)

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		// The FTS5 migration is the only one that needs the extension; skip it
		// loudly rather than aborting the whole startup.
		if m.Version == 3 && !fts {
			return ErrNoFTS5
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.Version, m.Name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
	}
	return nil
}

// InTx runs fn inside a transaction, rolling back on error or panic.
func (d *DB) InTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
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
