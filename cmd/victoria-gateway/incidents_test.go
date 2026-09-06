package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

func TestHandleIncidentsList_NoFilter_ReturnsAllRecords(t *testing.T) {
	store := &fakeRAGStore{records: []rag.Record{
		{ID: 1, AlertName: "InstanceDown", Host: "172.16.100.6", Resolution: "r1", CreatedAt: time.Now(), ConfirmedAt: time.Now()},
		{ID: 2, AlertName: "DiskSpaceWarning", Host: "172.16.100.7", Resolution: "r2", CreatedAt: time.Now(), ConfirmedAt: time.Now()},
	}}
	h := &handler{rag: store}

	req := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	rec := httptest.NewRecorder()
	h.handleIncidentsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "InstanceDown") || !strings.Contains(body, "DiskSpaceWarning") {
		t.Errorf("expected both records in unfiltered list, got body: %s", body)
	}
}

func TestHandleIncidentsList_FilterByAlertName(t *testing.T) {
	store := &fakeRAGStore{records: []rag.Record{
		{ID: 1, AlertName: "InstanceDown", Host: "172.16.100.6", Resolution: "r1", CreatedAt: time.Now(), ConfirmedAt: time.Now()},
		{ID: 2, AlertName: "DiskSpaceWarning", Host: "172.16.100.7", Resolution: "r2", CreatedAt: time.Now(), ConfirmedAt: time.Now()},
	}}
	h := &handler{rag: store}

	req := httptest.NewRequest(http.MethodGet, "/incidents?alertname=DiskSpace", nil)
	rec := httptest.NewRecorder()
	h.handleIncidentsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "InstanceDown") {
		t.Errorf("expected InstanceDown to be filtered out, got body: %s", body)
	}
	if !strings.Contains(body, "DiskSpaceWarning") {
		t.Errorf("expected DiskSpaceWarning to remain, got body: %s", body)
	}
	// Sticky form: the submitted filter value should be echoed back into the input.
	if !strings.Contains(body, `value="DiskSpace"`) {
		t.Errorf("expected filter form to be sticky (echo back alertname=DiskSpace), got body: %s", body)
	}
}

func TestHandleIncidentsList_FilterByHost(t *testing.T) {
	store := &fakeRAGStore{records: []rag.Record{
		{ID: 1, AlertName: "InstanceDown", Host: "172.16.100.6", Resolution: "r1", CreatedAt: time.Now(), ConfirmedAt: time.Now()},
		{ID: 2, AlertName: "InstanceDown", Host: "172.16.100.7", Resolution: "r2", CreatedAt: time.Now(), ConfirmedAt: time.Now()},
	}}
	h := &handler{rag: store}

	req := httptest.NewRequest(http.MethodGet, "/incidents?host=100.7", nil)
	rec := httptest.NewRecorder()
	h.handleIncidentsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "172.16.100.7") {
		t.Errorf("expected 172.16.100.7 record to remain, got body: %s", body)
	}
	if strings.Contains(body, `/incidents/1"`) {
		t.Errorf("expected record 1 (172.16.100.6) to be filtered out, got body: %s", body)
	}
}

func TestHandleIncidentsList_NoMatch_ShowsEmptyState(t *testing.T) {
	store := &fakeRAGStore{records: []rag.Record{
		{ID: 1, AlertName: "InstanceDown", Host: "172.16.100.6", Resolution: "r1", CreatedAt: time.Now(), ConfirmedAt: time.Now()},
	}}
	h := &handler{rag: store}

	req := httptest.NewRequest(http.MethodGet, "/incidents?alertname=NoSuchAlert", nil)
	rec := httptest.NewRecorder()
	h.handleIncidentsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "InstanceDown") {
		t.Errorf("expected no records to match, got body: %s", rec.Body.String())
	}
}
