package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
