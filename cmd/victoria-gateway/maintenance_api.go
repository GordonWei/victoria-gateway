// maintenance_api.go serves GET/PUT /maintenance-windows — an admin API
// to inspect and hot-replace the maintenance window set without a
// restart. Before this existed, changing maintenance_windows in
// config.yaml required a redeploy; an operator adding a one-off window
// for tonight's patching run shouldn't have to rebuild and restart the
// service to do it.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/maintenance"
)

// handleMaintenanceWindows serves GET (read the currently active
// windows) and PUT (replace them) on /maintenance-windows. Gated by the
// same webhook_auth Basic Auth as POST /webhook/alertmanager when
// configured — this endpoint can suppress or mute alert delivery, which
// is at least as sensitive as triggering an analysis.
func (h *handler) handleMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	if !h.checkWebhookAuth(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="victoria-gateway"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.handleGetMaintenanceWindows(w, r)
	case http.MethodPut:
		h.handlePutMaintenanceWindows(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *handler) handleGetMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	h.maintenanceMu.RLock()
	defs := h.maintenanceWindowDefs
	h.maintenanceMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		MaintenanceWindows []config.MaintenanceWindow `json:"maintenance_windows"`
	}{MaintenanceWindows: defs}); err != nil {
		log.Printf("maintenance-windows: encode GET response: %v", err)
	}
}

// handlePutMaintenanceWindows replaces the entire maintenance window
// set. All-or-nothing: the body is decoded, validated, and parsed before
// anything about the live handler state is touched, so a bad PUT leaves
// whatever was in effect before completely unchanged rather than
// half-applied.
func (h *handler) handlePutMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MiB is generously more than any real window set
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	var defs []config.MaintenanceWindow
	if err := json.Unmarshal(body, &defs); err != nil {
		http.Error(w, "invalid JSON body (expected an array of maintenance window objects): "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := config.ValidateMaintenanceWindows(defs); err != nil {
		http.Error(w, "validation failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	windows, err := maintenance.ParseWindows(defs)
	if err != nil {
		// Shouldn't normally happen — ValidateMaintenanceWindows already
		// checks the schedule/timezone shapes ParseWindows would reject —
		// but the two functions are separately maintained, so treat a
		// disagreement between them as the caller's input being bad
		// rather than a 500: nothing about the live state has changed.
		http.Error(w, "parse failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.maintenanceMu.Lock()
	h.maintenanceWindowDefs = defs
	h.maintenanceWindows = windows
	h.maintenanceMu.Unlock()

	log.Printf("maintenance-windows: replaced via PUT, now %d window(s)", len(windows))

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}{Status: "ok", Count: len(windows)}); err != nil {
		log.Printf("maintenance-windows: encode PUT response: %v", err)
	}
}
