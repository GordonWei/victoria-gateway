package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

func TestHandlePendingList_ShowsPendingNotConfirmed(t *testing.T) {
	store := &fakeRAGStore{
		records: []rag.Record{{ID: 1, AlertName: "ConfirmedOne", Host: "h1"}},
		pending: []rag.Record{{ID: 2, AlertName: "PendingOne", Host: "h2", Summary: "looks like a blip", CreatedAt: time.Now()}},
	}
	h := &handler{rag: store}

	req := httptest.NewRequest(http.MethodGet, "/pending", nil)
	rec := httptest.NewRecorder()
	h.handlePendingList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "PendingOne") {
		t.Errorf("expected pending record in list, got body: %s", body)
	}
	if strings.Contains(body, "ConfirmedOne") {
		t.Errorf("confirmed records must never appear on the pending list, got body: %s", body)
	}
}

func TestHandlePendingDetail_GetShowsConfirmForm(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{
		{ID: 5, AlertName: "DiskSpace", Host: "h1", Summary: "disk is filling up", CreatedAt: time.Now()},
	}}
	h := &handler{rag: store}

	req := httptest.NewRequest(http.MethodGet, "/pending/5", nil)
	rec := httptest.NewRecorder()
	h.handlePendingDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/pending/5"`) {
		t.Errorf("expected a confirm form posting back to /pending/5, got body: %s", body)
	}
}

func TestHandlePendingDetail_Post_ConfirmsAndOffersWayBack(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{
		{ID: 7, AlertName: "DiskSpace", Host: "h1", Summary: "disk is filling up", CreatedAt: time.Now()},
	}}
	h := &handler{rag: store}

	form := url.Values{"resolution": {"log rotation was misconfigured; fixed and cleared"}}
	req := httptest.NewRequest(http.MethodPost, "/pending/7", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handlePendingDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if len(store.confirmPendingCalls) != 1 {
		t.Fatalf("ConfirmPending called %d times, want 1", len(store.confirmPendingCalls))
	}
	if got := store.confirmPendingCalls[0]; got.ID != 7 || got.Resolution != "log rotation was misconfigured; fixed and cleared" {
		t.Errorf("ConfirmPending called with %+v", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/pending"`) {
		t.Errorf("confirming must not be a dead end — expected a link back to /pending, got body: %s", body)
	}
	if strings.Contains(body, `<form class="confirm"`) {
		t.Errorf("a record just confirmed shouldn't still show a confirm form, got body: %s", body)
	}
}

func TestHandlePendingDetail_DoublePost_SecondAttemptDoesNotOverwrite(t *testing.T) {
	store := &fakeRAGStore{pending: []rag.Record{
		{ID: 9, AlertName: "DiskSpace", Host: "h1", CreatedAt: time.Now()},
	}}
	h := &handler{rag: store}

	post := func(resolution string) *httptest.ResponseRecorder {
		form := url.Values{"resolution": {resolution}}
		req := httptest.NewRequest(http.MethodPost, "/pending/9", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.handlePendingDetail(rec, req)
		return rec
	}

	first := post("first resolution wins")
	if first.Code != http.StatusOK {
		t.Fatalf("first post status = %d, want 200, body: %s", first.Code, first.Body.String())
	}
	second := post("second submission should not land")
	if second.Code != http.StatusOK {
		t.Fatalf("second post status = %d, want 200, body: %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "已經被別人確認過了") {
		t.Errorf("second post should render the already-confirmed message, got body: %s", second.Body.String())
	}
	if len(store.confirmPendingCalls) != 1 {
		t.Fatalf("ConfirmPending should only have taken effect once, got %d calls: %+v", len(store.confirmPendingCalls), store.confirmPendingCalls)
	}
}

func TestHandlePendingDetail_UnknownID_404s(t *testing.T) {
	store := &fakeRAGStore{}
	h := &handler{rag: store}

	req := httptest.NewRequest(http.MethodGet, "/pending/999", nil)
	rec := httptest.NewRecorder()
	h.handlePendingDetail(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWebUIAuthMiddleware_NilConfig_AllowsThrough(t *testing.T) {
	called := false
	h := webUIAuthMiddleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Error("expected the wrapped handler to run when WebUIAuth is nil (default, unauthenticated)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestWebUIAuthMiddleware_RejectsMissingOrWrongCredentials(t *testing.T) {
	auth := &config.WebhookAuthConfig{Username: "op", Password: "s3cret"}
	called := false
	h := webUIAuthMiddleware(auth)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Error("handler must not run without credentials")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	req2.SetBasicAuth("op", "wrong")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for wrong password", rec2.Code)
	}
}

func TestWebUIAuthMiddleware_AllowsCorrectCredentials(t *testing.T) {
	auth := &config.WebhookAuthConfig{Username: "op", Password: "s3cret"}
	called := false
	h := webUIAuthMiddleware(auth)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	req := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	req.SetBasicAuth("op", "s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Error("expected the wrapped handler to run with correct credentials")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
