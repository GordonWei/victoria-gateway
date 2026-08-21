package aiops

import (
	"testing"
)

func TestParseWebhook_ValidPayload(t *testing.T) {
	body := []byte(`{
		"version": "4",
		"groupKey": "{}:{alertname=\"HighCPU\"}",
		"truncatedAlerts": 0,
		"status": "firing",
		"receiver": "webhook-aiops",
		"groupLabels": {"alertname": "HighCPU"},
		"commonLabels": {"severity": "critical", "host": "web-01"},
		"commonAnnotations": {"summary": "CPU usage above 90%"},
		"externalURL": "http://alertmanager:9093",
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "host": "web-01", "severity": "critical"},
				"annotations": {"summary": "CPU usage above 90%", "description": "web-01 CPU at 95%"},
				"startsAt": "2026-08-20T10:00:00+08:00",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "http://prometheus:9090/graph?g0.expr=cpu_usage",
				"fingerprint": "abc123"
			}
		]
	}`)

	p, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Version != "4" {
		t.Errorf("version = %q, want \"4\"", p.Version)
	}
	if p.Status != "firing" {
		t.Errorf("status = %q, want \"firing\"", p.Status)
	}
	if len(p.Alerts) != 1 {
		t.Fatalf("alerts count = %d, want 1", len(p.Alerts))
	}

	host, ok := p.Alerts[0].Host()
	if !ok {
		t.Fatal("Host() returned ok=false, expected host label")
	}
	if host != "web-01" {
		t.Errorf("host = %q, want \"web-01\"", host)
	}

	st, err := p.Alerts[0].StartTime()
	if err != nil {
		t.Fatalf("StartTime() error: %v", err)
	}
	if st.IsZero() {
		t.Error("StartTime() returned zero time")
	}
}

func TestParseWebhook_EmptyAlerts(t *testing.T) {
	body := []byte(`{
		"version": "4",
		"status": "firing",
		"receiver": "test",
		"alerts": []
	}`)

	_, err := ParseWebhook(body)
	if err == nil {
		t.Fatal("expected error for empty alerts, got nil")
	}
}

func TestParseWebhook_NoHostLabel(t *testing.T) {
	body := []byte(`{
		"version": "4",
		"status": "firing",
		"receiver": "test",
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "DiskFull", "severity": "warning"},
				"annotations": {},
				"startsAt": "2026-08-20T10:00:00+08:00",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "",
				"fingerprint": "def456"
			}
		]
	}`)

	p, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("ParseWebhook itself should not fail: %v", err)
	}

	_, ok := p.Alerts[0].Host()
	if ok {
		t.Error("Host() returned ok=true, expected false when neither host nor instance label exists")
	}
}
