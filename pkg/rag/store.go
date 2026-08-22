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

// Record is one incident: an alert, the log context and summary it had at
// the time, and — once someone runs `orch-gateway note` — the resolution
// that was later confirmed. Resolution is what makes a record useful to
// retrieve later; Summary/LogExcerpt are kept for the operator's own
// reference when writing that resolution, not treated as ground truth.
type Record struct {
	ID         int64
	AlertName  string
	Host       string
	LogExcerpt string
	Summary    string
	Resolution string
	CreatedAt  time.Time
}

// Store is the persistence boundary rag talks to. It's an interface so
// the retrieval/formatting logic in this package (and the handler wiring
// in cmd/orch-gateway) can be unit tested against a fake without a real
// Postgres — PGStore is the only production implementation.
type Store interface {
	// Search returns up to topK past records most similar to embedding,
	// nearest first. An empty result (not an error) means nothing in the
	// store was close enough to be useful, or the store is empty.
	Search(ctx context.Context, embedding []float32, topK int) ([]Record, error)
	// Insert stores a new confirmed incident record with its embedding.
	Insert(ctx context.Context, rec Record, embedding []float32) error
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

// Search finds the topK incidents whose embedding is closest to the given
// one by cosine distance (the `<=>` operator, matching the
// vector_cosine_ops index in schema.sql).
func (s *PGStore) Search(ctx context.Context, embedding []float32, topK int) ([]Record, error) {
	if topK <= 0 {
		topK = 3
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, alert_name, host, log_excerpt, summary, resolution, created_at
		FROM incidents
		ORDER BY embedding <=> $1::vector
		LIMIT $2
	`, formatVector(embedding), topK)
	if err != nil {
		return nil, fmt.Errorf("rag search query: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.AlertName, &r.Host, &r.LogExcerpt, &r.Summary, &r.Resolution, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("rag search scan row: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag search iterate rows: %w", err)
	}
	return records, nil
}

// Insert stores a new incident record. Resolution is required (an empty
// resolution isn't useful reference material for a future alert) —
// callers (the `note` CLI) should validate this before calling in, but
// it's checked here too since this is the actual write boundary.
func (s *PGStore) Insert(ctx context.Context, rec Record, embedding []float32) error {
	if strings.TrimSpace(rec.Resolution) == "" {
		return fmt.Errorf("rag insert: resolution is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO incidents (alert_name, host, log_excerpt, summary, resolution, embedding, created_at)
		VALUES ($1, $2, $3, $4, $5, $6::vector, $7)
	`, rec.AlertName, rec.Host, rec.LogExcerpt, rec.Summary, rec.Resolution, formatVector(embedding), rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("rag insert: %w", err)
	}
	return nil
}
