package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

func giteaSignature(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func githubSignature(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGiteaSignature(t *testing.T) {
	body := []byte(`{"action":"closed"}`)
	valid := giteaSignature(t, "s3cret", body)

	if !verifyGiteaSignature("s3cret", body, valid) {
		t.Error("expected a correctly computed signature to verify")
	}
	if verifyGiteaSignature("s3cret", body, "") {
		t.Error("empty signature header must not verify")
	}
	if verifyGiteaSignature("s3cret", body, "deadbeef") {
		t.Error("wrong signature must not verify")
	}
	if verifyGiteaSignature("wrong-secret", body, valid) {
		t.Error("signature computed with a different secret must not verify")
	}
}

func TestVerifyGitHubSignature(t *testing.T) {
	body := []byte(`{"action":"closed"}`)
	valid := githubSignature(t, "s3cret", body)

	if !verifyGitHubSignature("s3cret", body, valid) {
		t.Error("expected a correctly computed signature to verify")
	}
	if verifyGitHubSignature("s3cret", body, "") {
		t.Error("empty signature header must not verify")
	}
	if verifyGitHubSignature("s3cret", body, strings.TrimPrefix(valid, "sha256=")) {
		t.Error("a signature missing the sha256= prefix must not verify")
	}
	if verifyGitHubSignature("wrong-secret", body, valid) {
		t.Error("signature computed with a different secret must not verify")
	}
}

func TestResyncIssue_NoTrackerConfigured(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}}
	_, err := h.resyncIssue(context.Background(), 1)
	if err == nil {
		t.Fatal("expected an error when no tracker is configured")
	}
}

func TestResyncIssue_NoPendingRecordLinked_IsBenign(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}, tracker: &fakeTracker{}}
	result, err := h.resyncIssue(context.Background(), 99)
	if err != nil {
		t.Fatalf("expected no error for an issue with no linked pending record, got %v", err)
	}
	if !strings.Contains(result, "no pending record") {
		t.Errorf("expected an explanatory benign result, got %q", result)
	}
}

func TestResyncIssue_ClosedWithNoComment_LeftPending(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{{ID: 1, GiteaIssueNumber: 5}}}
	tr := &fakeTracker{} // LastComment returns "" for an unset issue
	h := &handler{rag: store, tracker: tr}

	result, err := h.resyncIssue(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "no comment") {
		t.Errorf("expected the no-comment outcome, got %q", result)
	}
	if len(store.confirmPendingCalls) != 0 {
		t.Errorf("ConfirmPending must not be called with no comment to use as a resolution")
	}
}

func TestResyncIssue_ClosedWithComment_Confirms(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{{ID: 1, GiteaIssueNumber: 5}}}
	tr := &fakeTracker{comments: map[int64]string{5: "root cause: disk full, cleared logs"}}
	h := &handler{rag: store, tracker: tr}

	result, err := h.resyncIssue(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "confirmed id=1") {
		t.Errorf("expected a confirmed result, got %q", result)
	}
	if len(store.confirmPendingCalls) != 1 || store.confirmPendingCalls[0].Resolution != "root cause: disk full, cleared logs" {
		t.Errorf("expected ConfirmPending called with the issue's last comment, got %+v", store.confirmPendingCalls)
	}
}

func TestResyncIssue_AlreadyConfirmed_IsBenign(t *testing.T) {
	// No matching pending row (already moved to confirmed) but the fake
	// still needs to route the lookup through GetPendingByGiteaIssue, so
	// wire a store that returns ErrAlreadyConfirmed for the eventual
	// ConfirmPending call by leaving pending empty — GetPendingByGiteaIssue
	// itself would then return ErrNotFound, which is a different (also
	// benign) path. To exercise ConfirmPending's ErrAlreadyConfirmed
	// branch specifically, use a store whose pending row exists (so the
	// lookup succeeds) but whose ConfirmPending always reports it as
	// already confirmed.
	store := &alreadyConfirmedRAGStore{fakeRAGStore: fakeRAGStore{pending: []rag.Record{{ID: 1, GiteaIssueNumber: 5}}}}
	tr := &fakeTracker{comments: map[int64]string{5: "fixed"}}
	h := &handler{rag: store, tracker: tr}

	result, err := h.resyncIssue(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "already confirmed") {
		t.Errorf("expected the already-confirmed outcome, got %q", result)
	}
}

// alreadyConfirmedRAGStore overrides ConfirmPending to always report
// rag.ErrAlreadyConfirmed, simulating the race where a webhook fires for
// an issue the web form's own CloseWithComment just closed — the DB
// confirmation already landed by the time the webhook's resync runs.
type alreadyConfirmedRAGStore struct {
	fakeRAGStore
}

func (s *alreadyConfirmedRAGStore) ConfirmPending(ctx context.Context, id int64, resolution string) error {
	return rag.ErrAlreadyConfirmed
}

func postGiteaWebhook(h *handler, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/gitea-issues", strings.NewReader(string(body)))
	if sig != "" {
		req.Header.Set("X-Gitea-Signature", sig)
	}
	rec := httptest.NewRecorder()
	h.handleGiteaIssueWebhook(rec, req)
	return rec
}

func TestHandleGiteaIssueWebhook_NoSecretConfigured_404s(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}}
	rec := postGiteaWebhook(h, []byte(`{}`), "irrelevant")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when webhook_secret isn't configured", rec.Code)
	}
}

func TestHandleGiteaIssueWebhook_BadSignature_Rejected(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}, giteaWebhookSecret: "s3cret"}
	rec := postGiteaWebhook(h, []byte(`{"action":"closed","issue":{"number":5}}`), "deadbeef")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a bad signature", rec.Code)
	}
}

func TestHandleGiteaIssueWebhook_NonClosedAction_Ignored(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{{ID: 1, GiteaIssueNumber: 5}}}
	h := &handler{rag: store, giteaWebhookSecret: "s3cret", tracker: &fakeTracker{}}
	body := []byte(`{"action":"opened","issue":{"number":5}}`)
	rec := postGiteaWebhook(h, body, giteaSignature(t, "s3cret", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if len(store.confirmPendingCalls) != 0 {
		t.Errorf("a non-closed action must not trigger a resync, got calls: %+v", store.confirmPendingCalls)
	}
}

func TestHandleGiteaIssueWebhook_ClosedWithComment_ConfirmsEndToEnd(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{{ID: 1, GiteaIssueNumber: 5}}}
	tr := &fakeTracker{comments: map[int64]string{5: "root cause found, resolved"}}
	h := &handler{rag: store, giteaWebhookSecret: "s3cret", tracker: tr}
	body := []byte(`{"action":"closed","issue":{"number":5}}`)
	rec := postGiteaWebhook(h, body, giteaSignature(t, "s3cret", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if len(store.confirmPendingCalls) != 1 || store.confirmPendingCalls[0].ID != 1 {
		t.Errorf("expected the linked pending record to be confirmed, got calls: %+v", store.confirmPendingCalls)
	}
}

func TestHandleGiteaIssueWebhook_ResyncError_500s(t *testing.T) {
	// No tracker configured makes resyncIssue return an error even though
	// the signature and payload are both valid.
	h := &handler{rag: &fakeRAGStore{}, giteaWebhookSecret: "s3cret"}
	body := []byte(`{"action":"closed","issue":{"number":5}}`)
	rec := postGiteaWebhook(h, body, giteaSignature(t, "s3cret", body))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when resyncIssue itself errors", rec.Code)
	}
}

func postGitHubWebhook(h *handler, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/github-issues", strings.NewReader(string(body)))
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	h.handleGitHubIssueWebhook(rec, req)
	return rec
}

func TestHandleGitHubIssueWebhook_NoSecretConfigured_404s(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}}
	rec := postGitHubWebhook(h, []byte(`{}`), "irrelevant")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when webhook_secret isn't configured", rec.Code)
	}
}

func TestHandleGitHubIssueWebhook_BadSignature_Rejected(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}, githubWebhookSecret: "s3cret"}
	rec := postGitHubWebhook(h, []byte(`{"action":"closed","issue":{"number":5}}`), "sha256=deadbeef")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a bad signature", rec.Code)
	}
}

func TestHandleGitHubIssueWebhook_ClosedWithComment_ConfirmsEndToEnd(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{{ID: 1, GiteaIssueNumber: 5}}}
	tr := &fakeTracker{comments: map[int64]string{5: "root cause found, resolved"}}
	h := &handler{rag: store, githubWebhookSecret: "s3cret", tracker: tr}
	body := []byte(`{"action":"closed","issue":{"number":5}}`)
	rec := postGitHubWebhook(h, body, githubSignature(t, "s3cret", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if len(store.confirmPendingCalls) != 1 {
		t.Errorf("expected the linked pending record to be confirmed, got calls: %+v", store.confirmPendingCalls)
	}
}
