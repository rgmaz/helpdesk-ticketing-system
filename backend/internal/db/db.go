package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver: no CGO, no cgo toolchain required.
	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB with a schema-migration helper and SQLite pragmas applied
// once per pooled connection.
type DB struct {
	*sql.DB
	fts5 bool
}

// HasFTS5 reports whether full-text search is available on this build.
func (d *DB) HasFTS5() bool { return d.fts5 }

// Open creates the parent directory if needed, opens the SQLite file, applies
// pragmas, runs migrations, and returns a ready connection pool.
func Open(path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	dsn := "file:" + url.PathEscape(abs) + "?" + url.Values{
		"_pragma": []string{
			"foreign_keys(1)",
			"journal_mode(WAL)",
			"busy_timeout(5000)",
			"synchronous(NORMAL)",
		},
		"_time_format": []string{"sqlite"},
	}.Encode()

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite serialises writes; a small pool with a long-lived idle window keeps
	// throughput predictable for a single-node deployment.
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	d := &DB{DB: sqlDB, fts5: hasFTS5(ctx, sqlDB)}
	if !d.fts5 {
		sqlDB.Close()
		return nil, ErrNoFTS5
	}
	if err := d.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// ErrNoFTS5 indicates the bundled SQLite build lacks the FTS5 extension, which
// full-text search depends on.
var ErrNoFTS5 = errors.New("this SQLite build does not include the FTS5 extension")

// hasFTS5 probes the driver for FTS5 support so startup fails with a clear
// message rather than part-way through the schema migration.
func hasFTS5(ctx context.Context, d *sql.DB) bool {
	// sqlite_compileoption_used returns an integer flag, so scan into int.
	var used int
	if err := d.QueryRowContext(ctx, `SELECT sqlite_compileoption_used('ENABLE_FTS5')`).Scan(&used); err == nil && used != 0 {
		return true
	}
	// The compile-option table can be unavailable on some builds; fall back to
	// actually creating a throwaway FTS5 table.
	_, err := d.ExecContext(ctx, `CREATE VIRTUAL TABLE IF NOT EXISTS temp.__fts5_probe USING fts5(x)`)
	if err != nil {
		return false
	}
	_, _ = d.ExecContext(ctx, `DROP TABLE IF EXISTS temp.__fts5_probe`)
	return true
}
