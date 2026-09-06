package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/maintenance"
)

func newTestHandlerWithWindows(defs []config.MaintenanceWindow) *handler {
	windows, err := maintenance.ParseWindows(defs)
	if err != nil {
		panic(err) // test setup only; a bad literal here is a test bug
	}
	return &handler{
		maintenanceWindows:    windows,
		maintenanceWindowDefs: defs,
	}
}

func TestHandleMaintenanceWindows_Get_ReturnsCurrentDefs(t *testing.T) {
	defs := []config.MaintenanceWindow{
		{Name: "weekly", Schedule: "SAT 02:00-04:00", Matchers: map[string]string{"host": "*"}, Action: "suppress"},
	}
	h := newTestHandlerWithWindows(defs)

	req := httptest.NewRequest(http.MethodGet, "/maintenance-windows", nil)
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		MaintenanceWindows []config.MaintenanceWindow `json:"maintenance_windows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.MaintenanceWindows) != 1 || got.MaintenanceWindows[0].Name != "weekly" {
		t.Errorf("got %+v, want 1 window named weekly", got.MaintenanceWindows)
	}
}

func TestHandleMaintenanceWindows_Put_ReplacesWindows(t *testing.T) {
	h := newTestHandlerWithWindows([]config.MaintenanceWindow{
		{Name: "old", Schedule: "SAT 02:00-04:00", Matchers: map[string]string{"host": "*"}, Action: "suppress"},
	})

	newDefs := []config.MaintenanceWindow{
		{Name: "new-1", Schedule: "DAILY 03:00-03:30", Matchers: map[string]string{"host": "*"}, Action: "mute"},
		{Name: "new-2", Schedule: "SUN 01:00-02:00", Matchers: map[string]string{"severity": "warning"}, Action: "suppress"},
	}
	body, _ := json.Marshal(newDefs)
	req := httptest.NewRequest(http.MethodPut, "/maintenance-windows", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	// Verify via a follow-up GET that the replacement actually took
	// effect, rather than just trusting the PUT response body.
	getReq := httptest.NewRequest(http.MethodGet, "/maintenance-windows", nil)
	getRec := httptest.NewRecorder()
	h.handleMaintenanceWindows(getRec, getReq)
	var got struct {
		MaintenanceWindows []config.MaintenanceWindow `json:"maintenance_windows"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(got.MaintenanceWindows) != 2 || got.MaintenanceWindows[0].Name != "new-1" || got.MaintenanceWindows[1].Name != "new-2" {
		t.Errorf("got %+v, want the two new windows", got.MaintenanceWindows)
	}

	// And the parsed windows actually used by summarizeOne must also
	// have been swapped, not just the raw defs.
	if len(h.currentMaintenanceWindows()) != 2 {
		t.Errorf("currentMaintenanceWindows() has %d entries, want 2", len(h.currentMaintenanceWindows()))
	}
}

func TestHandleMaintenanceWindows_Put_InvalidBody_LeavesExistingWindowsUnchanged(t *testing.T) {
	original := []config.MaintenanceWindow{
		{Name: "keep-me", Schedule: "SAT 02:00-04:00", Matchers: map[string]string{"host": "*"}, Action: "suppress"},
	}
	h := newTestHandlerWithWindows(original)

	// Empty matchers is invalid (config.ValidateMaintenanceWindows
	// refuses "matches everything" windows), so this PUT must fail
	// without disturbing what's already in effect.
	badDefs := []config.MaintenanceWindow{
		{Name: "bad", Schedule: "SAT 02:00-04:00", Matchers: map[string]string{}, Action: "suppress"},
	}
	body, _ := json.Marshal(badDefs)
	req := httptest.NewRequest(http.MethodPut, "/maintenance-windows", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid matchers", rec.Code)
	}

	h.maintenanceMu.RLock()
	defs := h.maintenanceWindowDefs
	h.maintenanceMu.RUnlock()
	if len(defs) != 1 || defs[0].Name != "keep-me" {
		t.Errorf("existing windows were disturbed by a failed PUT: got %+v", defs)
	}
}

func TestHandleMaintenanceWindows_Put_InvalidAction_Returns400(t *testing.T) {
	h := newTestHandlerWithWindows(nil)

	badDefs := []config.MaintenanceWindow{
		{Name: "bad-action", Schedule: "SAT 02:00-04:00", Matchers: map[string]string{"host": "*"}, Action: "ignore"},
	}
	body, _ := json.Marshal(badDefs)
	req := httptest.NewRequest(http.MethodPut, "/maintenance-windows", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid action", rec.Code)
	}
}

func TestHandleMaintenanceWindows_Put_MalformedJSON_Returns400(t *testing.T) {
	h := newTestHandlerWithWindows(nil)

	req := httptest.NewRequest(http.MethodPut, "/maintenance-windows", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed JSON", rec.Code)
	}
}

func TestHandleMaintenanceWindows_MethodNotAllowed(t *testing.T) {
	h := newTestHandlerWithWindows(nil)

	req := httptest.NewRequest(http.MethodDelete, "/maintenance-windows", nil)
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHandleMaintenanceWindows_AuthConfigured_RejectsMissingCreds(t *testing.T) {
	h := newTestHandlerWithWindows(nil)
	h.webhookAuth = &config.WebhookAuthConfig{Username: "admin", Password: "s3cret"}

	req := httptest.NewRequest(http.MethodGet, "/maintenance-windows", nil)
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleMaintenanceWindows_AuthConfigured_AcceptsCorrectCreds(t *testing.T) {
	h := newTestHandlerWithWindows(nil)
	h.webhookAuth = &config.WebhookAuthConfig{Username: "admin", Password: "s3cret"}

	req := httptest.NewRequest(http.MethodGet, "/maintenance-windows", nil)
	req.SetBasicAuth("admin", "s3cret")
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHandleMaintenanceWindows_NoAuthConfigured_AnyoneCanCall(t *testing.T) {
	h := newTestHandlerWithWindows(nil) // h.webhookAuth left nil

	req := httptest.NewRequest(http.MethodGet, "/maintenance-windows", nil)
	rec := httptest.NewRecorder()
	h.handleMaintenanceWindows(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no auth configured = no auth required)", rec.Code)
	}
}
