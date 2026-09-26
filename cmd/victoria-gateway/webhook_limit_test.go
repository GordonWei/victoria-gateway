package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleAlertmanagerWebhook_BodyTooLarge(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")
	big := bytes.Repeat([]byte("a"), maxWebhookBodyBytes+1)
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}

	// Exactly at the cap is still read (and rejected only as bad JSON).
	rec = httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(big[:maxWebhookBodyBytes])))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status at the cap = %d, want 400 (parsed, not refused)", rec.Code)
	}
}
