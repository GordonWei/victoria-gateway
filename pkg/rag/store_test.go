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
	t.Cleanup(func() { _ = db.Close() })
	return &PGStore{db: db}, mock
}

var recordRowCols = []string{"id", "alert_name", "host", "log_excerpt", "summary", "resolution", "status", "gitea_issue_number", "created_at", "confirmed_at"}

// searchRowCols is recordRowCols plus the similarity column Search
// computes (1 - cosine distance).
var searchRowCols = append(append([]string{}, recordRowCols...), "similarity")

func TestPGStore_Search(t *testing.T) {
	store, mock := newMockStore(t)

	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	confirmedAt := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(searchRowCols).
		AddRow(int64(1), "InstanceDown", "172.16.100.7", "log excerpt", "old summary", "舊測試機殘留 target，已下線", "confirmed", int64(0), createdAt, confirmedAt, 0.83)

	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'confirmed'").
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
	if records[0].Status != "confirmed" {
		t.Errorf("status = %q, want confirmed", records[0].Status)
	}
	if records[0].Similarity != 0.83 {
		t.Errorf("similarity = %v, want 0.83", records[0].Similarity)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_Search_DefaultsTopK(t *testing.T) {
	store, mock := newMockStore(t)

	rows := sqlmock.NewRows(searchRowCols)
	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'confirmed'").
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
		WithArgs("InstanceDown", "172.16.100.7", "log", "summary", "舊測試機，已下線", "[0.1,0.2]", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := Record{
		AlertName:  "InstanceDown",
		Host:       "172.16.100.7",
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

func TestPGStore_InsertPending(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectQuery("INSERT INTO incidents").
		WithArgs("InstanceDown", "172.16.100.7", "log", "summary", int64(42), "[0.1,0.2]", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(9)))

	rec := Record{
		AlertName:        "InstanceDown",
		Host:             "172.16.100.7",
		LogExcerpt:       "log",
		Summary:          "summary",
		GiteaIssueNumber: 42,
	}
	id, err := store.InsertPending(context.Background(), rec, []float32{0.1, 0.2})
	if err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	if id != 9 {
		t.Errorf("id = %d, want 9", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_InsertPending_NoGiteaIssue(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectQuery("INSERT INTO incidents").
		WithArgs("InstanceDown", "172.16.100.7", "log", "summary", nil, "[0.1]", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(3)))

	rec := Record{AlertName: "InstanceDown", Host: "172.16.100.7", LogExcerpt: "log", Summary: "summary"}
	if _, err := store.InsertPending(context.Background(), rec, []float32{0.1}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_PendingWithGiteaIssue(t *testing.T) {
	store, mock := newMockStore(t)

	createdAt := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(recordRowCols).
		AddRow(int64(5), "InstanceDown", "172.16.100.7", "log", "summary", "", "pending", int64(42), createdAt, time.Time{})
	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'pending'").
		WillReturnRows(rows)

	records, err := store.PendingWithGiteaIssue(context.Background())
	if err != nil {
		t.Fatalf("PendingWithGiteaIssue: %v", err)
	}
	if len(records) != 1 || records[0].GiteaIssueNumber != 42 {
		t.Errorf("unexpected records: %+v", records)
	}
	if !records[0].ConfirmedAt.IsZero() {
		t.Errorf("expected zero ConfirmedAt for a pending record, got %v", records[0].ConfirmedAt)
	}
}

func TestPGStore_Confirm(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec("UPDATE incidents SET resolution").
		WithArgs(int64(5), "舊測試機殘留 target，已下線").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Confirm(context.Background(), 5, "舊測試機殘留 target，已下線"); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_Confirm_RequiresResolution(t *testing.T) {
	store, _ := newMockStore(t)
	if err := store.Confirm(context.Background(), 5, "  "); err == nil {
		t.Error("expected error when resolution is blank")
	}
}

func TestPGStore_Confirm_NoSuchRecord(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec("UPDATE incidents SET resolution").
		WithArgs(int64(999), "x").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.Confirm(context.Background(), 999, "x"); err == nil {
		t.Error("expected error when no row matches the id")
	}
}

func TestPGStore_GetConfirmed(t *testing.T) {
	store, mock := newMockStore(t)

	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	confirmedAt := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(recordRowCols).
		AddRow(int64(5), "DiskFull", "h1", "log", "summary", "清掉 /var/log", "confirmed", int64(0), createdAt, confirmedAt)
	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'confirmed' AND id").
		WithArgs(int64(5)).
		WillReturnRows(rows)

	rec, err := store.GetConfirmed(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetConfirmed: %v", err)
	}
	if rec.Resolution != "清掉 /var/log" {
		t.Errorf("unexpected record: %+v", rec)
	}
}

func TestPGStore_GetConfirmed_NotFound(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'confirmed' AND id").
		WithArgs(int64(999)).
		WillReturnRows(sqlmock.NewRows(recordRowCols))

	_, err := store.GetConfirmed(context.Background(), 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestPGStore_ListConfirmed(t *testing.T) {
	store, mock := newMockStore(t)

	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	confirmedAt := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(recordRowCols).
		AddRow(int64(2), "B", "h2", "", "s", "r2", "confirmed", int64(0), createdAt, confirmedAt).
		AddRow(int64(1), "A", "h1", "", "s", "r1", "confirmed", int64(0), createdAt, confirmedAt)
	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'confirmed'\\s+ORDER BY confirmed_at DESC").
		WithArgs(20).
		WillReturnRows(rows)

	records, err := store.ListConfirmed(context.Background(), ListFilter{}, 0) // 0 falls back to 20
	if err != nil {
		t.Fatalf("ListConfirmed: %v", err)
	}
	if len(records) != 2 || records[0].ID != 2 {
		t.Errorf("unexpected records: %+v", records)
	}
}

func TestPGStore_ListConfirmed_FilterByAlertNameAndHost(t *testing.T) {
	store, mock := newMockStore(t)

	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	confirmedAt := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(recordRowCols).
		AddRow(int64(3), "DiskSpaceWarning", "h3.example.com", "", "s", "r3", "confirmed", int64(0), createdAt, confirmedAt)

	// Both filters set: expect both ILIKE conditions ANDed in, args in
	// the order limit, alert_name, host (filter fields are appended in
	// that order by PGStore.ListConfirmed).
	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'confirmed' AND alert_name ILIKE \\$2 AND host ILIKE \\$3\\s+ORDER BY confirmed_at DESC").
		WithArgs(10, "%DiskSpace%", "%h3%").
		WillReturnRows(rows)

	records, err := store.ListConfirmed(context.Background(), ListFilter{AlertName: "DiskSpace", Host: "h3"}, 10)
	if err != nil {
		t.Fatalf("ListConfirmed: %v", err)
	}
	if len(records) != 1 || records[0].ID != 3 {
		t.Errorf("unexpected records: %+v", records)
	}
}

func TestPGStore_ListConfirmed_FilterByHostOnly(t *testing.T) {
	store, mock := newMockStore(t)

	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	confirmedAt := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(recordRowCols).
		AddRow(int64(4), "AnyAlert", "172.16.100.6", "", "s", "r4", "confirmed", int64(0), createdAt, confirmedAt)

	// Only Host set: alert_name clause must be absent, and the sole
	// filter arg lands at $2 (not $3) since it's the only one appended.
	mock.ExpectQuery("SELECT (.|\n)*FROM incidents\\s+WHERE status = 'confirmed' AND host ILIKE \\$2\\s+ORDER BY confirmed_at DESC").
		WithArgs(20, "%172.16.100.6%").
		WillReturnRows(rows)

	records, err := store.ListConfirmed(context.Background(), ListFilter{Host: "172.16.100.6"}, 0)
	if err != nil {
		t.Fatalf("ListConfirmed: %v", err)
	}
	if len(records) != 1 || records[0].ID != 4 {
		t.Errorf("unexpected records: %+v", records)
	}
}
