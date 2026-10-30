package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// groupingRAGStore is fakeRAGStore plus the four methods the grouped
// pending list needs. Kept separate so the existing fake — used by a lot
// of tests that don't care about grouping — doesn't have to grow them.
type groupingRAGStore struct {
	*fakeRAGStore
	groups     []rag.PendingGroup
	members    map[int64][]int64
	confirmed  map[int64]int // repID -> rows confirmed
	confirmErr error
	// resolutions records what each group was confirmed with, so a test
	// can assert every group in one submission got the same text.
	resolutions map[int64]string
	// issues maps an incident id to its linked tracker issue number. Only
	// ids present here have one; GiteaIssuesForIDs returns exactly the
	// intersection of its argument with this map.
	issues map[int64]int64
	// giteaLookupErr, if set, makes GiteaIssuesForIDs fail — used to prove
	// a lookup failure doesn't undo the DB confirmation.
	giteaLookupErr error
	// giteaLookupIDs records the ids passed to GiteaIssuesForIDs, so a
	// test can assert the batch path only tries to close rows it actually
	// confirmed.
	giteaLookupIDs [][]int64
}

func (g *groupingRAGStore) ListPendingGroups(ctx context.Context, f rag.ListFilter, limit int) ([]rag.PendingGroup, error) {
	return g.groups, nil
}

func (g *groupingRAGStore) CountPending(ctx context.Context, f rag.ListFilter) (int, error) {
	n := 0
	for _, gr := range g.groups {
		n += gr.Size
	}
	return n, nil
}

func (g *groupingRAGStore) GroupMemberIDs(ctx context.Context, repID int64) ([]int64, error) {
	return g.members[repID], nil
}

func (g *groupingRAGStore) ConfirmPendingGroup(ctx context.Context, ids []int64, resolution string, repID int64) ([]int64, error) {
	if g.confirmErr != nil {
		return nil, g.confirmErr
	}
	if g.confirmed == nil {
		g.confirmed = map[int64]int{}
	}
	if g.resolutions == nil {
		g.resolutions = map[int64]string{}
	}
	g.confirmed[repID] = len(ids)
	g.resolutions[repID] = resolution
	// Echo the ids back as "actually confirmed", matching the real store's
	// contract: the batch path closes the issues linked to exactly these.
	return append([]int64(nil), ids...), nil
}

func (g *groupingRAGStore) GiteaIssuesForIDs(ctx context.Context, ids []int64) (map[int64]int64, error) {
	g.giteaLookupIDs = append(g.giteaLookupIDs, append([]int64(nil), ids...))
	if g.giteaLookupErr != nil {
		return nil, g.giteaLookupErr
	}
	out := map[int64]int64{}
	for _, id := range ids {
		if n, ok := g.issues[id]; ok && n != 0 {
			out[id] = n
		}
	}
	return out, nil
}

func batchTestHandler() (*handler, *groupingRAGStore) {
	store := &groupingRAGStore{
		fakeRAGStore: &fakeRAGStore{},
		members: map[int64][]int64{
			91: {91, 88, 80},
			42: {42},
		},
		groups: []rag.PendingGroup{
			{RepID: 91, AlertName: "SSLCertExpiringSoon", Host: "https://www.kmp.tw", Size: 3,
				Summary: "憑證即將到期", NewestAt: time.Now(), OldestAt: time.Now().Add(-9 * 24 * time.Hour)},
			{RepID: 42, AlertName: "InstanceDown", Host: "192.0.2.6:9222", Size: 1,
				Summary: "服務沒回應", NewestAt: time.Now(), OldestAt: time.Now()},
		},
	}
	return &handler{rag: store, batchConfirm: true}, store
}

func postBatch(h *handler, form url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/pending/batch", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.handlePendingBatch(w, r)
	return w
}

func TestHandlePendingBatch_ConfirmsEveryCheckedGroup(t *testing.T) {
	h, store := batchTestHandler()
	w := postBatch(h, url.Values{
		"rep":        {"91", "42"},
		"resolution": {"ACM 會自動續約，不需處理"},
	})
	body := w.Body.String()
	if !strings.Contains(body, "批次確認完成") {
		t.Fatalf("expected success page, got: %s", body)
	}
	if store.confirmed[91] != 3 || store.confirmed[42] != 1 {
		t.Errorf("confirmed = %v, want {91:3, 42:1}", store.confirmed)
	}
	// Both groups must carry the same resolution — that's the whole point
	// of one textarea for the submission.
	if store.resolutions[91] != store.resolutions[42] {
		t.Errorf("groups got different resolutions: %v", store.resolutions)
	}
	// 4 rows across 2 groups means 2 of them are duplicates; the page has
	// to say so, because that's what stops them reaching RAG.
	if !strings.Contains(body, "4 筆") || !strings.Contains(body, "2 筆標記為同群代表列的重複") {
		t.Errorf("result should account for both the row count and the duplicates: %s", body)
	}
}

// --- batch close-issue tests (the fix this file exercises) ---

// batchCloseHandler builds a handler whose store links the SSLCert group's
// three rows to issues #42/#43/#44 and the InstanceDown singleton to #50,
// with a fakeTracker to observe CloseWithComment.
func batchCloseHandler(toggle bool) (*handler, *groupingRAGStore, *fakeTracker) {
	h, store := batchTestHandler()
	store.issues = map[int64]int64{91: 42, 88: 43, 80: 44, 42: 50}
	tr := &fakeTracker{}
	h.tracker = tr
	h.closeIssueOnWebConfirm = toggle
	return h, store, tr
}

// The whole point of this change: with the toggle on, confirming a group
// batch-closes the issue linked to every confirmed row, not just the DB.
func TestHandlePendingBatch_ClosesIssuesForEveryConfirmedRow(t *testing.T) {
	h, _, tr := batchCloseHandler(true)
	w := postBatch(h, url.Values{
		"rep":        {"91", "42"},
		"resolution": {"ACM 自動續約，不需處理"},
	})
	if !strings.Contains(w.Body.String(), "批次確認完成") {
		t.Fatalf("expected success page, got: %s", w.Body.String())
	}
	// 91's group (91,88,80 -> #42,#43,#44) plus 42's singleton (-> #50).
	if len(tr.closeCalls) != 4 {
		t.Fatalf("CloseWithComment called %d times, want 4 (one per linked confirmed row): %+v", len(tr.closeCalls), tr.closeCalls)
	}
	closed := map[int64]string{}
	for _, c := range tr.closeCalls {
		closed[c.Number] = c.Comment
	}
	for _, n := range []int64{42, 43, 44, 50} {
		if _, ok := closed[n]; !ok {
			t.Errorf("issue #%d was not closed: %+v", n, tr.closeCalls)
		}
		if closed[n] != "ACM 自動續約，不需處理" {
			t.Errorf("issue #%d closed with wrong comment %q", n, closed[n])
		}
	}
}

// Toggle off (the OSS default) must behave exactly as before: DB only, no
// tracker calls.
func TestHandlePendingBatch_DoesNotCloseIssuesWhenToggleOff(t *testing.T) {
	h, store, tr := batchCloseHandler(false)
	w := postBatch(h, url.Values{"rep": {"91", "42"}, "resolution": {"x"}})
	if !strings.Contains(w.Body.String(), "批次確認完成") {
		t.Fatalf("expected success page, got: %s", w.Body.String())
	}
	if len(tr.closeCalls) != 0 {
		t.Errorf("CloseWithComment must not be called when the toggle is off: %+v", tr.closeCalls)
	}
	// DB confirmation still happened.
	if store.confirmed[91] != 3 || store.confirmed[42] != 1 {
		t.Errorf("confirmed = %v, want {91:3, 42:1}", store.confirmed)
	}
}

// Rows without a linked issue are simply skipped — no spurious close, no
// error — while rows that do have one in the same batch still close.
func TestHandlePendingBatch_SkipsRowsWithoutLinkedIssue(t *testing.T) {
	h, store, tr := batchCloseHandler(true)
	// Drop the links for 88 and the singleton 42: only 91 and 80 remain.
	store.issues = map[int64]int64{91: 42, 80: 44}
	w := postBatch(h, url.Values{"rep": {"91", "42"}, "resolution": {"x"}})
	if !strings.Contains(w.Body.String(), "批次確認完成") {
		t.Fatalf("expected success page, got: %s", w.Body.String())
	}
	if len(tr.closeCalls) != 2 {
		t.Fatalf("want 2 closes (only 91 and 80 are linked), got %d: %+v", len(tr.closeCalls), tr.closeCalls)
	}
}

// A tracker failure must not roll back the DB confirmation or fail the
// request — same contract as the single-record path. The other rows in
// the batch must still get their close attempt.
func TestHandlePendingBatch_TrackerErrorStillConfirmsInDB(t *testing.T) {
	h, store, tr := batchCloseHandler(true)
	tr.closeErr = fmt.Errorf("gitea is down")
	w := postBatch(h, url.Values{"rep": {"91", "42"}, "resolution": {"x"}})
	if w.Code != http.StatusOK {
		t.Fatalf("a tracker failure must not surface as an error: code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "批次確認完成") {
		t.Fatalf("DB confirm must be reported as success regardless of tracker outcome: %s", w.Body.String())
	}
	if store.confirmed[91] != 3 || store.confirmed[42] != 1 {
		t.Errorf("DB confirm must proceed regardless of tracker outcome, got %v", store.confirmed)
	}
}

// A lookup failure (not a per-issue close failure) is logged and ignored
// too: the DB is still the source of truth, and the other groups in the
// submission must still be processed.
func TestHandlePendingBatch_IssueLookupErrorStillConfirms(t *testing.T) {
	h, store, _ := batchCloseHandler(true)
	store.giteaLookupErr = fmt.Errorf("db hiccup")
	w := postBatch(h, url.Values{"rep": {"91", "42"}, "resolution": {"x"}})
	if !strings.Contains(w.Body.String(), "批次確認完成") {
		t.Fatalf("lookup failure must not fail the request: %s", w.Body.String())
	}
	if store.confirmed[91] != 3 || store.confirmed[42] != 1 {
		t.Errorf("DB confirm must proceed, got %v", store.confirmed)
	}
}

// The batch path must only try to close issues for rows it actually
// confirmed — exactly what ConfirmPendingGroup echoed back, not the
// pre-confirm group it looked up.
func TestHandlePendingBatch_ClosesOnlyConfirmedRows(t *testing.T) {
	h, store, _ := batchCloseHandler(true)
	postBatch(h, url.Values{"rep": {"91"}, "resolution": {"x"}})
	if len(store.giteaLookupIDs) != 1 {
		t.Fatalf("GiteaIssuesForIDs called %d times, want 1", len(store.giteaLookupIDs))
	}
	got := store.giteaLookupIDs[0]
	if len(got) != 3 {
		t.Fatalf("looked up %v, want the 3 confirmed rows of group 91", got)
	}
}

// A batch writes one resolution into every row it touches, so a blank one
// is worse here than on the single-record form.
func TestHandlePendingBatch_RequiresResolution(t *testing.T) {
	h, store := batchTestHandler()
	w := postBatch(h, url.Values{"rep": {"91"}, "resolution": {"   "}})
	if !strings.Contains(w.Body.String(), "沒有填寫結論") {
		t.Fatalf("blank resolution should be rejected: %s", w.Body.String())
	}
	if len(store.confirmed) != 0 {
		t.Error("nothing should have been confirmed")
	}
}

func TestHandlePendingBatch_NoSelection(t *testing.T) {
	h, _ := batchTestHandler()
	w := postBatch(h, url.Values{"resolution": {"x"}})
	if !strings.Contains(w.Body.String(), "沒有勾選任何群組") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandlePendingBatch_RejectsGet(t *testing.T) {
	h, _ := batchTestHandler()
	w := httptest.NewRecorder()
	h.handlePendingBatch(w, httptest.NewRequest(http.MethodGet, "/pending/batch", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", w.Code)
	}
}

// Posting here on a deployment that never ran migrate_0003 must explain
// itself rather than 500 — the list page wouldn't even have shown the form.
func TestHandlePendingBatch_DisabledWithoutMigration(t *testing.T) {
	h, _ := batchTestHandler()
	h.batchConfirm = false
	w := postBatch(h, url.Values{"rep": {"91"}, "resolution": {"x"}})
	body := w.Body.String()
	if !strings.Contains(body, "批次確認未啟用") || !strings.Contains(body, "migrate_0003") {
		t.Fatalf("should name the migration that enables it: %s", body)
	}
}

// A group someone else confirmed first comes back with no members. That's
// a normal concurrent outcome, and it has to be reported, not swallowed.
func TestHandlePendingBatch_ReportsSkippedGroups(t *testing.T) {
	h, store := batchTestHandler()
	store.members[91] = nil
	w := postBatch(h, url.Values{"rep": {"91"}, "resolution": {"x"}})
	body := w.Body.String()
	if !strings.Contains(body, "沒有任何群組被確認") {
		t.Fatalf("unexpected title: %s", body)
	}
	if !strings.Contains(body, "1 群在你送出前已被確認過") {
		t.Errorf("skipped groups must be reported: %s", body)
	}
}

func TestHandlePendingBatch_TooManyGroups(t *testing.T) {
	h, _ := batchTestHandler()
	reps := make([]string, maxBatchGroups+1)
	for i := range reps {
		reps[i] = fmt.Sprint(i + 1)
	}
	w := postBatch(h, url.Values{"rep": reps, "resolution": {"x"}})
	if !strings.Contains(w.Body.String(), "群組太多") {
		t.Fatalf("over-limit submission should be rejected: %s", w.Body.String())
	}
}

// Partial failure must not read as success: someone who submitted two
// groups and had one fail would otherwise believe the list is clear.
func TestBatchMessage_ReportsEveryFailureMode(t *testing.T) {
	msg := batchMessage(2, 38, 1, 1, []string{"#7 確認失敗"})
	for _, want := range []string{
		"2 群", "38 筆", "36 筆標記為同群代表列的重複",
		"1 群在你送出前已被確認過", "1 個送上來的 id 無法解析", "#7 確認失敗",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}

// Every group being a single row means there are no duplicates — saying
// "0 筆標記為重複" would be noise.
func TestBatchMessage_NoDuplicateNoteForSingletons(t *testing.T) {
	msg := batchMessage(2, 2, 0, 0, nil)
	if strings.Contains(msg, "重複") {
		t.Errorf("singleton groups should not mention duplicates: %s", msg)
	}
}

// "35 firings in two minutes" and "35 firings over nine days" both render
// as ×35 and need different responses, so the span has to be legible.
func TestSpanText(t *testing.T) {
	base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		g    rag.PendingGroup
		want string
	}{
		{"burst", rag.PendingGroup{Size: 16, NewestAt: base.Add(61 * time.Second), OldestAt: base}, "分鐘內"},
		{"days", rag.PendingGroup{Size: 35, NewestAt: base.Add(9 * 24 * time.Hour), OldestAt: base}, "橫跨 9 天"},
		{"singleton", rag.PendingGroup{Size: 1, NewestAt: base, OldestAt: base}, ""},
	}
	for _, c := range cases {
		if got := spanText(c.g); !strings.Contains(got, c.want) || (c.want == "" && got != "") {
			t.Errorf("%s: spanText = %q, want to contain %q", c.name, got, c.want)
		}
	}
}

// The list must show both numbers. Only showing groups would make the
// backlog look smaller than it is.
func TestHandlePendingList_ShowsRowsAndGroups(t *testing.T) {
	h, _ := batchTestHandler()
	w := httptest.NewRecorder()
	h.handlePendingList(w, httptest.NewRequest(http.MethodGet, "/pending", nil))
	body := w.Body.String()
	if !strings.Contains(body, "4 筆") || !strings.Contains(body, "2 群") {
		t.Errorf("header should show rows and groups: %s", body)
	}
	if !strings.Contains(body, `×3`) {
		t.Errorf("a 3-row group must render its size: %s", body)
	}
	if !strings.Contains(body, `action="/pending/batch"`) {
		t.Errorf("batch form should be present when enabled: %s", body)
	}
	if !strings.Contains(body, "沒有逐筆看過") {
		t.Errorf("the page must say the other rows in a group weren't individually reviewed")
	}
}

// Without the migration the list still works — it just can't offer batch
// confirm, and says why.
func TestHandlePendingList_WithoutMigrationStillLists(t *testing.T) {
	h, _ := batchTestHandler()
	h.batchConfirm = false
	w := httptest.NewRecorder()
	h.handlePendingList(w, httptest.NewRequest(http.MethodGet, "/pending", nil))
	body := w.Body.String()
	if strings.Contains(body, `action="/pending/batch"`) {
		t.Error("batch form must not render when dup_of is missing")
	}
	if !strings.Contains(body, "migrate_0003") {
		t.Errorf("should tell the operator how to enable it: %s", body)
	}
	if !strings.Contains(body, "SSLCertExpiringSoon") {
		t.Error("the list itself must still work")
	}
}
