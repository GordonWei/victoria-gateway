// Package aiops implements the AIOps hub: Alertmanager webhook -> Loki log
// fetch -> local LLM summary. See Cowork/docs/_draft_orch_aiops_hub_1031_plan.md
// for the overall design and docs/_agent_handoff.md for task assignment.
//
// This file defines the shared contract types used across webhook.go,
// loki.go, and summarize.go. Defining them here (rather than letting each
// file declare its own copy) keeps the three pieces — split across two
// contributors — compiling against one definition instead of drifting.
package aiops

import "time"

// WebhookPayload mirrors the JSON body Alertmanager's webhook receiver POSTs.
// Field set and shapes are the official format documented at
// https://prometheus.io/docs/alerting/latest/configuration/#webhook_config —
// verified against that page on 2026-08-20, not guessed.
type WebhookPayload struct {
	Version            string            `json:"version"`
	GroupKey           string            `json:"groupKey"`
	TruncatedAlerts    int               `json:"truncatedAlerts"`
	Status             string            `json:"status"` // "resolved" | "firing"
	Receiver           string            `json:"receiver"`
	GroupLabels        map[string]string `json:"groupLabels"`
	CommonLabels       map[string]string `json:"commonLabels"`
	CommonAnnotations  map[string]string `json:"commonAnnotations"`
	ExternalURL        string            `json:"externalURL"`
	NotificationReason string            `json:"notification_reason"`
	Alerts             []Alert           `json:"alerts"`
}

// Alert is one entry in WebhookPayload.Alerts.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"` // RFC3339
	EndsAt       string            `json:"endsAt"`   // RFC3339; "0001-01-01T00:00:00Z" while firing
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Host returns the label this alert uses to identify the affected machine.
// Alertmanager rule authors aren't consistent about naming this label
// "instance" vs "host" — this checks both rather than assuming one.
// Returns ok=false if neither is set, so callers can fail loudly instead of
// querying Loki with an empty host filter.
func (a Alert) Host() (host string, ok bool) {
	if v, present := a.Labels["host"]; present && v != "" {
		return v, true
	}
	if v, present := a.Labels["instance"]; present && v != "" {
		return v, true
	}
	return "", false
}

// StartTime parses StartsAt as RFC3339. Returns the zero Time and an error
// if StartsAt is empty or malformed — callers should treat that as fatal
// for building a Loki query window, not silently default to time.Now().
func (a Alert) StartTime() (time.Time, error) {
	return time.Parse(time.RFC3339, a.StartsAt)
}

// LogEntry is one line returned from a Loki query_range call, decoded from
// Loki's `data.result[].values` shape: [["<unix_nanos_string>", "<line>"], ...].
type LogEntry struct {
	Timestamp time.Time
	Line      string
}
