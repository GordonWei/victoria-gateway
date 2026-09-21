package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/audit"
)

func TestHandleAuditLog_ShowsEntries(t *testing.T) {
	auditLog := &fakeAuditLogger{entries: []audit.Entry{
		{Time: time.Now(), Actor: "alice", Action: "pending.confirm", Target: "id=9", Detail: "fixed the disk"},
	}}
	h := &handler{audit: auditLog}

	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	h.handleAuditLog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice") || !strings.Contains(body, "pending.confirm") {
		t.Errorf("expected the recorded entry in the page, got body: %s", body)
	}
}

func TestHandleAuditLog_NilAudit_DoesNotPanic(t *testing.T) {
	h := &handler{} // audit unset — must not panic, same nil-safe idiom as h.metrics
	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	h.handleAuditLog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
