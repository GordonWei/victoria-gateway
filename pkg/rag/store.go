package rag

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// Status values for Record.Status. A row is Pending the moment an alert
// is captured — nothing about it is verified yet — and becomes Confirmed
// only once a human attaches a real resolution (via the `note` CLI
// directly, or via `sync` reading a linked Gitea issue's closing
// comment). Search only ever returns Confirmed rows.
const (
	StatusPending   = "pending"
	StatusConfirmed = "confirmed"
)

// Record is one incident: an alert, the log context and summary it had at
// capture time, and — once confirmed — the resolution that was later
// verified. Resolution is what makes a Confirmed record useful to
// retrieve later; Summary/LogExcerpt are kept as reference for whoever
// writes that resolution, not treated as ground truth on their own.
type Record struct {
	ID         int64
	AlertName  string
	Host       string
	LogExcerpt string
	Summary    string
	Resolution string
	Status     string
	// GiteaIssueNumber is 0 if not linked to a tracker issue. Named for
	// when Gitea was the only backend; it's used the same way regardless
	// of whether pkg/gitea or pkg/github (see pkg/tracker) filed it — the
	// DB column wasn't renamed along with the code to avoid a schema
	// migration over a field that already had real rows in it.
	GiteaIssueNumber int64
	CreatedAt        time.Time
	ConfirmedAt      time.Time // zero if not yet Confirmed
}

// Store is the persistence boundary rag talks to. It's an interface so
// the retrieval/formatting logic in this package (and the handler wiring
// in cmd/victoria-gateway) can be unit tested against a fake without a real
// Postgres — PGStore is the only production implementation.
type Store interface {
	// Search returns up to topK Confirmed records most similar to
	// embedding, nearest first. Pending records are never returned — an
	// empty result (not an error) means nothing Confirmed was close
	// enough to be useful, or there's nothing Confirmed yet.
	Search(ctx context.Context, embedding []float32, topK int) ([]Record, error)
	// Insert stores a new Confirmed incident record directly (used by
	// `victoria-gateway note` when there's no prior pending capture to
	// attach a resolution to).
	Insert(ctx context.Context, rec Record, embedding []float32) error
	// InsertPending stores a new Pending record captured right after an
	// alert was analyzed — no resolution yet, not retrievable by Search
	// until confirmed. Returns the row's ID so a Gitea issue can
	// reference it back.
	InsertPending(ctx context.Context, rec Record, embedding []float32) (int64, error)
	// PendingWithGiteaIssue returns all Pending records that have a
	// linked Gitea issue, for `sync` to check against.
	PendingWithGiteaIssue(ctx context.Context) ([]Record, error)
	// Confirm attaches a resolution to an existing record (Pending or
	// not) and marks it Confirmed, making it eligible for Search.
	Confirm(ctx context.Context, id int64, resolution string) error
	Close() error
}

// PGStore is a Store backed by PostgreSQL + the pgvector extension. See
// schema.sql for the table/index this expects to already exist —
// PGStore does not run migrations itself.
type PGStore struct {
	db *sql.DB
}

// OpenPostgres opens a connection pool against dsn (a standard Postgres
// connection string) and verifies it's reachable. Callers must Close() it
// when done.
func OpenPostgres(dsn string) (*PGStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &PGStore{db: db}, nil
}

func (s *PGStore) Close() error {
	return s.db.Close()
}

// formatVector renders an embedding in pgvector's text input format
// (e.g. "[0.1,0.2,0.3]"), which is what the `::vector` cast in the SQL
// below expects. pgvector has no binary encoding reachable from plain
// database/sql, so this is the standard way to pass a vector as a query
// parameter without a driver-specific vector type.
func formatVector(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

const recordColumns = "id, alert_name, host, log_excerpt, summary, resolution, status, COALESCE(gitea_issue_number, 0), created_at, COALESCE(confirmed_at, 'epoch'::timestamptz)"

func scanRecord(row interface{ Scan(...any) error }) (Record, error) {
	var r Record
	var confirmedAt time.Time
	err := row.Scan(&r.ID, &r.AlertName, &r.Host, &r.LogExcerpt, &r.Summary, &r.Resolution, &r.Status, &r.GiteaIssueNumber, &r.CreatedAt, &confirmedAt)
	if err != nil {
		return Record{}, err
	}
	if !confirmedAt.IsZero() && confirmedAt.Unix() != 0 {
		r.ConfirmedAt = confirmedAt
	}
	return r, nil
}

// Search finds the topK Confirmed incidents whose embedding is closest to
// the given one by cosine distance (the `<=>` operator, matching the
// vector_cosine_ops index in schema.sql).
func (s *PGStore) Search(ctx context.Context, embedding []float32, topK int) ([]Record, error) {
	if topK <= 0 {
		topK = 3
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+recordColumns+`
		FROM incidents
		WHERE status = 'confirmed'
		ORDER BY embedding <=> $1::vector
		LIMIT $2
	`, formatVector(embedding), topK)
	if err != nil {
		return nil, fmt.Errorf("rag search query: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("rag search scan row: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag search iterate rows: %w", err)
	}
	return records, nil
}

// Insert stores a new Confirmed incident record. Resolution is required
// (an empty resolution isn't useful reference material for a future
// alert) — callers (the `note` CLI) should validate this before calling
// in, but it's checked here too since this is the actual write boundary.
func (s *PGStore) Insert(ctx context.Context, rec Record, embedding []float32) error {
	if strings.TrimSpace(rec.Resolution) == "" {
		return fmt.Errorf("rag insert: resolution is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO incidents (alert_name, host, log_excerpt, summary, resolution, status, embedding, created_at, confirmed_at)
		VALUES ($1, $2, $3, $4, $5, 'confirmed', $6::vector, $7, $7)
	`, rec.AlertName, rec.Host, rec.LogExcerpt, rec.Summary, rec.Resolution, formatVector(embedding), rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("rag insert: %w", err)
	}
	return nil
}

// InsertPending stores a new Pending record — no resolution yet. Used
// right after an alert is analyzed, before anyone has confirmed what it
// actually was. GiteaIssueNumber, if set, links it to the issue `sync`
// should watch for a closing resolution.
func (s *PGStore) InsertPending(ctx context.Context, rec Record, embedding []float32) (int64, error) {
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	var id int64
	var issueNumber any
	if rec.GiteaIssueNumber != 0 {
		issueNumber = rec.GiteaIssueNumber
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO incidents (alert_name, host, log_excerpt, summary, resolution, status, gitea_issue_number, embedding, created_at)
		VALUES ($1, $2, $3, $4, '', 'pending', $5, $6::vector, $7)
		RETURNING id
	`, rec.AlertName, rec.Host, rec.LogExcerpt, rec.Summary, issueNumber, formatVector(embedding), rec.CreatedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("rag insert pending: %w", err)
	}
	return id, nil
}

// PendingWithGiteaIssue returns every Pending record that has a linked
// Gitea issue, for `sync` to check the issue's current state against.
func (s *PGStore) PendingWithGiteaIssue(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+recordColumns+`
		FROM incidents
		WHERE status = 'pending' AND gitea_issue_number IS NOT NULL
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("rag pending query: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("rag pending scan row: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// Confirm attaches resolution to an existing record and marks it
// Confirmed, making it eligible for Search from then on.
func (s *PGStore) Confirm(ctx context.Context, id int64, resolution string) error {
	if strings.TrimSpace(resolution) == "" {
		return fmt.Errorf("rag confirm: resolution is required")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE incidents SET resolution = $2, status = 'confirmed', confirmed_at = now()
		WHERE id = $1
	`, id, resolution)
	if err != nil {
		return fmt.Errorf("rag confirm: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rag confirm: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("rag confirm: no record with id %d", id)
	}
	return nil
}
