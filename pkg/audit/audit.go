// Package audit records who changed what, and when, for the handful of
// operations in victoria-gateway that change live system behavior rather
// than just observing it: replacing the maintenance window set, confirming
// a pending incident's resolution, and applying a suppression-rule
// candidate as a real Alertmanager silence. Everything else (an alert
// being analyzed, escalated, notified) is already visible in the process
// log and pkg/metrics — this package exists for the question those don't
// answer well: "who did this, and when," after the fact, queryable without
// grepping historical container logs that may have rotated away.
//
// Deliberately reuses the RAG Postgres database rather than inventing a
// second storage dependency (it opens its own separate *sql.DB pool
// against the same rag.postgres_dsn — pkg/rag's own pool isn't exported
// for this to share directly — but that's an implementation detail, not
// a second database to provision): audit logging is only available when
// rag.enabled is true (see config.RAGConfig.AuditLog), the same way
// suppression-candidates already requires RAG to have any confirmed
// history to analyze. A deployment that doesn't run RAG gets NoopLogger —
// every Record call is a no-op — so callers never need a nil check.
package audit

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// Entry is one recorded operation.
type Entry struct {
	ID int64
	// Time is when the operation happened. Record sets this itself
	// (server-side now()) — callers don't supply it.
	Time time.Time
	// Actor identifies who performed the operation: the webui_auth
	// username when the web UI is authenticated, the caller's remote IP
	// otherwise (see cmd/victoria-gateway's actorFromRequest), or a fixed
	// string like "cli" for operations triggered from a command-line tool
	// rather than an HTTP request. Never empty — an operation with no
	// identifiable actor still records "unknown" rather than silently
	// omitting the field, since an empty audit trail entry is worse than
	// one honestly admitting it doesn't know who.
	Actor string
	// Action is a short, stable machine-readable label, e.g.
	// "maintenance_windows.replace" or "pending.confirm" — stable across
	// versions so a future query can filter on it reliably.
	Action string
	// Target identifies what the action applied to, e.g. a pending
	// record's id or a suppression candidate's alertname+host. Empty when
	// the action has no single target (e.g. replacing the entire
	// maintenance window set).
	Target string
	// Detail is a short human-readable description of what changed —
	// not a full diff, just enough for "what happened here" to make sense
	// without cross-referencing the process log from the same moment.
	Detail string
}

// Logger is the persistence boundary this package talks to. It's an
// interface so callers can be unit tested against a fake, and so
// NoopLogger can stand in when audit logging isn't configured — see the
// package doc.
type Logger interface {
	// Record stores one entry. Entry.Time is set by the implementation,
	// not read from the argument. Best-effort by convention: callers treat
	// a Record failure as log-and-continue, the same way pkg/rag's capture
	// path does — an audit trail write failing must never block or fail
	// the operation it was recording.
	Record(ctx context.Context, e Entry) error
	// List returns up to limit entries, most recent first.
	List(ctx context.Context, limit int) ([]Entry, error)
}

// NoopLogger discards every Record call and returns an empty List. The
// zero value is ready to use.
type NoopLogger struct{}

func (NoopLogger) Record(ctx context.Context, e Entry) error            { return nil }
func (NoopLogger) List(ctx context.Context, limit int) ([]Entry, error) { return nil, nil }

// PGLogger is the Postgres-backed Logger, writing to the audit_log table
// (see pkg/rag/schema.sql — audit shares that schema file rather than
// having its own, since it's the same database).
type PGLogger struct {
	db *sql.DB
}

// OpenPostgres opens a connection pool against dsn — expected to be the
// same rag.postgres_dsn already configured for RAG, not a separate
// database. Callers should Close it on shutdown.
func OpenPostgres(dsn string) (*PGLogger, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("audit: open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("audit: ping postgres: %w", err)
	}
	return &PGLogger{db: db}, nil
}

func (l *PGLogger) Close() error { return l.db.Close() }

func (l *PGLogger) Record(ctx context.Context, e Entry) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO audit_log (actor, action, target, detail) VALUES ($1, $2, $3, $4)`,
		e.Actor, e.Action, e.Target, e.Detail)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

func (l *PGLogger) List(ctx context.Context, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, at, actor, action, target, detail FROM audit_log ORDER BY at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Time, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, fmt.Errorf("audit: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
