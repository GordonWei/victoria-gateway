package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/judge"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// captureLog redirects the standard logger for the rest of the test and
// returns a function yielding everything written so far.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(orig) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// newDownLLM is a local model server that is up but can't serve (503),
// the same shape LM Studio gives with no model loaded.
func newDownLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no model loaded", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// countingCloud answers like newFakeCloud and counts calls; status != 200
// makes every call fail with that status.
func countingCloud(t *testing.T, reply string, status int, calls *int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "upstream error", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []map[string]string{{"type": "text", "text": reply}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func anthropicAt(url string) model.LLM {
	return model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: url, APIKey: "k", Model: "m"})
}

func testAlert() aiops.Alert {
	return aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
}

func scrape(t *testing.T, m *metrics.Counters) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func wantContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s missing %q; got:\n%s", what, w, got)
		}
	}
}

// ── local summarizer fallbacks, through buildSummarizer ─────────────

func TestBuildSummarizer_FallbackAnswersWhenPrimaryDown(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	down := newDownLLM(t)
	backup := newFakeLLM(t, `{"summary":"backup answer","confidence":"high","escalate":false,"reason":"x"}`)
	defer backup.Close()
	logs := captureLog(t)

	h := newTestHandler(t, lokiSrv.URL, down.URL)
	h.summarizer = buildSummarizer(config.LLMConfig{
		Endpoint:  down.URL,
		Model:     "primary",
		Fallbacks: []config.LLMConfig{{Endpoint: backup.URL, Model: "backup"}},
	})
	res := h.summarizeOne(testAlert())
	if res.Error != "" || res.AnalyzedBy != "local" || res.Summary != "backup answer" {
		t.Fatalf("res = %+v, want the backup's local answer", res)
	}
	wantContains(t, "log", logs(), "summarizer failed for alert \"cpu_high\"", "trying summarizer.fallbacks[0]", "summarized by fallback summarizer.fallbacks[0]")
}

// ── every local model down → escalate instead ───────────────────────

func TestSummarizeOne_LocalDown_EscalatesToCloud(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	down := newDownLLM(t)
	var cloudCalls, judgeCalls int
	cloud := countingCloud(t, "cloud took over", http.StatusOK, &cloudCalls)
	judgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		judgeCalls++
		http.Error(w, "should not be called", 500)
	}))
	defer judgeSrv.Close()
	logs := captureLog(t)

	h := newTestHandler(t, lokiSrv.URL, down.URL)
	h.cloud = anthropicAt(cloud.URL)
	h.legacyEscalations = []escalationStep{{display: "anthropic", llm: h.cloud}}
	h.judgeClient = judge.NewClientWithEndpoint("k", judgeSrv.URL)
	h.metrics = &metrics.Counters{}

	res := h.summarizeOne(testAlert())
	if res.Error != "" || res.AnalyzedBy != "cloud" || res.EscalatedTo != "anthropic" || res.Summary != "cloud took over" {
		t.Fatalf("res = %+v", res)
	}
	if judgeCalls != 0 {
		t.Errorf("Jev called %d times; it judges a local summary and there isn't one", judgeCalls)
	}
	wantContains(t, "log", logs(), "local summarizer unavailable", "escalated to cloud (local LLM unavailable)")
	wantContains(t, "metrics", scrape(t, h.metrics), `victoria_gateway_escalations_total{target="anthropic"} 1`)
}

func TestSummarizeOne_LocalDown_NoCloud_ErrorUnchanged(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	down := newDownLLM(t)
	h := newTestHandler(t, lokiSrv.URL, down.URL)

	res := h.summarizeOne(testAlert())
	if !strings.HasPrefix(res.Error, "summarize: summarize: test chat failed: test returned 503") {
		t.Fatalf("Error = %q, want the pre-fallback error text", res.Error)
	}
	if strings.Contains(res.Error, "cloud") {
		t.Errorf("Error = %q mentions cloud, but no cloud is configured", res.Error)
	}
}

func TestSummarizeOne_LocalDown_CloudAlsoFails_ErrorNamesBoth(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	down := newDownLLM(t)
	var calls int
	cloud := countingCloud(t, "", http.StatusBadGateway, &calls)
	h := newTestHandler(t, lokiSrv.URL, down.URL)
	h.cloud = anthropicAt(cloud.URL)

	res := h.summarizeOne(testAlert())
	wantContains(t, "Error", res.Error, "test returned 503", "cloud escalation (local LLM unavailable) also failed", "502")
	if calls != 1 {
		t.Errorf("cloud calls = %d, want 1", calls)
	}
}

// A local 401 is a wrong API key, not an outage: it must fail the alert
// with the original error and never reach the cloud, even with a cloud
// target configured.
func TestSummarizeOne_LocalAuthError_DoesNotEscalate(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	var calls int
	cloud := countingCloud(t, "should not be used", http.StatusOK, &calls)
	h := newTestHandler(t, lokiSrv.URL, unauthorized.URL)
	h.cloud = anthropicAt(cloud.URL)
	h.legacyEscalations = []escalationStep{{display: "anthropic", llm: h.cloud}}

	res := h.summarizeOne(testAlert())
	if !strings.HasPrefix(res.Error, "summarize: summarize: test chat failed: test returned 401: invalid api key") {
		t.Fatalf("Error = %q, want the local 401 reported as-is", res.Error)
	}
	if calls != 0 || res.AnalyzedBy == "cloud" {
		t.Errorf("cloud calls = %d, AnalyzedBy = %q; a 401 must not escalate", calls, res.AnalyzedBy)
	}
}

func TestSummarizeOne_LocalDown_RateLimited(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	down := newDownLLM(t)
	var calls int
	cloud := countingCloud(t, "x", http.StatusOK, &calls)
	h := newTestHandler(t, lokiSrv.URL, down.URL)
	h.cloud = anthropicAt(cloud.URL)
	h.escalation = config.EscalationConfig{MaxPerHour: 1}
	h.metrics = &metrics.Counters{}

	first := h.summarizeOne(testAlert())
	second := h.summarizeOne(testAlert())
	if first.AnalyzedBy != "cloud" {
		t.Fatalf("first = %+v, want escalated", first)
	}
	wantContains(t, "second.Error", second.Error, "test returned 503", "escalation.max_per_hour reached")
	if calls != 1 {
		t.Errorf("cloud calls = %d, want 1 (the cap applies to this path too)", calls)
	}
	wantContains(t, "metrics", scrape(t, h.metrics), `victoria_gateway_escalation_rate_limited_total{target="anthropic"} 1`)
}

// ── escalation fallbacks ────────────────────────────────────────────

func TestSummarizeOne_LegacyCloudFallback(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"low","escalate":true,"reason":"unsure"}`)
	defer llmSrv.Close()
	var primaryCalls, fbCalls int
	primary := countingCloud(t, "", http.StatusInternalServerError, &primaryCalls)
	fb := countingCloud(t, "fallback answer", http.StatusOK, &fbCalls)
	logs := captureLog(t)

	cfg := &config.Config{
		Cloud:          &config.CloudConfig{Provider: "anthropic", APIKey: "k", Endpoint: primary.URL},
		CloudFallbacks: []*config.CloudConfig{{Provider: "anthropic", APIKey: "k", Endpoint: fb.URL}},
	}
	cloud, err := buildCloud("cloud", cfg.Cloud)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := buildLegacyEscalations(cfg, cloud)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = cloud
	h.legacyEscalations = steps
	h.escalation = config.EscalationConfig{MaxPerHour: 1}
	h.metrics = &metrics.Counters{}

	res := h.summarizeOne(testAlert())
	if res.AnalyzedBy != "cloud" || res.Summary != "fallback answer" || res.EscalatedTo != "anthropic (cloud_fallbacks[0])" {
		t.Fatalf("res = %+v", res)
	}
	if primaryCalls != 1 || fbCalls != 1 {
		t.Errorf("calls primary=%d fallback=%d, want 1/1", primaryCalls, fbCalls)
	}
	// One escalation, however many targets it tried, is one slot.
	if h.escalationCount != 1 {
		t.Errorf("escalationCount = %d, want 1", h.escalationCount)
	}
	wantContains(t, "log", logs(), "cloud escalation failed for alert \"cpu_high\" (local model requested escalation: unsure)", `falling back to escalation target "cloud_fallbacks[0]"`, `escalated to cloud target "cloud_fallbacks[0]"`)
	wantContains(t, "metrics", scrape(t, h.metrics),
		`victoria_gateway_escalation_failures_total{target="anthropic"} 1`,
		`victoria_gateway_escalations_total{target="anthropic (cloud_fallbacks[0])"} 1`,
		`victoria_gateway_alerts_total{route="legacy"} 1`)
}

func TestSummarizeOne_HybridEscalationList(t *testing.T) {
	onprem := &recordingLogSource{name: "onprem"}
	aws := &recordingLogSource{name: "aws"}
	var awsCalls, defCalls int
	awsSrv := countingCloud(t, "", http.StatusServiceUnavailable, &awsCalls)
	defSrv := countingCloud(t, "default answer", http.StatusOK, &defCalls)
	h := newHybridTestHandler(t, onprem, aws, nil, nil)
	h.router.routes[0].target.escalations = []escalationStep{
		{name: "aws", display: "aws", llm: anthropicAt(awsSrv.URL)},
		{name: "default", display: "default", llm: anthropicAt(defSrv.URL)},
	}
	h.metrics = &metrics.Counters{}
	logs := captureLog(t)

	alert := testAlert()
	alert.Labels["cloud"] = "aws"
	res := h.summarizeOne(alert)
	if res.AnalyzedBy != "cloud" || res.EscalatedTo != "default" || res.Summary != "default answer" {
		t.Fatalf("res = %+v", res)
	}
	if awsCalls != 1 || defCalls != 1 {
		t.Errorf("calls aws=%d default=%d, want 1/1", awsCalls, defCalls)
	}
	wantContains(t, "log", logs(), `escalation "aws → default"`, `cloud escalation to "aws" failed`, `escalated to cloud target "default"`)
	wantContains(t, "metrics", scrape(t, h.metrics),
		`victoria_gateway_alerts_total{route="hybrid_routes[0]"} 1`,
		`victoria_gateway_loki_query_duration_seconds_count{log_source="aws"} 1`,
		`victoria_gateway_cloud_llm_duration_seconds_count{target="aws"} 1`,
		`victoria_gateway_cloud_llm_duration_seconds_count{target="default"} 1`)
}

func TestSummarizeOne_AllEscalationsFail_StaysLocal(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"low","escalate":true,"reason":"unsure"}`)
	defer llmSrv.Close()
	var a, b int
	s1 := countingCloud(t, "", 500, &a)
	s2 := countingCloud(t, "", 500, &b)
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = anthropicAt(s1.URL)
	h.legacyEscalations = []escalationStep{{display: "anthropic", llm: h.cloud}, {name: "cloud_fallbacks[0]", display: "x", llm: anthropicAt(s2.URL)}}
	res := h.summarizeOne(testAlert())
	if res.AnalyzedBy != "local" || res.Summary != "local" || res.Error != "" || res.EscalatedTo != "" {
		t.Fatalf("res = %+v, want the local result", res)
	}
}

// ── escalate with nowhere to go ─────────────────────────────────────

func TestSummarizeOne_EscalateWithoutTarget_LogsAndCounts(t *testing.T) {
	onprem := &recordingLogSource{name: "onprem"}
	aws := &recordingLogSource{name: "aws"}
	h := newHybridTestHandler(t, onprem, aws, nil, nil) // default route: no escalation
	h.metrics = &metrics.Counters{}
	logs := captureLog(t)

	res := h.summarizeOne(testAlert())
	if res.AnalyzedBy != "local" {
		t.Fatalf("res = %+v", res)
	}
	wantContains(t, "log", logs(), `would escalate (local model requested escalation`, `but route "default" has no escalation target`)
	wantContains(t, "metrics", scrape(t, h.metrics), `victoria_gateway_escalation_no_target_total{route="default"} 1`)
}

func TestStartupNotes(t *testing.T) {
	legacy := &config.Config{Loki: config.LokiConfig{Endpoint: "http://loki:3100"}}
	if notes := startupNotes(legacy); len(notes) != 0 {
		t.Errorf("plain legacy config got notes %v, want none (banner unchanged)", notes)
	}

	h := hybridTestConfig()
	h.HybridRoutes[2].Escalation = nil
	h.Escalation.AlwaysCloud = []string{"cpu_high"}
	wantContains(t, "notes", strings.Join(startupNotes(h), "\n"), "always_cloud is set, but route(s) default have no escalation target")

	fallbacks := &config.Config{
		Loki:           config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer:     config.LLMConfig{Fallbacks: []config.LLMConfig{{Endpoint: "http://b", Model: "bm"}}},
		Cloud:          &config.CloudConfig{Provider: "bedrock"},
		CloudFallbacks: []*config.CloudConfig{{Provider: "anthropic"}},
	}
	wantContains(t, "notes", strings.Join(startupNotes(fallbacks), "\n"),
		"summarizer fallbacks: http://b (bm)", "cloud fallbacks: anthropic")
}

// Investigation targets whose poll limit exceeds the shutdown grace get a
// warning, wherever they are configured; the defaults are chosen so a
// default gcp-cloud-assist doesn't trigger it.
func TestStartupNotes_PollBeyondGrace(t *testing.T) {
	legacy := &config.Config{
		Loki:  config.LokiConfig{Endpoint: "http://loki:3100"},
		Cloud: &config.CloudConfig{Provider: "gcp-cloud-assist", Project: "p"},
	}
	if notes := startupNotes(legacy); len(notes) != 0 {
		t.Errorf("default gcp-cloud-assist (270s) under the default grace (300s) got notes %v, want none", notes)
	}
	legacy.Cloud.PollTimeoutSec = 301
	wantContains(t, "notes", strings.Join(startupNotes(legacy), "\n"),
		"cloud (gcp-cloud-assist) can wait up to 5m1s for an investigation, longer than shutdown_grace_sec (5m0s)")
	legacy.ShutdownGraceSec = 400
	if notes := startupNotes(legacy); len(notes) != 0 {
		t.Errorf("poll 301s under a 400s grace got notes %v, want none", notes)
	}

	fallbacks := &config.Config{
		Loki:  config.LokiConfig{Endpoint: "http://loki:3100"},
		Cloud: &config.CloudConfig{Provider: "gemini"},
		CloudFallbacks: []*config.CloudConfig{
			{Provider: "vertex-ai", Project: "p", Model: "m"},
			{Provider: "aws-devops-agent", DevOpsAgent: &config.DevOpsAgentConfig{SpaceID: "s"}},
		},
		ShutdownGraceSec: 120,
	}
	notes := strings.Join(startupNotes(fallbacks), "\n")
	wantContains(t, "notes", notes, "cloud_fallbacks[1] (aws-devops-agent) can wait up to 10m0s", "shutdown_grace_sec (2m0s)")
	if strings.Contains(notes, "cloud_fallbacks[0]") {
		t.Errorf("vertex-ai isn't an investigation target, but got a note:\n%s", notes)
	}

	h := hybridTestConfig()
	h.EscalationTargets["gcp"] = &config.CloudConfig{Provider: "gcp-cloud-assist", Project: "p", PollTimeoutSec: 900}
	h.EscalationTargets["aws"] = &config.CloudConfig{Provider: "aws-devops-agent", DevOpsAgent: &config.DevOpsAgentConfig{SpaceID: "s"}}
	notes = strings.Join(startupNotes(h), "\n")
	wantContains(t, "notes", notes, "escalation_targets.aws (aws-devops-agent) can wait up to 10m0s", "escalation_targets.gcp (gcp-cloud-assist) can wait up to 15m0s")
	if strings.Index(notes, "escalation_targets.aws") > strings.Index(notes, "escalation_targets.gcp") {
		t.Errorf("notes not in target-name order:\n%s", notes)
	}
}

// ── legacy log_source.loki is dropped even if Validate was skipped ──

func TestBuildLogSource_LegacyIgnoresNestedLokiEndpoint(t *testing.T) {
	var topHits, nestedHits int
	top := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topHits++
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer top.Close()
	nested := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nestedHits++
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	defer nested.Close()

	cfg := &config.Config{
		Loki:      config.LokiConfig{Endpoint: top.URL},
		LogSource: &config.LogSourceConfig{Type: "loki", Loki: &config.LokiEndpointConfig{Endpoint: nested.URL}},
	}
	src, err := buildLogSource(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, id, _ := (aiops.Alert{Labels: map[string]string{"host": "h"}}).LogIdentity()
	_, _ = src.QueryRange(t.Context(), id, time.Now().Add(-time.Minute), time.Now(), 10)
	if topHits != 1 || nestedHits != 0 {
		t.Errorf("hits top=%d nested=%d, want the top-level loki.endpoint only", topHits, nestedHits)
	}
	if cfg.LogSource.Loki == nil {
		t.Error("buildLogSource must not mutate the caller's config")
	}
}

// ── which target answered reaches the tracker issue ─────────────────

func TestCaptureIncident_IssueBodyNamesTarget(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"low","escalate":true,"reason":"unsure"}`)
	defer llmSrv.Close()
	var calls int
	cloud := countingCloud(t, "cloud answer", http.StatusOK, &calls)
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1}}}})
	}))
	defer embedSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = anthropicAt(cloud.URL)
	h.legacyEscalations = []escalationStep{{display: "bedrock", llm: h.cloud}}
	store := &fakeRAGStore{}
	tr := &fakeTracker{}
	h.rag = store
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "bge-m3", "")
	h.tracker = tr

	res := h.summarizeOne(testAlert())
	if res.EscalatedTo != "bedrock" {
		t.Fatalf("res = %+v", res)
	}
	if len(tr.bodies) != 1 || !strings.Contains(tr.bodies[0], "**分析結果（cloud → bedrock）：**") {
		t.Errorf("issue bodies = %q", tr.bodies)
	}
}

// ── failures are logged in both modes ───────────────────────────────

func noHostPayload(fp string) []byte {
	b, _ := json.Marshal(map[string]any{
		"version": "4",
		"status":  "firing",
		"alerts": []map[string]any{{
			"status":      "firing",
			"labels":      map[string]string{"alertname": "mystery_alert"},
			"startsAt":    time.Now().Format(time.RFC3339),
			"fingerprint": fp,
		}},
	})
	return b
}

func TestProcessAlert_FailureLogged_SyncAndAsync(t *testing.T) {
	for _, async := range []bool{false, true} {
		logs := captureLog(t)
		h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")
		h.async = async
		rec := httptest.NewRecorder()
		h.handleAlertmanagerWebhook(rec, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(noHostPayload(""))))
		h.inFlight.Wait()
		wantContains(t, "log", logs(), `aiops: alert "mystery_alert" failed: alert has neither`)
	}
}
