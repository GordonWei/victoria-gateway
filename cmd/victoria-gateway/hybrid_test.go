package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/model"
)

// recordingLogSource is a fake LogSource that remembers which identities
// it was asked about, so hybrid tests can assert which backend an alert
// was actually routed to (not just that some logs came back).
type recordingLogSource struct {
	name  string
	mu    sync.Mutex
	calls []aiops.LogIdentity
}

func (r *recordingLogSource) QueryRange(_ context.Context, id aiops.LogIdentity, _, _ time.Time, _ int) ([]aiops.LogEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, id)
	return []aiops.LogEntry{{Timestamp: time.Now(), Line: r.name + " log line: error rate spiked"}}, nil
}

func (r *recordingLogSource) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestHybridRouter_Pick(t *testing.T) {
	aws := alertRoute{name: "hybrid_routes[0]", logSourceName: "aws"}
	gcp := alertRoute{name: "hybrid_routes[1]", logSourceName: "gcp"}
	def := alertRoute{name: "default", logSourceName: "onprem"}
	r := &hybridRouter{routes: []hybridRoute{
		{matchers: map[string]string{"cloud": "aws"}, target: aws},
		{matchers: map[string]string{"cloud": "gcp", "env": "prod-*"}, target: gcp},
		{isDefault: true, target: def},
	}}
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"exact match", map[string]string{"cloud": "aws", "host": "x"}, "aws"},
		{"all matchers must match", map[string]string{"cloud": "gcp", "env": "staging"}, "onprem"},
		{"glob matcher", map[string]string{"cloud": "gcp", "env": "prod-tw"}, "gcp"},
		{"no cloud label falls to default", map[string]string{"host": "db01"}, "onprem"},
		{"empty labels fall to default", map[string]string{}, "onprem"},
	}
	for _, tc := range cases {
		if got := r.pick(tc.labels).logSourceName; got != tc.want {
			t.Errorf("%s: pick(%v) = %q, want %q", tc.name, tc.labels, got, tc.want)
		}
	}
}

func TestHybridRouter_FirstMatchWins(t *testing.T) {
	r := &hybridRouter{routes: []hybridRoute{
		{matchers: map[string]string{"cloud": "aws"}, target: alertRoute{logSourceName: "first"}},
		{matchers: map[string]string{"cloud": "aws", "team": "pay"}, target: alertRoute{logSourceName: "second"}},
		{isDefault: true, target: alertRoute{logSourceName: "default"}},
	}}
	if got := r.pick(map[string]string{"cloud": "aws", "team": "pay"}).logSourceName; got != "first" {
		t.Errorf("pick = %q, want %q (routes are evaluated in config order)", got, "first")
	}
}

func hybridTestConfig() *config.Config {
	return &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm", Model: "m"},
		LogSources: map[string]*config.LogSourceConfig{
			"onprem": {Type: "loki"},
			"site-b": {Type: "loki", Loki: &config.LokiEndpointConfig{Endpoint: "http://loki-b:3100"}},
			"aws":    {Type: "cloudwatch", CloudWatch: &config.CloudWatchConfig{Region: "us-east-1", LogGroupNames: []string{"/g"}}},
		},
		EscalationTargets: map[string]*config.CloudConfig{
			"default": {Provider: "anthropic", APIKey: "k", Model: "m"},
			"aws":     {Provider: "aws-devops-agent", DevOpsAgent: &config.DevOpsAgentConfig{SpaceID: "s"}},
		},
		HybridRoutes: []config.HybridRouteConfig{
			{Matchers: map[string]string{"cloud": "aws"}, LogSource: "aws", Escalation: "aws"},
			{Matchers: map[string]string{"site": "b"}, LogSource: "site-b", Escalation: "default"},
			{Default: true, LogSource: "onprem", Escalation: "default"},
		},
	}
}

func TestBuildHybridRouter_WiresNamedSourcesAndTargets(t *testing.T) {
	cfg := hybridTestConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	r, err := buildHybridRouter(cfg)
	if err != nil {
		t.Fatalf("buildHybridRouter: %v", err)
	}
	aws := r.pick(map[string]string{"cloud": "aws"})
	if aws.logSourceName != "aws" || aws.escalationName != "aws" || aws.cloud == nil || aws.logs == nil {
		t.Fatalf("aws route = %+v", aws)
	}
	if _, ok := aws.cloud.(*model.DevOpsAgentClient); !ok {
		t.Errorf("aws route cloud = %T, want *model.DevOpsAgentClient", aws.cloud)
	}
	siteB := r.pick(map[string]string{"site": "b"})
	def := r.pick(map[string]string{"host": "db01"})
	if siteB.cloud != def.cloud {
		t.Errorf("routes sharing escalation target %q should share one client", "default")
	}
	if siteB.logs == def.logs {
		t.Errorf("site-b and onprem are different Loki sources and must not share a client")
	}
	if def.name != "default" || def.logSourceName != "onprem" {
		t.Errorf("default route = %+v", def)
	}
}

func TestHybridSummary(t *testing.T) {
	got := hybridSummary(hybridTestConfig())
	for _, want := range []string{"aws(cloudwatch)", "onprem(loki)", "site-b(loki)", "aws(aws-devops-agent)", "default(anthropic)", "3 routes"} {
		if !strings.Contains(got, want) {
			t.Errorf("hybridSummary = %q, missing %q", got, want)
		}
	}
}

// newHybridTestHandler builds a handler whose router sends cloud=aws alerts
// to the "aws" fake log source + awsCloud, everything else to the "onprem"
// fake log source + defaultCloud, with a local model that always asks to
// escalate so the escalation target actually gets exercised.
func newHybridTestHandler(t *testing.T, onprem, aws *recordingLogSource, defaultCloud, awsCloud model.LLM) *handler {
	t.Helper()
	llmSrv := newFakeLLM(t, `{"summary": "local guess", "confidence": "low", "escalate": true, "reason": "unsure"}`)
	t.Cleanup(llmSrv.Close)
	h := newTestHandler(t, "http://unused-legacy-loki", llmSrv.URL)
	h.logs = nil // hybrid mode must not touch the legacy single source
	h.router = &hybridRouter{routes: []hybridRoute{
		{matchers: map[string]string{"cloud": "aws"}, target: alertRoute{name: "hybrid_routes[0]", logs: aws, logSourceName: "aws", cloud: awsCloud, escalationName: "aws"}},
		{isDefault: true, target: alertRoute{name: "default", logs: onprem, logSourceName: "onprem", cloud: defaultCloud, escalationName: "default"}},
	}}
	return h
}

func TestSummarizeOne_Hybrid_AWSAlertUsesAWSSourceAndTarget(t *testing.T) {
	onprem := &recordingLogSource{name: "onprem"}
	aws := &recordingLogSource{name: "aws"}
	defSrv := newFakeCloud(t, "default target answer")
	defer defSrv.Close()
	awsSrv := newFakeCloud(t, "aws target answer")
	defer awsSrv.Close()
	h := newHybridTestHandler(t, onprem, aws,
		model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: defSrv.URL, APIKey: "k", Model: "m"}),
		model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: awsSrv.URL, APIKey: "k", Model: "m"}))

	res := h.summarizeOne(aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "LambdaErrors", "host": "checkout-service", "cloud": "aws"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	})
	if res.Error != "" {
		t.Fatalf("summarizeOne error: %s", res.Error)
	}
	if aws.count() != 1 || onprem.count() != 0 {
		t.Errorf("log queries: aws=%d onprem=%d, want aws=1 onprem=0", aws.count(), onprem.count())
	}
	if res.AnalyzedBy != "cloud" || res.Summary != "aws target answer" {
		t.Errorf("AnalyzedBy=%q Summary=%q, want cloud / the aws target's answer", res.AnalyzedBy, res.Summary)
	}
}

func TestSummarizeOne_Hybrid_OnPremAlertUsesDefaultRoute(t *testing.T) {
	onprem := &recordingLogSource{name: "onprem"}
	aws := &recordingLogSource{name: "aws"}
	defSrv := newFakeCloud(t, "default target answer")
	defer defSrv.Close()
	awsSrv := newFakeCloud(t, "aws target answer")
	defer awsSrv.Close()
	h := newHybridTestHandler(t, onprem, aws,
		model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: defSrv.URL, APIKey: "k", Model: "m"}),
		model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: awsSrv.URL, APIKey: "k", Model: "m"}))

	res := h.summarizeOne(aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "DiskFull", "host": "db01"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	})
	if res.Error != "" {
		t.Fatalf("summarizeOne error: %s", res.Error)
	}
	if onprem.count() != 1 || aws.count() != 0 {
		t.Errorf("log queries: onprem=%d aws=%d, want onprem=1 aws=0", onprem.count(), aws.count())
	}
	if res.Summary != "default target answer" {
		t.Errorf("Summary = %q, want the default target's answer", res.Summary)
	}
}

func TestSummarizeOne_Hybrid_RouteWithoutEscalationStaysLocal(t *testing.T) {
	onprem := &recordingLogSource{name: "onprem"}
	aws := &recordingLogSource{name: "aws"}
	awsSrv := newFakeCloud(t, "aws target answer")
	defer awsSrv.Close()
	// default route has no escalation target (nil cloud), even though
	// the local model asks to escalate.
	h := newHybridTestHandler(t, onprem, aws, nil,
		model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: awsSrv.URL, APIKey: "k", Model: "m"}))

	res := h.summarizeOne(aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "DiskFull", "host": "db01"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	})
	if res.AnalyzedBy != "local" || res.Summary != "local guess" {
		t.Errorf("AnalyzedBy=%q Summary=%q, want local / the local guess", res.AnalyzedBy, res.Summary)
	}
}

func TestHandlerRoute_LegacyModeReturnsSingleSourceAndCloud(t *testing.T) {
	src := &recordingLogSource{name: "legacy"}
	cloud := model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: "http://x", APIKey: "k", Model: "m"})
	h := &handler{logs: src, cloud: cloud}
	got := h.route(map[string]string{"cloud": "aws"})
	if got.logs != src || got.cloud != cloud || got.name != "" {
		t.Errorf("legacy route = %+v, want the handler's single logs/cloud with an empty name", got)
	}
}
