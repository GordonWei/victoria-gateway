package config

import (
	"fmt"
	"regexp"
	"strings"
)

// AlertSourcesConfig enables the non-Alertmanager webhook endpoints. See
// Config.AlertSources.
type AlertSourcesConfig struct {
	// Grafana enables POST /webhook/grafana.
	Grafana *GrafanaSourceConfig `yaml:"grafana"`
	// Generic adds one POST /webhook/generic/{name} endpoint per entry.
	Generic []GenericSourceConfig `yaml:"generic"`
}

// GrafanaSourceConfig turns on the Grafana Alerting webhook endpoint. It
// has no options beyond Enabled: Grafana's payload carries the same
// labels/annotations/fingerprint shape Alertmanager's does.
type GrafanaSourceConfig struct {
	Enabled bool `yaml:"enabled"`
}

// GenericSourceConfig describes how to read alerts out of an arbitrary
// JSON body. Every path is a dot-separated walk from the alert object
// (or from the body root when AlertsPath is empty), with a leading "$."
// allowed and numeric segments indexing arrays: "host.name",
// "$.tags.0.value".
type GenericSourceConfig struct {
	// Name is the {name} in /webhook/generic/{name}: lowercase letters,
	// digits, '-' and '_'.
	Name string `yaml:"name"`
	// AlertsPath points at an array of alert objects in the body. Empty
	// means the whole body is a single alert.
	AlertsPath string             `yaml:"alerts_path"`
	Fields     GenericSourceField `yaml:"fields"`
	// StatusMap translates the raw value at Fields.Status into "firing"
	// or "resolved" (e.g. PROBLEM: firing, OK: resolved). A raw value that
	// is already "firing" or "resolved" needs no entry.
	StatusMap map[string]string `yaml:"status_map"`
	// StartsAtFormat says how to read Fields.StartsAt: "rfc3339" (the
	// default), "unix", "unix_ms", or a Go time layout such as
	// "2006.01.02 15:04:05".
	StartsAtFormat string `yaml:"starts_at_format"`
	// Labels maps label name -> path. These are what log lookup and
	// routing key on, so map whatever identifies the affected machine to
	// "host" (or "namespace" plus "pod" for Kubernetes).
	Labels map[string]string `yaml:"labels"`
	// Annotations maps annotation name -> path ("summary", "description"
	// are the ones the prompt uses).
	Annotations map[string]string `yaml:"annotations"`
	// StaticLabels are added to every alert from this source, e.g.
	// {source: zabbix} so routes can match on it.
	StaticLabels map[string]string `yaml:"static_labels"`
}

// GenericSourceField holds the paths for the fields every alert has.
type GenericSourceField struct {
	AlertName   string `yaml:"alertname"`   // required
	Status      string `yaml:"status"`      // optional: unset means every alert is firing
	StartsAt    string `yaml:"starts_at"`   // optional: unset means the time the webhook arrived
	Fingerprint string `yaml:"fingerprint"` // optional: unset means a hash of the label set; set but missing in the body is an error. The source name is always prefixed (generic:<name>:<value>)
}

var genericSourceName = regexp.MustCompile(`^[a-z0-9_-]+$`)

func (a *AlertSourcesConfig) validate() error {
	if a == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, g := range a.Generic {
		label := fmt.Sprintf("alert_sources.generic[%d]", i)
		if !genericSourceName.MatchString(g.Name) {
			return fmt.Errorf("%s.name %q must be non-empty lowercase letters, digits, '-' or '_' (it becomes the URL path /webhook/generic/{name})", label, g.Name)
		}
		if seen[g.Name] {
			return fmt.Errorf("%s.name %q is used twice", label, g.Name)
		}
		seen[g.Name] = true
		if strings.TrimSpace(g.Fields.AlertName) == "" {
			return fmt.Errorf("%s.fields.alertname is required", label)
		}
		for raw, v := range g.StatusMap {
			if v != "firing" && v != "resolved" {
				return fmt.Errorf(`%s.status_map[%q] = %q; values must be "firing" or "resolved"`, label, raw, v)
			}
		}
		switch f := g.StartsAtFormat; f {
		case "", "rfc3339", "unix", "unix_ms":
		default:
			if !strings.Contains(f, "2006") {
				return fmt.Errorf("%s.starts_at_format %q is not rfc3339, unix, unix_ms or a Go time layout (a layout spells the year as 2006)", label, f)
			}
		}
		if len(g.StatusMap) > 0 && strings.TrimSpace(g.Fields.Status) == "" {
			return fmt.Errorf("%s.status_map is set but fields.status is not, so every alert would be treated as firing", label)
		}
		if g.StartsAtFormat != "" && strings.TrimSpace(g.Fields.StartsAt) == "" {
			return fmt.Errorf("%s.starts_at_format is set but fields.starts_at is not, so the format would never be used", label)
		}
		for name, path := range g.Labels {
			if name == "" || strings.TrimSpace(path) == "" {
				return fmt.Errorf("%s.labels has an empty name or path", label)
			}
			if name == "alertname" {
				return fmt.Errorf("%s.labels.alertname: set fields.alertname instead", label)
			}
		}
		for name, path := range g.Annotations {
			if name == "" || strings.TrimSpace(path) == "" {
				return fmt.Errorf("%s.annotations has an empty name or path", label)
			}
		}
	}
	return nil
}
