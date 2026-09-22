package rag

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPendingGroup_Span(t *testing.T) {
	base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	// A single-row group has no span to speak of — reporting one would
	// imply a recurrence that hasn't happened.
	if got := (PendingGroup{Size: 1, NewestAt: base, OldestAt: base}).Span(); got != 0 {
		t.Errorf("single-row span = %v, want 0", got)
	}
	g := PendingGroup{Size: 35, NewestAt: base, OldestAt: base.Add(-9 * 24 * time.Hour)}
	if got := g.Span(); got != 9*24*time.Hour {
		t.Errorf("span = %v, want 216h", got)
	}
}

func TestPGStore_ListPendingGroups(t *testing.T) {
	store, mock := newMockStore(t)
	newest := time.Date(2026, 9, 22, 4, 12, 0, 0, time.UTC)
	oldest := time.Date(2026, 9, 14, 0, 11, 0, 0, time.UTC)

	rows := sqlmock.NewRows([]string{"rep_id", "alert_name", "host", "n", "summary", "newest", "oldest", "member_ids"}).
		AddRow(int64(91), "SSLCertExpiringSoon", "https://www.kmp.tw", 35, "憑證將於 10-13 到期", newest, oldest, "{91,88,80}")

	mock.ExpectQuery("WITH pend AS(.|\n)*GROUP BY alert_name, host").
		WithArgs(20).
		WillReturnRows(rows)

	groups, err := store.ListPendingGroups(context.Background(), ListFilter{}, 0)
	if err != nil {
		t.Fatalf("ListPendingGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	g := groups[0]
	if g.RepID != 91 || g.Size != 35 || g.AlertName != "SSLCertExpiringSoon" {
		t.Errorf("unexpected group: %+v", g)
	}
	if len(g.MemberIDs) != 3 || g.MemberIDs[0] != 91 {
		t.Errorf("member ids = %v, want [91 88 80]", g.MemberIDs)
	}
	if g.Span() != newest.Sub(oldest) {
		t.Errorf("span = %v, want %v", g.Span(), newest.Sub(oldest))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The batch form only carries the representative's id, so the server
// re-derives the group at submit time. Rows created *after* the
// representative must stay out: nobody looked at those.
func TestPGStore_GroupMemberIDs_BoundedByRepresentativeTime(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery("i.created_at <= rep.created_at").
		WithArgs(int64(91)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(91)).AddRow(int64(88)))

	ids, err := store.GroupMemberIDs(context.Background(), 91)
	if err != nil {
		t.Fatalf("GroupMemberIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != 91 {
		t.Errorf("ids = %v, want representative first", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestPGStore_ConfirmPendingGroup(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE incidents(.|\n)*SET resolution").
		WithArgs("{91,88,80}", "憑證由 ACM 自動續約，不需人工處理").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow(int64(91)).AddRow(int64(88)).AddRow(int64(80)))
	mock.ExpectExec("UPDATE incidents SET dup_of = \\$2").
		WithArgs("{88,80}", int64(91)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	// The representative's own dup_of is cleared, not left alone: an
	// earlier batch may have set it, and a representative still pointing
	// at an older row would take this whole group out of Search.
	mock.ExpectExec("UPDATE incidents SET dup_of = NULL WHERE id = \\$1").
		WithArgs(int64(91)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := store.ConfirmPendingGroup(context.Background(),
		[]int64{91, 88, 80}, "憑證由 ACM 自動續約，不需人工處理", 91)
	if err != nil {
		t.Fatalf("ConfirmPendingGroup: %v", err)
	}
	if n != 3 {
		t.Errorf("confirmed = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Rows someone else confirmed between page load and submit are not in
// RETURNING, and must not be marked as duplicates of this group — doing so
// would drop their (possibly contradictory) resolution out of Search with
// nothing to show it happened.
func TestPGStore_ConfirmPendingGroup_OnlyMarksRowsItActuallyConfirmed(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE incidents(.|\n)*SET resolution").
		WithArgs("{91,88,80}", "同上").
		// 88 was confirmed by someone else already: not returned.
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(91)).AddRow(int64(80)))
	mock.ExpectExec("UPDATE incidents SET dup_of = \\$2").
		WithArgs("{80}", int64(91)). // 88 absent
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE incidents SET dup_of = NULL WHERE id = \\$1").
		WithArgs(int64(91)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := store.ConfirmPendingGroup(context.Background(), []int64{91, 88, 80}, "同上", 91)
	if err != nil {
		t.Fatalf("ConfirmPendingGroup: %v", err)
	}
	if n != 2 {
		t.Errorf("confirmed = %d, want 2 (88 was already confirmed)", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Every row already confirmed is not an error — it's what a concurrent
// confirm looks like. Commit and report zero.
func TestPGStore_ConfirmPendingGroup_AllAlreadyConfirmed(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE incidents(.|\n)*SET resolution").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectCommit()

	n, err := store.ConfirmPendingGroup(context.Background(), []int64{91}, "x", 91)
	if err != nil {
		t.Fatalf("ConfirmPendingGroup: %v", err)
	}
	if n != 0 {
		t.Errorf("confirmed = %d, want 0", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// dup_of pointing at a row this call didn't confirm would be a dangling
// reference into another operator's decision.
func TestPGStore_ConfirmPendingGroup_RejectsForeignRepresentative(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.ConfirmPendingGroup(context.Background(), []int64{88, 80}, "x", 91)
	if err == nil || !strings.Contains(err.Error(), "not in the batch") {
		t.Fatalf("err = %v, want a complaint about the representative", err)
	}
}

// Same rule as the single-record confirm: a batch that writes an empty
// resolution to N rows is worse than one that writes it to one.
func TestPGStore_ConfirmPendingGroup_RequiresResolution(t *testing.T) {
	store, _ := newMockStore(t)
	if _, err := store.ConfirmPendingGroup(context.Background(), []int64{91}, "   ", 91); err == nil {
		t.Fatal("blank resolution should be rejected")
	}
}

// Search must not offer duplicates back as retrieval examples, or one
// batch-confirmed burst crowds out everything else.
func TestPGStore_Search_ExcludesDuplicates(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery("WHERE status = 'confirmed'\\s+AND dup_of IS NULL").
		WithArgs("[0.1]", 3).
		WillReturnRows(sqlmock.NewRows(searchRowCols))
	if _, err := store.Search(context.Background(), []float32{0.1}, 3); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Search is not filtering on dup_of: %v", err)
	}
}

func TestPqInt64Array_RoundTrip(t *testing.T) {
	v, err := pqInt64Array{91, 88, 80}.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if v != "{91,88,80}" {
		t.Errorf("Value = %v, want {91,88,80}", v)
	}
	for _, src := range []any{"{91,88,80}", []byte("{91,88,80}")} {
		var a pqInt64Array
		if err := a.Scan(src); err != nil {
			t.Fatalf("Scan(%T): %v", src, err)
		}
		if len(a) != 3 || a[0] != 91 || a[2] != 80 {
			t.Errorf("Scan(%T) = %v", src, a)
		}
	}
	var empty pqInt64Array
	if err := empty.Scan("{}"); err != nil || empty != nil {
		t.Errorf("empty array: %v %v", empty, err)
	}
	if err := empty.Scan(nil); err != nil || empty != nil {
		t.Errorf("nil: %v %v", empty, err)
	}
}
