package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
)

// openAICompatServer answers /v1/chat/completions with reply, or with
// status when it isn't 200, and records the Authorization header of the
// last request.
func openAICompatServer(t *testing.T, reply string, status int, calls *int, auth *string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		*auth = r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			http.Error(w, "upstream error", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A 503 from an openai-compatible cloud target falls back to the next
// one; the Authorization header is sent only when api_key is set; and
// the metrics carry the provider name as their target label.
func TestSummarizeOne_OpenAICompatibleCloudAndFallback(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"low","escalate":true,"reason":"unsure"}`)
	defer llmSrv.Close()
	var primaryCalls, fbCalls int
	var primaryAuth, fbAuth string
	primary := openAICompatServer(t, "", http.StatusServiceUnavailable, &primaryCalls, &primaryAuth)
	fb := openAICompatServer(t, `{"summary":"vllm answer","confidence":"high","escalate":false,"reason":"r"}`, http.StatusOK, &fbCalls, &fbAuth)

	cfg := &config.Config{
		Cloud:          &config.CloudConfig{Provider: "openai-compatible", Endpoint: primary.URL, Model: "m", APIKey: "sk-test"},
		CloudFallbacks: []*config.CloudConfig{{Provider: "openai-compatible", Endpoint: fb.URL, Model: "m"}},
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
	if res.AnalyzedBy != "cloud" || res.Summary != "vllm answer" || res.EscalatedTo != "openai-compatible (cloud_fallbacks[0])" {
		t.Fatalf("res = %+v", res)
	}
	if primaryCalls != 1 || fbCalls != 1 {
		t.Errorf("calls primary=%d fallback=%d, want 1/1", primaryCalls, fbCalls)
	}
	if primaryAuth != "Bearer sk-test" {
		t.Errorf("primary Authorization = %q, want the configured key", primaryAuth)
	}
	if fbAuth != "" {
		t.Errorf("fallback Authorization = %q, want none (no api_key)", fbAuth)
	}
	wantContains(t, "metrics", scrape(t, h.metrics),
		`victoria_gateway_escalation_failures_total{target="openai-compatible"} 1`,
		`victoria_gateway_escalations_total{target="openai-compatible (cloud_fallbacks[0])"} 1`)
}

// noADC points Application Default Credentials at a file that doesn't
// exist, so building a GCP client in a test never picks up the
// developer's own gcloud login or probes a metadata server.
func noADC(t *testing.T) {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing.json"))
}

// fakeADC writes an authorized_user ADC file whose token_uri is a local
// server handing out "fake-adc-token", and points
// GOOGLE_APPLICATION_CREDENTIALS at it — so the real NewVertexAIClient /
// NewCloudAssistClient code path (FindDefaultCredentials included) runs
// without real credentials.
func fakeADC(t *testing.T) {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-adc-token","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenSrv.Close)
	path := filepath.Join(t.TempDir(), "adc.json")
	adc := fmt.Sprintf(`{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r","token_uri":%q}`, tokenSrv.URL+"/token")
	if err := os.WriteFile(path, []byte(adc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

// vertexServer answers generateContent with reply (or with status when
// it isn't 200) and records the last Authorization header and path.
func vertexServer(t *testing.T, reply string, status int, calls *int, auth, path *string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		*auth = r.Header.Get("Authorization")
		*path = r.URL.Path
		mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{"content": map[string]any{"parts": []map[string]string{{"text": reply}}}, "finishReason": "STOP"}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSummarizeOne_VertexAILegacyCloud(t *testing.T) {
	fakeADC(t)
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"low","escalate":true,"reason":"unsure"}`)
	defer llmSrv.Close()
	var calls int
	var auth, path string
	vx := vertexServer(t, `{"summary":"vertex answer","confidence":"high","escalate":false,"reason":"r"}`, http.StatusOK, &calls, &auth, &path)

	cfg := &config.Config{Cloud: &config.CloudConfig{Provider: "vertex-ai", Project: "proj-1", Location: "us-central1", Model: "gemini-2.5-flash", Endpoint: vx.URL}}
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
	if res.AnalyzedBy != "cloud" || res.Summary != "vertex answer" || res.EscalatedTo != "vertex-ai" {
		t.Fatalf("res = %+v", res)
	}
	if auth != "Bearer fake-adc-token" {
		t.Errorf("Authorization = %q, want the ADC token", auth)
	}
	if want := "/v1/projects/proj-1/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	wantContains(t, "metrics", scrape(t, h.metrics), `victoria_gateway_escalations_total{target="vertex-ai"} 1`)
}

// A 429 (quota) from a vertex-ai escalation target falls through to the
// route's next target, and both are labeled by target name.
func TestSummarizeOne_HybridVertexAIQuotaFallsBack(t *testing.T) {
	fakeADC(t)
	var vxCalls, defCalls int
	var vxAuth, vxPath, defAuth string
	vx := vertexServer(t, "", http.StatusTooManyRequests, &vxCalls, &vxAuth, &vxPath)
	def := openAICompatServer(t, `{"summary":"litellm answer","confidence":"high","escalate":false,"reason":"r"}`, http.StatusOK, &defCalls, &defAuth)

	cfg := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		LogSources: map[string]*config.LogSourceConfig{"onprem": {Type: "loki"}},
		EscalationTargets: map[string]*config.CloudConfig{
			"gcp":     {Provider: "vertex-ai", Project: "proj-1", Model: "gemini-2.5-flash", Endpoint: vx.URL},
			"default": {Provider: "openai-compatible", Endpoint: def.URL, Model: "m"},
		},
		HybridRoutes: []config.HybridRouteConfig{{Default: true, LogSource: "onprem", Escalation: config.EscalationList{"gcp", "default"}}},
	}
	router, err := buildHybridRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	onprem := &recordingLogSource{name: "onprem"}
	h := newHybridTestHandler(t, onprem, &recordingLogSource{name: "aws"}, nil, nil)
	h.router.routes = []hybridRoute{{isDefault: true, target: alertRoute{name: "default", logs: onprem, logSourceName: "onprem", escalations: router.routes[0].target.escalations}}}
	h.metrics = &metrics.Counters{}
	h.escalation = config.EscalationConfig{AlwaysCloud: []string{"cpu_high"}}

	res := h.summarizeOne(testAlert())
	if res.AnalyzedBy != "cloud" || res.EscalatedTo != "default" || res.Summary != "litellm answer" {
		t.Fatalf("res = %+v", res)
	}
	if vxCalls != 1 || defCalls != 1 {
		t.Errorf("calls vertex=%d default=%d, want 1/1", vxCalls, defCalls)
	}
	// Global is the default location when none is set.
	if want := "/v1/projects/proj-1/locations/global/publishers/google/models/gemini-2.5-flash:generateContent"; vxPath != want {
		t.Errorf("vertex path = %q, want %q", vxPath, want)
	}
	wantContains(t, "metrics", scrape(t, h.metrics),
		`victoria_gateway_escalation_failures_total{target="gcp"} 1`,
		`victoria_gateway_escalations_total{target="default"} 1`,
		`victoria_gateway_cloud_llm_duration_seconds_count{target="gcp"} 1`)
}
