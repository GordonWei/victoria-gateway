package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/config"
)

const grafanaHostPayload = `{"receiver":"vg","status":"firing","orgId":1,"version":"1","state":"alerting",
 "alerts":[{"status":"firing","labels":{"alertname":"cpu_high","host":"test-host"},
 "annotations":{"summary":"cpu high"},"startsAt":"2026-10-30T06:05:12Z","endsAt":"0001-01-01T00:00:00Z",
 "fingerprint":"f1","values":{"A":97.5}}]}`

func alertSourcesMux(h *handler, ac *config.AlertSourcesConfig) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/alertmanager", h.handleAlertmanagerWebhook)
	registerAlertSources(mux, h, ac)
	return mux
}

func testSources() *config.AlertSourcesConfig {
	return &config.AlertSourcesConfig{
		Grafana: &config.GrafanaSourceConfig{Enabled: true},
		Generic: []config.GenericSourceConfig{{
			Name:      "zabbix",
			Fields:    config.GenericSourceField{AlertName: "trigger.name", Status: "event.status"},
			StatusMap: map[string]string{"PROBLEM": "firing", "OK": "resolved"},
			Labels:    map[string]string{"host": "host.name"},
		}},
	}
}

func post(mux http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestAlertSources_NotRegisteredByDefault(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")
	mux := alertSourcesMux(h, nil)
	for _, p := range []string{"/webhook/grafana", "/webhook/generic/zabbix"} {
		if rec := post(mux, p, grafanaHostPayload); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404 when alert_sources is unset", p, rec.Code)
		}
	}
	off := &config.AlertSourcesConfig{Grafana: &config.GrafanaSourceConfig{Enabled: false}}
	if rec := post(alertSourcesMux(h, off), "/webhook/grafana", grafanaHostPayload); rec.Code != http.StatusNotFound {
		t.Errorf("grafana.enabled=false: status %d, want 404", rec.Code)
	}
}

func TestAlertSources_GrafanaEndToEnd(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "CPU 持續偏高，建議檢查負載來源。")
	defer llmSrv.Close()
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)

	rec := post(alertSourcesMux(h, testSources()), "/webhook/grafana", grafanaHostPayload)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Results []alertResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Error != "" || out.Results[0].Host != "test-host" {
		t.Fatalf("results = %+v", out.Results)
	}
}

func TestAlertSources_GenericEndToEndAndResolvedSkipped(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "磁碟快滿。")
	defer llmSrv.Close()
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	mux := alertSourcesMux(h, testSources())

	rec := post(mux, "/webhook/generic/zabbix", `{"event":{"status":"PROBLEM"},"trigger":{"name":"disk_full"},"host":{"name":"test-host"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"host":"test-host"`) {
		t.Fatalf("firing: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = post(mux, "/webhook/generic/zabbix", `{"event":{"status":"OK"},"trigger":{"name":"disk_full"},"host":{"name":"test-host"}}`)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "test-host") {
		t.Fatalf("resolved should be skipped like an Alertmanager resolved alert: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestAlertSources_GenericRejectsAndUnknownName(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")
	mux := alertSourcesMux(h, testSources())
	rec := post(mux, "/webhook/generic/zabbix", `{"event":{"status":"PROBLEM"},"trigger":{"name":"disk_full"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no host") {
		t.Errorf("missing identity: status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := post(mux, "/webhook/generic/nagios", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown generic name: status %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhook/grafana", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.Code)
	}
}

func TestAlertSources_UseWebhookAuth(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")
	h.webhookAuth = &config.WebhookAuthConfig{Username: "am", Password: "s3cret"}
	mux := alertSourcesMux(h, testSources())
	for _, p := range []string{"/webhook/grafana", "/webhook/generic/zabbix"} {
		if rec := post(mux, p, grafanaHostPayload); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials: status %d, want 401", p, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/grafana", strings.NewReader("not json"))
	req.SetBasicAuth("am", "s3cret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("with credentials, bad body: status %d, want 400 (auth passed, parse failed)", rec.Code)
	}
}

func TestAlertSources_GenericDedupIsPerSource(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "磁碟快滿。")
	defer llmSrv.Close()
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	src := func(name string) config.GenericSourceConfig {
		return config.GenericSourceConfig{
			Name:   name,
			Fields: config.GenericSourceField{AlertName: "trigger.name", Fingerprint: "event.id"},
			Labels: map[string]string{"host": "host.name"},
		}
	}
	mux := alertSourcesMux(h, &config.AlertSourcesConfig{Generic: []config.GenericSourceConfig{src("zabbix"), src("datadog")}})
	body := `{"event":{"id":"7"},"trigger":{"name":"disk_full"},"host":{"name":"test-host"}}`

	if rec := post(mux, "/webhook/generic/zabbix", body); !strings.Contains(rec.Body.String(), `"host":"test-host"`) {
		t.Fatalf("first zabbix alert not processed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(mux, "/webhook/generic/datadog", body); !strings.Contains(rec.Body.String(), `"host":"test-host"`) {
		t.Errorf("same event id from another source must not be deduped: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(mux, "/webhook/generic/zabbix/", body); strings.Contains(rec.Body.String(), "test-host") {
		t.Errorf("resend from the same source (trailing slash) must still be deduped: %d %s", rec.Code, rec.Body.String())
	}
}
