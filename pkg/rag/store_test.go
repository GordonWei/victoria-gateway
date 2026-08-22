package rag

import (
	"context"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestFormatVector(t *testing.T) {
	got := formatVector([]float32{0.1, -0.5, 1})
	want := "[0.1,-0.5,1]"
	if got != want {
		t.Errorf("formatVector = %q, want %q", got, want)
	}
}

func TestFormatVector_Empty(t *testing.T) {
	if got := formatVector(nil); got != "[]" {
		t.Errorf("formatVector(nil) = %q, want %q", got, "[]")
	}
}

func newMockStore(t *testing.T) (*PGStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &PGStore{db: db}, mock
}

func TestPGStore_Search(t *testing.T) {
	store, mock := newMockStore(t)

	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "alert_name", "host", "log_excerpt", "summary", "resolution", "created_at"}).
		AddRow(int64(1), "InstanceDown", "192.0.2.7", "log excerpt", "old summary", "舊測試機殘留 target，已下線", createdAt)

	mock.ExpectQuery("SELECT id, alert_name, host, log_excerpt, summary, resolution, created_at").
		WithArgs("[0.1,0.2]", 3).
		WillReturnRows(rows)

	records, err := store.Search(context.Background(), []float32{0.1, 0.2}, 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].AlertName != "InstanceDown" || records[0].Resolution != "舊測試機殘留 target，已下線" {
		t.Errorf("unexpected record: %+v", records[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_Search_DefaultsTopK(t *testing.T) {
	store, mock := newMockStore(t)

	rows := sqlmock.NewRows([]string{"id", "alert_name", "host", "log_excerpt", "summary", "resolution", "created_at"})
	mock.ExpectQuery("SELECT id, alert_name, host, log_excerpt, summary, resolution, created_at").
		WithArgs("[1]", 3).
		WillReturnRows(rows)

	// topK <= 0 should fall back to 3 rather than sending a nonsensical
	// LIMIT to Postgres.
	if _, err := store.Search(context.Background(), []float32{1}, 0); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_Search_QueryError(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery("SELECT").WillReturnError(errors.New("connection reset"))

	_, err := store.Search(context.Background(), []float32{0.1}, 3)
	if err == nil {
		t.Error("expected error when the query fails")
	}
}

func TestPGStore_Insert(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec("INSERT INTO incidents").
		WithArgs("InstanceDown", "192.0.2.7", "log", "summary", "舊測試機，已下線", "[0.1,0.2]", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := Record{
		AlertName:  "InstanceDown",
		Host:       "192.0.2.7",
		LogExcerpt: "log",
		Summary:    "summary",
		Resolution: "舊測試機，已下線",
	}
	if err := store.Insert(context.Background(), rec, []float32{0.1, 0.2}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_Insert_RequiresResolution(t *testing.T) {
	store, _ := newMockStore(t)

	rec := Record{AlertName: "InstanceDown", Host: "x"} // no Resolution
	err := store.Insert(context.Background(), rec, []float32{0.1})
	if err == nil {
		t.Error("expected error when Resolution is empty")
	}
}
