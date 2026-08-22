package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// newFakeLoki returns an httptest server that answers Loki's query_range
// API shape with one canned log line, regardless of the query.
func newFakeLoki(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/loki/api/v1/query_range") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"host":"test-host"},"values":[["%d","cpu at 97%%"]]}]}}`,
			time.Now().UnixNano())
	}))
}

// newFakeLLM returns an httptest server that answers the OpenAI-compatible
// /v1/chat/completions shape pkg/model.OpenAIClient expects.
func newFakeLLM(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": reply}},
			},
		})
	}))
}

func newTestHandler(t *testing.T, lokiURL, llmURL string) *handler {
	t.Helper()
	llm := model.NewOpenAIClient(model.OpenAIClientConfig{
		Endpoint: llmURL,
		Model:    "test-model",
		Backend:  "test",
	})
	return &handler{
		loki:       aiops.NewClient(lokiURL),
		summarizer: aiops.NewSummarizer(llm),
		lookback:   5 * time.Minute,
		limit:      50,
	}
}

func validAlertmanagerPayload(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{
		"version": "4",
		"status":  "firing",
		"alerts": []map[string]any{
			{
				"status": "firing",
				"labels": map[string]string{
					"alertname": "cpu_high",
					"host":      "test-host",
				},
				"annotations": map[string]string{
					"summary": "CPU usage above threshold",
				},
				"startsAt": time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
				"endsAt":   "0001-01-01T00:00:00Z",
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// TestHandleAlertmanagerWebhook_EndToEnd exercises the full path: webhook
// body in -> ParseWebhook -> Loki query -> LLM summarize -> JSON response
// out, against fake Loki and fake LLM servers (no real infra touched).
func TestHandleAlertmanagerWebhook_EndToEnd(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "CPU 持續偏高，log 顯示已達 97%，建議檢查該主機負載來源。")
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(string(validAlertmanagerPayload(t))))
	rec := httptest.NewRecorder()

	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Results []alertResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if len(out.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(out.Results))
	}
	r := out.Results[0]
	if r.Error != "" {
		t.Fatalf("unexpected error in result: %s", r.Error)
	}
	if r.Host != "test-host" {
		t.Errorf("host = %q, want %q", r.Host, "test-host")
	}
	if !strings.Contains(r.Summary, "CPU") {
		t.Errorf("summary = %q, expected it to contain the fake LLM's reply", r.Summary)
	}
}

// TestHandleAlertmanagerWebhook_MalformedBody confirms a bad payload is
// rejected with 400 before any Loki/LLM call is attempted.
func TestHandleAlertmanagerWebhook_MalformedBody(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader("not json"))
	rec := httptest.NewRecorder()

	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleAlertmanagerWebhook_MissingHostLabel confirms an alert with no
// host/instance label surfaces as a per-alert error in the response body,
// not a request-level failure — one bad alert in a batch shouldn't blank
// out results for the others.
func TestHandleAlertmanagerWebhook_MissingHostLabel(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "unused")
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)

	payload := map[string]any{
		"version": "4",
		"status":  "firing",
		"alerts": []map[string]any{
			{
				"status":   "firing",
				"labels":   map[string]string{"alertname": "mystery_alert"},
				"startsAt": time.Now().Format(time.RFC3339),
			},
		},
	}
	b, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()

	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-alert errors shouldn't fail the request)", rec.Code)
	}
	var out struct {
		Results []alertResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Results) != 1 || out.Results[0].Error == "" {
		t.Fatalf("expected one result with a non-empty error, got %+v", out.Results)
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// Cloud escalation
// ══════════════════════════════════════════════════════════════════════════════

// newFakeCloud returns an httptest server answering the Anthropic Messages
// API shape, so escalation tests don't touch the real Anthropic API.
func newFakeCloud(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": reply}},
		})
	}))
}

// TestSummarizeOne_EscalatesToCloud_LocalRequestsIt confirms that when the
// local model's structured reply sets escalate=true, the handler re-runs
// the analysis against the configured cloud model and reports the cloud
// result rather than the local one.
func TestSummarizeOne_EscalatesToCloud_LocalRequestsIt(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary": "local guess", "confidence": "low", "escalate": true, "reason": "log 內容不足以判斷"}`)
	defer llmSrv.Close()
	cloudSrv := newFakeCloud(t, "cloud 深度分析結果")
	defer cloudSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: cloudSrv.URL, APIKey: "k", Model: "m"})

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.AnalyzedBy != "cloud" {
		t.Errorf("AnalyzedBy = %q, want %q", res.AnalyzedBy, "cloud")
	}
	if res.Summary != "cloud 深度分析結果" {
		t.Errorf("Summary = %q, want the cloud reply", res.Summary)
	}
}

// TestSummarizeOne_EscalatesToCloud_AlwaysCloudRule confirms the
// operator-defined always_cloud rule escalates even when the local model
// itself is confident and didn't ask to escalate.
func TestSummarizeOne_EscalatesToCloud_AlwaysCloudRule(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary": "local answer", "confidence": "high", "escalate": false, "reason": "clear"}`)
	defer llmSrv.Close()
	cloudSrv := newFakeCloud(t, "cloud answer")
	defer cloudSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: cloudSrv.URL, APIKey: "k", Model: "m"})
	h.escalation = config.EscalationConfig{AlwaysCloud: []string{"cpu_high"}}

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.AnalyzedBy != "cloud" {
		t.Errorf("AnalyzedBy = %q, want %q (always_cloud rule should override a confident local result)", res.AnalyzedBy, "cloud")
	}
}

// TestSummarizeOne_NoEscalation_StaysLocal confirms a confident local
// result with no matching rule is returned as-is, with no call to Cloud.
func TestSummarizeOne_NoEscalation_StaysLocal(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary": "local answer", "confidence": "high", "escalate": false, "reason": "clear"}`)
	defer llmSrv.Close()

	cloudCalled := false
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudCalled = true
		w.WriteHeader(500)
	}))
	defer cloudSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: cloudSrv.URL, APIKey: "k", Model: "m"})

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.AnalyzedBy != "local" {
		t.Errorf("AnalyzedBy = %q, want %q", res.AnalyzedBy, "local")
	}
	if cloudCalled {
		t.Error("cloud endpoint should not be called when nothing triggers escalation")
	}
}

// TestSummarizeOne_EscalationFails_FallsBackToLocal confirms that when
// escalation is triggered but the cloud call itself fails, the alert
// still reports the local result instead of erroring out entirely.
func TestSummarizeOne_EscalationFails_FallsBackToLocal(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary": "local answer", "confidence": "low", "escalate": true, "reason": "unsure"}`)
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = model.NewAnthropicClient(model.AnthropicClientConfig{Endpoint: "http://127.0.0.1:19999", APIKey: "k", Model: "m"})

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.AnalyzedBy != "local" {
		t.Errorf("AnalyzedBy = %q, want %q (cloud call failed, should fall back)", res.AnalyzedBy, "local")
	}
	if res.Summary != "local answer" {
		t.Errorf("Summary = %q, want the local fallback", res.Summary)
	}
	if res.Error != "" {
		t.Errorf("Error = %q, a failed escalation should not fail the whole alert", res.Error)
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// RAG retrieval
// ══════════════════════════════════════════════════════════════════════════════

// fakeRAGStore is an in-memory rag.Store for tests that don't need a real
// Postgres — it just returns whatever records were configured.
type fakeRAGStore struct {
	records []rag.Record
	err     error
	// searchCalled records whether Search was actually invoked, so tests
	// can confirm retrieval is skipped when RAG isn't wired up.
	searchCalled bool
}

func (f *fakeRAGStore) Search(ctx context.Context, embedding []float32, topK int) ([]rag.Record, error) {
	f.searchCalled = true
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}
func (f *fakeRAGStore) Insert(ctx context.Context, rec rag.Record, embedding []float32) error {
	return fmt.Errorf("not implemented in fake")
}
func (f *fakeRAGStore) Close() error { return nil }

func TestSummarizeOne_RAGContext_InjectedIntoPrompt(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()

	var receivedPrompt string
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []model.Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				receivedPrompt = m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"summary":"ok","confidence":"high","escalate":false,"reason":"x"}`}}},
		})
	}))
	defer llmSrv.Close()

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2}}},
		})
	}))
	defer embedSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	store := &fakeRAGStore{records: []rag.Record{
		{AlertName: "InstanceDown", Host: "192.0.2.7", Resolution: "舊測試機殘留 target，已下線", CreatedAt: time.Now()},
	}}
	h.rag = store
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "bge-m3")
	h.ragTopK = 3

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	h.summarizeOne(alert)

	if !store.searchCalled {
		t.Fatal("expected rag.Store.Search to be called")
	}
	if !strings.Contains(receivedPrompt, "過去類似事件") || !strings.Contains(receivedPrompt, "舊測試機殘留 target") {
		t.Errorf("prompt sent to LLM missing RAG context: %q", receivedPrompt)
	}
}

func TestSummarizeOne_RAGDisabled_NoSearchCalled(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"ok","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL) // h.rag left nil

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	// No assertion beyond "didn't panic / didn't error" — the real check
	// is that retrieveRAGContext's nil guard is exercised, which the fake
	// store in the other test proves is otherwise reachable.
}
