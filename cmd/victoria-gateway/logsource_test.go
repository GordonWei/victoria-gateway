package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestSummarizeOne_RefusedSearchTerm_SummarizesWithoutLogs(t *testing.T) {
	llmSrv := newFakeLLM(t, `{"summary":"probe down","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	src := &refusingLogSource{}
	h := newTestHandler(t, "http://unused.invalid", llmSrv.URL)
	h.logs = src
	h.legacyLogSource = "cloudwatch"
	h.metrics = &metrics.Counters{}
	logs := captureLog(t)

	res := h.summarizeOne(blackboxAlert())
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
