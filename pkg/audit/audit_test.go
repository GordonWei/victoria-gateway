package audit

import (
	"context"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestNoopLogger_RecordIsNoop(t *testing.T) {
	var l NoopLogger
	if err := l.Record(context.Background(), Entry{Actor: "x", Action: "y"}); err != nil {
		t.Errorf("Record = %v, want nil", err)
	}
}

func TestNoopLogger_ListReturnsEmpty(t *testing.T) {
	var l NoopLogger
	entries, err := l.List(context.Background(), 10)
	if err != nil {
		t.Errorf("List err = %v, want nil", err)
	}
	if len(entries) != 0 {
		t.Errorf("List = %v, want empty", entries)
	}
}

func newMockLogger(t *testing.T) (*PGLogger, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &PGLogger{db: db}, mock
}

func TestPGLogger_Record(t *testing.T) {
	l, mock := newMockLogger(t)
	mock.ExpectExec("INSERT INTO audit_log").
		WithArgs("alice", "pending.confirm", "id=42", "fixed the disk").
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := l.Record(context.Background(), Entry{
		Actor:  "alice",
		Action: "pending.confirm",
		Target: "id=42",
		Detail: "fixed the disk",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGLogger_Record_PropagatesError(t *testing.T) {
	l, mock := newMockLogger(t)
	mock.ExpectExec("INSERT INTO audit_log").WillReturnError(errors.New("boom"))

	if err := l.Record(context.Background(), Entry{Actor: "a", Action: "b"}); err == nil {
		t.Error("Record = nil, want an error")
	}
}

func TestPGLogger_List(t *testing.T) {
	l, mock := newMockLogger(t)
	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "at", "actor", "action", "target", "detail"}).
		AddRow(int64(2), now, "bob", "maintenance_windows.replace", "", "now 3 window(s)").
		AddRow(int64(1), now.Add(-time.Hour), "alice", "pending.confirm", "id=42", "fixed the disk")
	mock.ExpectQuery("SELECT id, at, actor, action, target, detail FROM audit_log").
		WithArgs(50).
		WillReturnRows(rows)

	entries, err := l.List(context.Background(), 0) // 0 should default to 50
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List returned %d entries, want 2", len(entries))
	}
	if entries[0].Actor != "bob" || entries[0].Action != "maintenance_windows.replace" {
		t.Errorf("entries[0] = %+v, want bob/maintenance_windows.replace", entries[0])
	}
	if entries[1].Actor != "alice" || entries[1].Target != "id=42" {
		t.Errorf("entries[1] = %+v, want alice/id=42", entries[1])
	}
}
