// Package alertsource converts alert webhooks from sources other than
// Alertmanager into the aiops.WebhookPayload the gateway already
// processes. Each parser only translates; dedup, log lookup, routing and
// notifications stay in one place and behave the same whatever sent the
// alert.
package alertsource

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
)

// grafanaPayload is the body Grafana Alerting's webhook contact point
// POSTs. Field set from the official documentation,
// https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/
// (checked 2026-11-01). It extends Alertmanager's webhook body with
// orgId, state, title and message at the top level, and values,
// silenceURL, dashboardURL, panelURL and imageURL per alert. Its
// version is "1", unrelated to Alertmanager's "4".
type grafanaPayload struct {
	Receiver          string            `json:"receiver"`
	Status            string            `json:"status"`
	OrgID             int64             `json:"orgId"`
	Alerts            []grafanaAlert    `json:"alerts"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	State             string            `json:"state"`
	Title             string            `json:"title"`
	Message           string            `json:"message"`
}

type grafanaAlert struct {
	Status       string                     `json:"status"`
	Labels       map[string]string          `json:"labels"`
	Annotations  map[string]string          `json:"annotations"`
	StartsAt     string                     `json:"startsAt"`
	EndsAt       string                     `json:"endsAt"`
	Values       map[string]json.RawMessage `json:"values"`
	GeneratorURL string                     `json:"generatorURL"`
	Fingerprint  string                     `json:"fingerprint"`
	SilenceURL   string                     `json:"silenceURL"`
	DashboardURL string                     `json:"dashboardURL"`
	PanelURL     string                     `json:"panelURL"`
	ImageURL     string                     `json:"imageURL"`
}

// Annotation keys the Grafana-only fields are copied into. Prefixed so
// they can't collide with an annotation a rule author set.
const (
	AnnotationGrafanaValues    = "grafana_values"
	AnnotationGrafanaDashboard = "grafana_dashboard_url"
	AnnotationGrafanaPanel     = "grafana_panel_url"
	AnnotationGrafanaSilence   = "grafana_silence_url"
	AnnotationGrafanaImage     = "grafana_image_url"
)

// ParseGrafana converts a Grafana Alerting webhook body. Labels,
// annotations, status, timestamps, generatorURL and fingerprint carry
// over unchanged; values and the extra URLs become grafana_*
// annotations. Like the Alertmanager path, an alert without a host or
// Kubernetes identity label is not rejected here — it fails per alert
// later, with the same error result an Alertmanager alert would get.
func ParseGrafana(body []byte) (*aiops.WebhookPayload, error) {
	var g grafanaPayload
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("grafana webhook: unmarshal: %w", err)
	}
	if len(g.Alerts) == 0 {
		return nil, fmt.Errorf("grafana webhook: payload contains zero alerts")
	}
	out := &aiops.WebhookPayload{
		Version:           "grafana/" + g.Version,
		GroupKey:          g.GroupKey,
		TruncatedAlerts:   g.TruncatedAlerts,
		Status:            g.Status,
		Receiver:          g.Receiver,
		GroupLabels:       g.GroupLabels,
		CommonLabels:      g.CommonLabels,
		CommonAnnotations: g.CommonAnnotations,
		ExternalURL:       g.ExternalURL,
	}
	for i, a := range g.Alerts {
		if a.Status != "firing" && a.Status != "resolved" {
			return nil, fmt.Errorf("grafana webhook: alerts[%d].status %q is not firing or resolved", i, a.Status)
		}
		ann := make(map[string]string, len(a.Annotations)+5)
		for k, v := range a.Annotations {
			ann[k] = v
		}
		if v := formatValues(a.Values); v != "" {
			ann[AnnotationGrafanaValues] = v
		}
		for key, val := range map[string]string{
			AnnotationGrafanaDashboard: a.DashboardURL,
			AnnotationGrafanaPanel:     a.PanelURL,
			AnnotationGrafanaSilence:   a.SilenceURL,
			AnnotationGrafanaImage:     a.ImageURL,
		} {
			if val != "" {
				ann[key] = val
			}
		}
		labels := a.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		out.Alerts = append(out.Alerts, aiops.Alert{
			Status:       a.Status,
			Labels:       labels,
			Annotations:  ann,
			StartsAt:     a.StartsAt,
			EndsAt:       a.EndsAt,
			GeneratorURL: a.GeneratorURL,
			Fingerprint:  a.Fingerprint,
		})
	}
	return out, nil
}

// formatValues renders Grafana's per-alert values ({"B": 44.2, "C": 1})
// as "B=44.2, C=1", sorted by key so the same alert always produces the
// same annotation text.
func formatValues(values map[string]json.RawMessage) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		raw := strings.TrimSpace(string(values[k]))
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			raw = strconv.FormatFloat(f, 'g', 6, 64)
		}
		parts = append(parts, k+"="+raw)
	}
	return strings.Join(parts, ", ")
}
