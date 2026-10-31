package config

import (
	"strings"
	"testing"
)

func TestValidate_AlertSources(t *testing.T) {
	good := GenericSourceConfig{Name: "zabbix", Fields: GenericSourceField{AlertName: "trigger.name"}, Labels: map[string]string{"host": "host.name"}}
	cases := map[string]struct {
		mutate func(*GenericSourceConfig)
		second bool
		want   string
	}{
		"ok":               {func(*GenericSourceConfig) {}, false, ""},
		"bad name":         {func(g *GenericSourceConfig) { g.Name = "Zab/bix" }, false, "name"},
		"empty name":       {func(g *GenericSourceConfig) { g.Name = "" }, false, "name"},
		"duplicate":        {func(*GenericSourceConfig) {}, true, "used twice"},
		"no alertname":     {func(g *GenericSourceConfig) { g.Fields.AlertName = " " }, false, "fields.alertname"},
		"bad status value": {func(g *GenericSourceConfig) { g.StatusMap = map[string]string{"OK": "ok"} }, false, "status_map"},
		"bad time format":  {func(g *GenericSourceConfig) { g.StartsAtFormat = "iso" }, false, "starts_at_format"},
		"go layout ok": {func(g *GenericSourceConfig) {
			g.StartsAtFormat = "2006.01.02 15:04:05"
			g.Fields.StartsAt = "event.time"
		}, false, ""},
		"status_map without field": {func(g *GenericSourceConfig) { g.StatusMap = map[string]string{"OK": "resolved"} }, false, "fields.status"},
		"format without field":     {func(g *GenericSourceConfig) { g.StartsAtFormat = "unix" }, false, "fields.starts_at"},
		"status_map with field": {func(g *GenericSourceConfig) {
			g.StatusMap = map[string]string{"OK": "resolved"}
			g.Fields.Status = "event.status"
		}, false, ""},
		"alertname label": {func(g *GenericSourceConfig) { g.Labels = map[string]string{"alertname": "x"} }, false, "fields.alertname"},
		"empty path":      {func(g *GenericSourceConfig) { g.Annotations = map[string]string{"summary": ""} }, false, "annotations"},
	}
	for name, c := range cases {
		g := good
		g.Labels = map[string]string{"host": "host.name"}
		c.mutate(&g)
		cfg := validConfig()
		cfg.AlertSources = &AlertSourcesConfig{Generic: []GenericSourceConfig{g}}
		if c.second {
			cfg.AlertSources.Generic = append(cfg.AlertSources.Generic, g)
		}
		err := cfg.Validate()
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestLoad_AlertSourcesYAML(t *testing.T) {
	cfg := loadYAML(t, `
loki: {endpoint: http://loki.example.com}
summarizer: {endpoint: http://llm.example.com, model: m}
alert_sources:
  grafana: {enabled: true}
  generic:
    - name: zabbix
      fields: {alertname: trigger.name, status: event.status}
      status_map: {PROBLEM: firing, OK: resolved}
      labels: {host: host.name}
`)
	if cfg.AlertSources == nil || !cfg.AlertSources.Grafana.Enabled || len(cfg.AlertSources.Generic) != 1 {
		t.Fatalf("alert_sources not loaded: %+v", cfg.AlertSources)
	}
	if g := cfg.AlertSources.Generic[0]; g.StatusMap["OK"] != "resolved" || g.Labels["host"] != "host.name" {
		t.Errorf("generic fields not loaded: %+v", g)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}
