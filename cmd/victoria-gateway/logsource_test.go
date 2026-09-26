package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
)

func TestLogSourceType_NilBlockDefaultsToLoki(t *testing.T) {
	if got := logSourceType(&config.Config{}); got != "loki" {
		t.Errorf("logSourceType(nil block) = %q, want %q", got, "loki")
	}
}

func TestLogSourceType_EmptyTypeDefaultsToLoki(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{}}
	if got := logSourceType(cfg); got != "loki" {
		t.Errorf("logSourceType(empty Type) = %q, want %q", got, "loki")
	}
}

func TestLogSourceType_ExplicitType(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "cloudwatch"}}
	if got := logSourceType(cfg); got != "cloudwatch" {
		t.Errorf("logSourceType = %q, want %q", got, "cloudwatch")
	}
}

func TestBuildLogSource_DefaultsToLoki(t *testing.T) {
	cfg := &config.Config{Loki: config.LokiConfig{Endpoint: "http://loki:3100"}}
	src, err := buildLogSource(cfg)
	if err != nil {
		t.Fatalf("buildLogSource: %v", err)
	}
	if src == nil {
		t.Fatal("expected a non-nil LogSource for the loki default")
	}
}

func TestBuildLogSource_CloudWatch(t *testing.T) {
	cfg := &config.Config{
		LogSource: &config.LogSourceConfig{
			Type: "cloudwatch",
			CloudWatch: &config.CloudWatchConfig{
				Region:        "us-east-1",
				LogGroupNames: []string{"/aws/lambda/my-fn"},
			},
		},
	}
	src, err := buildLogSource(cfg)
	if err != nil {
		t.Fatalf("buildLogSource: %v", err)
	}
	if src == nil {
		t.Fatal("expected a non-nil LogSource for cloudwatch")
	}
}

func TestBuildLogSource_CloudWatch_NilBlockErrors(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "cloudwatch"}}
	if _, err := buildLogSource(cfg); err == nil {
		t.Error("expected an error when log_source.type is cloudwatch but log_source.cloudwatch is nil")
	}
}

func TestBuildLogSource_GCPLogging(t *testing.T) {
	cfg := &config.Config{
		LogSource: &config.LogSourceConfig{
			Type:       "gcp_logging",
			GCPLogging: &config.GCPLoggingConfig{ProjectID: "my-project"},
		},
	}
	src, err := buildLogSource(cfg)
	if err != nil {
		t.Fatalf("buildLogSource: %v", err)
	}
	if src == nil {
		t.Fatal("expected a non-nil LogSource for gcp_logging")
	}
}

func TestBuildLogSource_GCPLogging_NilBlockErrors(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "gcp_logging"}}
	if _, err := buildLogSource(cfg); err == nil {
		t.Error("expected an error when log_source.type is gcp_logging but log_source.gcp_logging is nil")
	}
}

func TestBuildLogSource_UnknownType(t *testing.T) {
	cfg := &config.Config{LogSource: &config.LogSourceConfig{Type: "splunk"}}
	if _, err := buildLogSource(cfg); err == nil {
		t.Error("expected an error for an unrecognized log_source.type")
	}
}

// refusingLogSource fails every query the way cloudwatch/gcplogging do
// when SafeTerm refuses the alert's identity.
type refusingLogSource struct{ calls int }

func (r *refusingLogSource) QueryRange(_ context.Context, id aiops.LogIdentity, _, _ time.Time, _ int) ([]aiops.LogEntry, error) {
	r.calls++
	if _, err := id.SafeTerm(); err != nil {
		return nil, fmt.Errorf("cloudwatch: %w", err)
	}
	return nil, errors.New("unexpected: term was safe")
}

type failingLogSource struct{}

func (failingLogSource) QueryRange(context.Context, aiops.LogIdentity, time.Time, time.Time, int) ([]aiops.LogEntry, error) {
	return nil, errors.New("cloudwatch: AccessDeniedException")
}

func blackboxAlert() aiops.Alert {
	return aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "probe_failed", "instance": "https://x/health"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
}

// unsafeBlackboxAlert's instance is a URL whose hostname is still unsafe
// after ForLogQuery reduces it, so it takes the refused path.
func unsafeBlackboxAlert() aiops.Alert {
	a := blackboxAlert()
	a.Labels = map[string]string{"alertname": "probe_failed", "instance": "http://a'b.example/health"}
	return a
}

func TestSummarizeOne_RefusedSearchTerm_SummarizesWithoutLogs(t *testing.T) {
	llmSrv := newFakeLLM(t, `{"summary":"probe down","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	src := &refusingLogSource{}
	h := newTestHandler(t, "http://unused.invalid", llmSrv.URL)
	h.logs = src
	h.legacyLogSource = "cloudwatch"
	h.metrics = &metrics.Counters{}
	logs := captureLog(t)

	res := h.summarizeOne(unsafeBlackboxAlert())
	if res.Error != "" || res.Summary != "probe down" || res.AnalyzedBy != "local" {
		t.Fatalf("res = %+v, want a local summary without logs", res)
	}
	if src.calls != 1 {
		t.Errorf("log source calls = %d, want 1", src.calls)
	}
	wantContains(t, "log", logs(), `log query skipped on log source "cloudwatch"`, "summarizing without logs", "refusing to search for")
	wantContains(t, "metrics", scrape(t, h.metrics), `victoria_gateway_log_query_refused_total{log_source="cloudwatch"} 1`)
}

func TestSummarizeOne_OtherLogQueryErrorStillFails(t *testing.T) {
	llmSrv := newFakeLLM(t, `{"summary":"x","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	h := newTestHandler(t, "http://unused.invalid", llmSrv.URL)
	h.logs = failingLogSource{}
	h.legacyLogSource = "cloudwatch"
	h.metrics = &metrics.Counters{}

	res := h.summarizeOne(blackboxAlert())
	if res.Error != "log query: cloudwatch: AccessDeniedException" {
		t.Fatalf("Error = %q, want the log query error unchanged", res.Error)
	}
	if strings.Contains(scrape(t, h.metrics), `victoria_gateway_log_query_refused_total{`) {
		t.Error("a real log query error must not count as refused")
	}
}

// identityRecordingLogSource remembers the identity it was asked for.
type identityRecordingLogSource struct{ got []aiops.LogIdentity }

func (r *identityRecordingLogSource) QueryRange(_ context.Context, id aiops.LogIdentity, _, _ time.Time, _ int) ([]aiops.LogEntry, error) {
	r.got = append(r.got, id)
	if _, err := id.SafeTerm(); err != nil {
		return nil, err
	}
	return []aiops.LogEntry{{Timestamp: time.Now(), Line: "probe log"}}, nil
}

func TestSummarizeOne_URLInstanceSearchesHostname(t *testing.T) {
	llmSrv := newFakeLLM(t, `{"summary":"probe down","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	src := &identityRecordingLogSource{}
	h := newTestHandler(t, "http://unused.invalid", llmSrv.URL)
	h.logs = src
	h.legacyLogSource = "cloudwatch"
	h.metrics = &metrics.Counters{}
	logs := captureLog(t)

	alert := blackboxAlert()
	alert.Labels["instance"] = "https://kmp.tw:8443/health?x=1"
	res := h.summarizeOne(alert)
	if res.Error != "" || res.Summary != "probe down" {
		t.Fatalf("res = %+v", res)
	}
	if len(src.got) != 1 || src.got[0].Host != "kmp.tw" {
		t.Fatalf("log source asked for %+v, want host kmp.tw", src.got)
	}
	if res.Host != "https://kmp.tw:8443/health?x=1" {
		t.Errorf("res.Host = %q, want the original instance kept for display", res.Host)
	}
	wantContains(t, "log", logs(), `log search host "https://kmp.tw:8443/health?x=1" is a URL, searching for its hostname "kmp.tw" instead`)
	if strings.Contains(scrape(t, h.metrics), `victoria_gateway_log_query_refused_total{`) {
		t.Error("a normalized URL must not count as refused")
	}
}

// Loki gets the same normalization; a host:port instance is untouched.
func TestSummarizeOne_LokiSelectorForURLAndHostPort(t *testing.T) {
	llmSrv := newFakeLLM(t, `{"summary":"s","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	var mu sync.Mutex
	var queries []string
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer loki.Close()
	h := newTestHandler(t, loki.URL, llmSrv.URL)
	logs := captureLog(t)

	for _, instance := range []string{"https://kmp.tw/health", "172.16.100.6:9100", "http://[2001:db8::1]:9115/probe"} {
		alert := blackboxAlert()
		alert.Labels["instance"] = instance
		if res := h.summarizeOne(alert); res.Error != "" {
			t.Fatalf("%s: res = %+v", instance, res)
		}
	}
	want := []string{`{host="kmp.tw"}`, `{host="172.16.100.6:9100"}`, `{host="2001:db8::1"}`}
	if strings.Join(queries, " ") != strings.Join(want, " ") {
		t.Errorf("Loki queries = %q, want %q", queries, want)
	}
	if strings.Contains(logs(), `"172.16.100.6:9100" is a URL`) {
		t.Error("a host:port instance was logged as normalized")
	}
}
