package alertsource

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixture is the example payload from Grafana's webhook notifier
// documentation, copied as-is (see grafanaPayload's doc comment).
func TestParseGrafana_OfficialExample(t *testing.T) {
	p, err := ParseGrafana(readFixture(t, "grafana_official.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(p.Alerts))
	}
	a := p.Alerts[0]
	if a.Status != "firing" || a.Labels["alertname"] != "High memory usage" || a.Labels["zone"] != "us-1" {
		t.Errorf("labels/status not carried over: %+v", a)
	}
	if a.Fingerprint != "c6eadffa33fcdf37" || a.StartsAt != "2021-10-12T09:51:03.157076+02:00" {
		t.Errorf("fingerprint/startsAt not carried over: %+v", a)
	}
	if a.Annotations["summary"] != "This alert was triggered for zone us-1" {
		t.Errorf("annotations not carried over: %v", a.Annotations)
	}
	if got := a.Annotations[AnnotationGrafanaValues]; got != "B=44.2394, C=1" {
		t.Errorf("grafana_values = %q", got)
	}
	if !strings.HasPrefix(a.Annotations[AnnotationGrafanaSilence], "https://play.grafana.org/alerting/silence/new") {
		t.Errorf("silence URL missing: %v", a.Annotations)
	}
	if _, ok := a.Annotations[AnnotationGrafanaDashboard]; ok {
		t.Errorf("empty dashboardURL should not become an annotation")
	}
	if p.Receiver != "My Super Webhook" || p.CommonLabels["team"] != "blue" {
		t.Errorf("payload fields not carried over: %+v", p)
	}
	if _, _, ok := a.AffectedIdentity(); ok {
		t.Errorf("the official example has no host label; identity should be missing (handled per alert downstream)")
	}
}

func TestParseGrafana_Rejects(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   "nope",
		"no alerts":  `{"version":"1","alerts":[]}`,
		"bad status": `{"version":"1","alerts":[{"status":"pending","labels":{"alertname":"x"}}]}`,
	} {
		if _, err := ParseGrafana([]byte(body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// Same alert sent by Alertmanager and by Grafana must become the same
// internal alert, apart from the grafana_* annotations Grafana adds.
func TestParseGrafana_EquivalentToAlertmanager(t *testing.T) {
	am := `{"version":"4","status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"HighCPU","host":"web-01","severity":"critical"},
	  "annotations":{"summary":"cpu high"},
	  "startsAt":"2026-10-30T06:05:12Z","endsAt":"0001-01-01T00:00:00Z",
	  "generatorURL":"https://example.com/rule","fingerprint":"abc123"}]}`
	gf := `{"version":"1","status":"firing","state":"alerting","orgId":1,"alerts":[{"status":"firing",
	  "labels":{"alertname":"HighCPU","host":"web-01","severity":"critical"},
	  "annotations":{"summary":"cpu high"},
	  "startsAt":"2026-10-30T06:05:12Z","endsAt":"0001-01-01T00:00:00Z",
	  "generatorURL":"https://example.com/rule","fingerprint":"abc123",
	  "values":{"A":97.5},"dashboardURL":"https://example.com/d/1"}]}`
	want, err := aiops.ParseWebhook([]byte(am))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseGrafana([]byte(gf))
	if err != nil {
		t.Fatal(err)
	}
	g := got.Alerts[0]
	for k := range g.Annotations {
		if strings.HasPrefix(k, "grafana_") {
			delete(g.Annotations, k)
		}
	}
	w := want.Alerts[0]
	if g.Status != w.Status || g.StartsAt != w.StartsAt || g.EndsAt != w.EndsAt || g.Fingerprint != w.Fingerprint || g.GeneratorURL != w.GeneratorURL {
		t.Errorf("scalar fields differ:\n got  %+v\n want %+v", g, w)
	}
	if !equalMaps(g.Labels, w.Labels) || !equalMaps(g.Annotations, w.Annotations) {
		t.Errorf("labels/annotations differ:\n got  %+v\n want %+v", g, w)
	}
	gd, _, _ := g.AffectedIdentity()
	wd, _, _ := w.AffectedIdentity()
	if gd != wd || gd != "web-01" {
		t.Errorf("identity differs: %q vs %q", gd, wd)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func zabbixCfg() config.GenericSourceConfig {
	return config.GenericSourceConfig{
		Name: "zabbix",
		Fields: config.GenericSourceField{
			AlertName: "trigger.name", Status: "event.status", StartsAt: "event.time", Fingerprint: "event.id",
		},
		StatusMap:      map[string]string{"PROBLEM": "firing", "OK": "resolved"},
		StartsAtFormat: "2006.01.02 15:04:05",
		Labels:         map[string]string{"host": "host.name", "severity": "trigger.severity"},
		Annotations:    map[string]string{"summary": "trigger.description"},
		StaticLabels:   map[string]string{"source": "zabbix"},
	}
}

func TestGeneric_ZabbixStyleSingleAlert(t *testing.T) {
	p, err := Generic{Cfg: zabbixCfg()}.Parse(readFixture(t, "zabbix_style.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Alerts) != 1 {
		t.Fatalf("got %d alerts", len(p.Alerts))
	}
	a := p.Alerts[0]
	if a.Labels["alertname"] != "High CPU utilization" || a.Labels["host"] != "web-01" || a.Labels["severity"] != "High" || a.Labels["source"] != "zabbix" {
		t.Errorf("labels: %v", a.Labels)
	}
	if a.Status != "firing" || a.Fingerprint != "generic:zabbix:48213" || a.StartsAt != "2026-10-30T14:05:12Z" {
		t.Errorf("status/fingerprint/startsAt: %+v", a)
	}
	if a.Annotations["summary"] != "CPU over 90% for 5m" {
		t.Errorf("annotations: %v", a.Annotations)
	}
	if _, err := a.StartTime(); err != nil {
		t.Errorf("StartsAt must stay RFC3339 for the log window: %v", err)
	}
}

func datadogCfg() config.GenericSourceConfig {
	return config.GenericSourceConfig{
		Name:       "datadog",
		AlertsPath: "$.alerts",
		Fields: config.GenericSourceField{
			AlertName: "title", Status: "transition", StartsAt: "date", Fingerprint: "id",
		},
		StatusMap:      map[string]string{"Triggered": "firing", "Recovered": "resolved"},
		StartsAtFormat: "unix_ms",
		Labels:         map[string]string{"host": "tags.host", "namespace": "tags.kube_namespace", "pod": "tags.pod_name"},
		Annotations:    map[string]string{"description": "body"},
	}
}

func TestGeneric_DatadogStyleMultipleAlerts(t *testing.T) {
	p, err := Generic{Cfg: datadogCfg()}.Parse(readFixture(t, "datadog_style.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Alerts) != 2 {
		t.Fatalf("got %d alerts", len(p.Alerts))
	}
	if a := p.Alerts[0]; a.Status != "firing" || a.Labels["host"] != "db-01" || a.StartsAt != "2026-10-31T08:00:00Z" {
		t.Errorf("first alert: %+v", a)
	}
	b := p.Alerts[1]
	if b.Status != "resolved" {
		t.Errorf("second alert should be resolved: %+v", b)
	}
	if d, _, ok := b.AffectedIdentity(); !ok || d != "shop/cart-7d9f" {
		t.Errorf("k8s identity = %q, %v", d, ok)
	}
	if _, has := b.Labels["host"]; has {
		t.Errorf("unmapped (missing) path must not create an empty label: %v", b.Labels)
	}
	if p.Status != "firing" {
		t.Errorf("payload status = %q, want firing (one alert still firing)", p.Status)
	}
}

func TestGeneric_RejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		cfg  config.GenericSourceConfig
		body string
		want string
	}{
		"missing alertname": {zabbixCfg(), `{"event":{"status":"PROBLEM","time":"2026.10.30 14:05:12"},"host":{"name":"web-01"}}`, "alertname"},
		"no identity":       {zabbixCfg(), `{"event":{"id":"1","status":"PROBLEM","time":"2026.10.30 14:05:12"},"trigger":{"name":"x"}}`, "no host"},
		"no fingerprint":    {zabbixCfg(), `{"event":{"status":"PROBLEM","time":"2026.10.30 14:05:12"},"trigger":{"name":"x"},"host":{"name":"h"}}`, "fingerprint"},
		"unknown status":    {zabbixCfg(), `{"event":{"status":"MAYBE","time":"2026.10.30 14:05:12"},"trigger":{"name":"x"},"host":{"name":"h"}}`, "status"},
		"bad time":          {zabbixCfg(), `{"event":{"status":"PROBLEM","time":"yesterday"},"trigger":{"name":"x"},"host":{"name":"h"}}`, "starts_at"},
		"alerts not array":  {datadogCfg(), `{"alerts":{"title":"x"}}`, "alerts_path"},
		"empty alerts":      {datadogCfg(), `{"alerts":[]}`, "zero alerts"},
		"not json":          {datadogCfg(), `nope`, "unmarshal"},
	}
	for name, c := range cases {
		_, err := Generic{Cfg: c.cfg}.Parse([]byte(c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestGeneric_DefaultsWhenOptionalFieldsUnmapped(t *testing.T) {
	cfg := config.GenericSourceConfig{
		Name:   "minimal",
		Fields: config.GenericSourceField{AlertName: "name"},
		Labels: map[string]string{"host": "server"},
	}
	fixed := time.Date(2026, 11, 1, 2, 3, 4, 0, time.UTC)
	g := Generic{Cfg: cfg, Now: func() time.Time { return fixed }}
	p1, err := g.Parse([]byte(`{"name":"Disk","server":"h1"}`))
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := g.Parse([]byte(`{"name":"Disk","server":"h1"}`))
	a := p1.Alerts[0]
	if a.Status != "firing" || a.StartsAt != "2026-11-01T02:03:04Z" {
		t.Errorf("defaults: %+v", a)
	}
	if a.Fingerprint == "" || a.Fingerprint != p2.Alerts[0].Fingerprint {
		t.Errorf("label fingerprint must be stable and non-empty: %q vs %q", a.Fingerprint, p2.Alerts[0].Fingerprint)
	}
	p3, _ := g.Parse([]byte(`{"name":"Disk","server":"h2"}`))
	if p3.Alerts[0].Fingerprint == a.Fingerprint {
		t.Errorf("different labels must give a different fingerprint")
	}
}

func TestLookup(t *testing.T) {
	g := Generic{Cfg: config.GenericSourceConfig{Name: "t", Fields: config.GenericSourceField{AlertName: "$.a.list.1.n"}, Labels: map[string]string{"host": "a.num"}}}
	p, err := g.Parse([]byte(`{"a":{"list":[{"n":"zero"},{"n":"one"}],"num":42}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Alerts[0].Labels["alertname"] != "one" || p.Alerts[0].Labels["host"] != "42" {
		t.Errorf("lookup: %v", p.Alerts[0].Labels)
	}
}

func TestGeneric_FingerprintIncludesSourceName(t *testing.T) {
	body := []byte(`{"event":{"id":"48213","status":"PROBLEM","time":"2026.10.30 14:05:12"},"trigger":{"name":"x"},"host":{"name":"h"}}`)
	a := zabbixCfg()
	b := zabbixCfg()
	b.Name = "zabbix-dr"
	pa, err := Generic{Cfg: a}.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := Generic{Cfg: b}.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if pa.Alerts[0].Fingerprint == pb.Alerts[0].Fingerprint {
		t.Errorf("same event id from two sources must not share a fingerprint: %q", pa.Alerts[0].Fingerprint)
	}

	min := config.GenericSourceConfig{Name: "one", Fields: config.GenericSourceField{AlertName: "name"}, Labels: map[string]string{"host": "server"}}
	min2 := min
	min2.Name = "two"
	lb := []byte(`{"name":"disk full","server":"db-01"}`)
	p1, _ := Generic{Cfg: min}.Parse(lb)
	p2, _ := Generic{Cfg: min2}.Parse(lb)
	if p1.Alerts[0].Fingerprint == p2.Alerts[0].Fingerprint {
		t.Errorf("label-hash fingerprints must also differ per source: %q", p1.Alerts[0].Fingerprint)
	}
}
