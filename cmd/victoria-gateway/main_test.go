package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/gitea"
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
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"host":"test-host"},"values":[["%d","cpu at 97%%"]]}]}}`,
			time.Now().UnixNano())
	}))
}

// newFakeLLM returns an httptest server that answers the OpenAI-compatible
// /v1/chat/completions shape pkg/model.OpenAIClient expects.
func newFakeLLM(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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
	// insertPendingCalled/lastPending record whether/what captureIncident
	// wrote, so tests can confirm auto-capture actually happens.
	insertPendingCalled bool
	lastPending         rag.Record
	// distinctModels is what DistinctEmbeddingModels returns — tests that
	// don't care about drift checking leave this nil (no rows yet).
	distinctModels []string
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
func (f *fakeRAGStore) InsertPending(ctx context.Context, rec rag.Record, embedding []float32) (int64, error) {
	f.insertPendingCalled = true
	f.lastPending = rec
	return 1, nil
}
func (f *fakeRAGStore) PendingWithGiteaIssue(ctx context.Context) ([]rag.Record, error) {
	return nil, fmt.Errorf("not implemented in fake")
}
func (f *fakeRAGStore) Confirm(ctx context.Context, id int64, resolution string) error {
	return fmt.Errorf("not implemented in fake")
}
func (f *fakeRAGStore) GetConfirmed(ctx context.Context, id int64) (rag.Record, error) {
	for _, r := range f.records {
		if r.ID == id {
			return r, nil
		}
	}
	return rag.Record{}, rag.ErrNotFound
}
func (f *fakeRAGStore) ListConfirmed(ctx context.Context, filter rag.ListFilter, limit int) ([]rag.Record, error) {
	if f.err != nil {
		return nil, f.err
	}
	matched := f.records
	if filter.AlertName != "" || filter.Host != "" {
		matched = nil
		for _, r := range f.records {
			if filter.AlertName != "" && !strings.Contains(strings.ToLower(r.AlertName), strings.ToLower(filter.AlertName)) {
				continue
			}
			if filter.Host != "" && !strings.Contains(strings.ToLower(r.Host), strings.ToLower(filter.Host)) {
				continue
			}
			matched = append(matched, r)
		}
	}
	if limit > 0 && len(matched) > limit {
		return matched[:limit], nil
	}
	return matched, nil
}
func (f *fakeRAGStore) DistinctEmbeddingModels(ctx context.Context) ([]string, error) {
	return f.distinctModels, nil
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
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				receivedPrompt = m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"summary":"ok","confidence":"high","escalate":false,"reason":"x"}`}}},
		})
	}))
	defer llmSrv.Close()

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2}}},
		})
	}))
	defer embedSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	store := &fakeRAGStore{records: []rag.Record{
		{AlertName: "InstanceDown", Host: "172.16.100.7", Resolution: "舊測試機殘留 target，已下線", CreatedAt: time.Now()},
	}}
	h.rag = store
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "bge-m3", "")
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

// ══════════════════════════════════════════════════════════════════════════════
// RAG capture (pending record + optional Gitea issue)
// ══════════════════════════════════════════════════════════════════════════════

func TestSummarizeOne_CapturesPendingRecord(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"CPU 過載","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1}}}})
	}))
	defer embedSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	store := &fakeRAGStore{}
	h.rag = store
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "bge-m3", "")

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	h.summarizeOne(alert)

	if !store.insertPendingCalled {
		t.Fatal("expected InsertPending to be called after a successful analysis")
	}
	if store.lastPending.AlertName != "cpu_high" || store.lastPending.Summary != "CPU 過載" {
		t.Errorf("unexpected pending record: %+v", store.lastPending)
	}
	if store.lastPending.GiteaIssueNumber != 0 {
		t.Errorf("expected no Gitea issue number when Gitea isn't configured, got %d", store.lastPending.GiteaIssueNumber)
	}
}

func TestSummarizeOne_CapturesWithGiteaIssue(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"CPU 過載","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1}}}})
	}))
	defer embedSrv.Close()
	var createdTitle string
	giteaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		createdTitle = body.Title
		_ = json.NewEncoder(w).Encode(gitea.Issue{Number: 7, State: "open"})
	}))
	defer giteaSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	store := &fakeRAGStore{}
	h.rag = store
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "bge-m3", "")
	h.tracker = gitea.NewClient(gitea.ClientConfig{Endpoint: giteaSrv.URL, Token: "t", Owner: "admin", Repo: "victoria-gateway-incidents"})

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	h.summarizeOne(alert)

	if !strings.Contains(createdTitle, "cpu_high") {
		t.Errorf("gitea issue title = %q, expected it to mention the alertname", createdTitle)
	}
	if store.lastPending.GiteaIssueNumber != 7 {
		t.Errorf("pending record GiteaIssueNumber = %d, want 7", store.lastPending.GiteaIssueNumber)
	}
}

func TestSummarizeOne_GiteaCreateFails_StillCapturesPending(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"CPU 過載","confidence":"high","escalate":false,"reason":"x"}`)
	defer llmSrv.Close()
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1}}}})
	}))
	defer embedSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	store := &fakeRAGStore{}
	h.rag = store
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "bge-m3", "")
	h.tracker = gitea.NewClient(gitea.ClientConfig{Endpoint: "http://127.0.0.1:19999", Token: "t", Owner: "admin", Repo: "r"})

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if res.Error != "" {
		t.Fatalf("a failed gitea issue creation must not fail the alert: %s", res.Error)
	}
	if !store.insertPendingCalled {
		t.Fatal("expected the pending record to still be captured even though issue creation failed")
	}
	if store.lastPending.GiteaIssueNumber != 0 {
		t.Errorf("expected GiteaIssueNumber 0 when issue creation failed, got %d", store.lastPending.GiteaIssueNumber)
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// Dedup (Alertmanager retry storms / resolved deliveries)
// ══════════════════════════════════════════════════════════════════════════════

func alertmanagerPayloadWithFingerprint(status, fingerprint string) []byte {
	payload := map[string]any{
		"version": "4",
		"status":  status,
		"alerts": []map[string]any{
			{
				"status":      status,
				"labels":      map[string]string{"alertname": "cpu_high", "host": "test-host"},
				"startsAt":    time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
				"fingerprint": fingerprint,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// TestHandleAlertmanagerWebhook_DuplicateFingerprint_OnlyProcessedOnce
// simulates the actual bug found in production: Alertmanager retrying a
// notification it considers failed (its own webhook timeout/dispatch
// loop, not anything wrong with the payload) by re-POSTing the same
// still-firing alert. Without dedup this ran the full analysis — and
// filed a new Gitea issue — once per retry, for what's a single episode.
func TestHandleAlertmanagerWebhook_DuplicateFingerprint_OnlyProcessedOnce(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()

	var llmCalls int32
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"summary":"ok","confidence":"high","escalate":false,"reason":"x"}`}}},
		})
	}))
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	body := alertmanagerPayloadWithFingerprint("firing", "dup-fp-001")

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.handleAlertmanagerWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d, want 200", i, rec.Code)
		}
	}

	if got := atomic.LoadInt32(&llmCalls); got != 1 {
		t.Errorf("LLM was called %d times for 3 deliveries of the same fingerprint, want 1 (the retries should have been deduped)", got)
	}
}

// TestHandleAlertmanagerWebhook_DifferentFingerprints_BothProcessed
// confirms dedup is scoped to the fingerprint, not e.g. the alertname —
// two genuinely different alerts (even with the same alertname/host, as
// can happen across separate episodes) must both go through.
func TestHandleAlertmanagerWebhook_DifferentFingerprints_BothProcessed(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()

	var llmCalls int32
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"summary":"ok","confidence":"high","escalate":false,"reason":"x"}`}}},
		})
	}))
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)

	for _, fp := range []string{"fp-a", "fp-b"} {
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(alertmanagerPayloadWithFingerprint("firing", fp)))
		rec := httptest.NewRecorder()
		h.handleAlertmanagerWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("fingerprint %s: status = %d, want 200", fp, rec.Code)
		}
	}

	if got := atomic.LoadInt32(&llmCalls); got != 2 {
		t.Errorf("LLM was called %d times for 2 distinct fingerprints, want 2", got)
	}
}

// TestHandleAlertmanagerWebhook_ResolvedAlert_SkipsAnalysis confirms a
// "resolved" delivery never reaches the LLM/capture path — there's
// nothing new to diagnose about an alert that just stopped, and
// Alertmanager's own telegram_configs already tells the human it
// resolved (separately from this webhook).
func TestHandleAlertmanagerWebhook_ResolvedAlert_SkipsAnalysis(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()

	llmCalled := false
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalled = true
		w.WriteHeader(500)
	}))
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(alertmanagerPayloadWithFingerprint("resolved", "resolved-fp-001")))
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if llmCalled {
		t.Error("a resolved delivery should never reach the LLM")
	}
}

// TestHandleAlertmanagerWebhook_ResolvedThenRefired_ProcessedAsNewEpisode
// confirms resolving an alert clears its dedup entry, so the same
// fingerprint firing again later (a genuinely new episode, not a retry
// of the old one) still gets analyzed rather than being silently
// swallowed by the dedup window.
func TestHandleAlertmanagerWebhook_ResolvedThenRefired_ProcessedAsNewEpisode(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()

	var llmCalls int32
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"summary":"ok","confidence":"high","escalate":false,"reason":"x"}`}}},
		})
	}))
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	fp := "recurring-fp-001"

	post := func(status string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(alertmanagerPayloadWithFingerprint(status, fp)))
		rec := httptest.NewRecorder()
		h.handleAlertmanagerWebhook(rec, req)
		return rec.Code
	}

	if code := post("firing"); code != http.StatusOK {
		t.Fatalf("first firing: status = %d", code)
	}
	if code := post("resolved"); code != http.StatusOK {
		t.Fatalf("resolved: status = %d", code)
	}
	if code := post("firing"); code != http.StatusOK {
		t.Fatalf("second firing: status = %d", code)
	}

	if got := atomic.LoadInt32(&llmCalls); got != 2 {
		t.Errorf("LLM was called %d times across fire->resolve->fire, want 2 (each firing analyzed once, resolve skipped)", got)
	}
}

func TestClaimFingerprint_EmptyAlwaysClaims(t *testing.T) {
	h := &handler{}
	if !h.claimFingerprint("") {
		t.Error("an empty fingerprint should always claim true (defensive default, not a dedup key)")
	}
	if !h.claimFingerprint("") {
		t.Error("calling again with empty fingerprint should still claim true")
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// Webhook auth
// ══════════════════════════════════════════════════════════════════════════════

func TestHandleAlertmanagerWebhook_NoAuthConfigured_AnyoneCanCall(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "ok")
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL) // h.webhookAuth left nil

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(validAlertmanagerPayload(t)))
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no auth configured = no auth required)", rec.Code)
	}
}

func TestHandleAlertmanagerWebhook_AuthConfigured_RejectsMissingCreds(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")
	h.webhookAuth = &config.WebhookAuthConfig{Username: "alertmanager", Password: "s3cret"}

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(validAlertmanagerPayload(t)))
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAlertmanagerWebhook_AuthConfigured_RejectsWrongPassword(t *testing.T) {
	h := newTestHandler(t, "http://unused.invalid", "http://unused.invalid")
	h.webhookAuth = &config.WebhookAuthConfig{Username: "alertmanager", Password: "s3cret"}

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(validAlertmanagerPayload(t)))
	req.SetBasicAuth("alertmanager", "wrong")
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleAlertmanagerWebhook_AuthConfigured_AcceptsCorrectCreds(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "ok")
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.webhookAuth = &config.WebhookAuthConfig{Username: "alertmanager", Password: "s3cret"}

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(validAlertmanagerPayload(t)))
	req.SetBasicAuth("alertmanager", "s3cret")
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// buildTracker
// ══════════════════════════════════════════════════════════════════════════════

func TestBuildTracker_NilRAGConfig(t *testing.T) {
	tr, err := buildTracker(nil)
	if err != nil || tr != nil {
		t.Errorf("buildTracker(nil) = (%v, %v), want (nil, nil)", tr, err)
	}
}

func TestBuildTracker_NeitherConfigured(t *testing.T) {
	tr, err := buildTracker(&config.RAGConfig{})
	if err != nil || tr != nil {
		t.Errorf("buildTracker with neither gitea nor github set = (%v, %v), want (nil, nil)", tr, err)
	}
}

func TestBuildTracker_BothConfigured_Errors(t *testing.T) {
	ragCfg := &config.RAGConfig{
		Gitea:  &config.GiteaConfig{Endpoint: "https://gitea.example.com", Owner: "o", Repo: "r"},
		GitHub: &config.GitHubConfig{Owner: "o", Repo: "r"},
	}
	if _, err := buildTracker(ragCfg); err == nil {
		t.Error("expected an error when both rag.gitea and rag.github are configured")
	}
}

func TestBuildTracker_GiteaOnly(t *testing.T) {
	ragCfg := &config.RAGConfig{Gitea: &config.GiteaConfig{Endpoint: "https://gitea.example.com", Owner: "o", Repo: "r"}}
	tr, err := buildTracker(ragCfg)
	if err != nil {
		t.Fatalf("buildTracker: %v", err)
	}
	if tr == nil {
		t.Fatal("expected a non-nil tracker when rag.gitea is set")
	}
}

func TestBuildTracker_GitHubOnly(t *testing.T) {
	ragCfg := &config.RAGConfig{GitHub: &config.GitHubConfig{Owner: "o", Repo: "r"}}
	tr, err := buildTracker(ragCfg)
	if err != nil {
		t.Fatalf("buildTracker: %v", err)
	}
	if tr == nil {
		t.Fatal("expected a non-nil tracker when rag.github is set")
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// Concurrent alert processing
// ══════════════════════════════════════════════════════════════════════════════

// multiAlertPayload builds one Alertmanager delivery carrying several
// distinct alerts (distinct fingerprints and hosts, so none are deduped
// against each other), to exercise the fan-out in
// handleAlertmanagerWebhook.
func multiAlertPayload(n int) []byte {
	alerts := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		alerts[i] = map[string]any{
			"status":      "firing",
			"labels":      map[string]string{"alertname": "cpu_high", "host": fmt.Sprintf("host-%d", i)},
			"startsAt":    time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
			"fingerprint": fmt.Sprintf("fp-%d", i),
		}
	}
	b, _ := json.Marshal(map[string]any{"version": "4", "status": "firing", "alerts": alerts})
	return b
}

// TestHandleAlertmanagerWebhook_MultipleAlerts_ProcessedConcurrently
// confirms a single payload with several alerts doesn't serialize on the
// LLM: with N concurrent slow calls capped at a shared latency, total
// wall time should look like one call's worth of latency, not N of them.
func TestHandleAlertmanagerWebhook_MultipleAlerts_ProcessedConcurrently(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()

	const perCallDelay = 150 * time.Millisecond
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(perCallDelay)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"summary":"ok","confidence":"high","escalate":false,"reason":"x"}`}}},
		})
	}))
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	const n = 5

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", bytes.NewReader(multiAlertPayload(n)))
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Serial processing would take roughly n*perCallDelay (750ms here);
	// concurrent processing should stay close to one call's delay. Give
	// generous headroom above a single call to absorb scheduling noise
	// without the check being meaningless.
	if elapsed >= n*perCallDelay {
		t.Errorf("elapsed = %v, want well under %v (n=%d alerts serialized instead of running concurrently)", elapsed, n*perCallDelay, n)
	}

	var body struct {
		Results []alertResult `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Results) != n {
		t.Fatalf("got %d results, want %d", len(body.Results), n)
	}
	for i, res := range body.Results {
		wantHost := fmt.Sprintf("host-%d", i)
		if res.Host != wantHost {
			t.Errorf("results[%d].Host = %q, want %q (result order should match request order)", i, res.Host, wantHost)
		}
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// Escalation rate limiting
// ══════════════════════════════════════════════════════════════════════════════

func TestAllowEscalation_UnlimitedByDefault(t *testing.T) {
	h := &handler{}
	for i := 0; i < 100; i++ {
		if !h.allowEscalation() {
			t.Fatalf("call %d: allowEscalation() = false, want true (max_per_hour 0 means unlimited)", i)
		}
	}
}

func TestAllowEscalation_CapsWithinWindow(t *testing.T) {
	h := &handler{escalation: config.EscalationConfig{MaxPerHour: 2}}
	if !h.allowEscalation() {
		t.Fatal("1st call: want true")
	}
	if !h.allowEscalation() {
		t.Fatal("2nd call: want true")
	}
	if h.allowEscalation() {
		t.Fatal("3rd call within the same hour: want false, the cap should hold")
	}
}

func TestAllowEscalation_ResetsAfterWindowExpires(t *testing.T) {
	h := &handler{escalation: config.EscalationConfig{MaxPerHour: 1}}
	if !h.allowEscalation() {
		t.Fatal("1st call: want true")
	}
	if h.allowEscalation() {
		t.Fatal("2nd call still within the window: want false")
	}
	// Simulate the window having expired instead of sleeping an hour.
	h.escalationWindowStart = time.Now().Add(-2 * time.Hour)
	if !h.allowEscalation() {
		t.Fatal("call after the window expired: want true, the cap should have reset")
	}
}

func TestSummarizeOne_EscalationRateLimited_StaysLocal(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, `{"summary":"local ok","confidence":"low","escalate":true,"reason":"need cloud"}`)
	defer llmSrv.Close()

	cloudCalled := false
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudCalled = true
		w.WriteHeader(500)
	}))
	defer cloudSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = model.NewOpenAIClient(model.OpenAIClientConfig{Endpoint: cloudSrv.URL, Model: "cloud-model", Backend: "test-cloud"})
	h.escalation = config.EscalationConfig{MaxPerHour: 1}
	h.escalationWindowStart = time.Now()
	h.escalationCount = 1 // already at the cap before this alert

	alert := aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "cpu_high", "host": "test-host"},
		StartsAt: time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	res := h.summarizeOne(alert)

	if cloudCalled {
		t.Error("cloud should not be called once escalation.max_per_hour is reached")
	}
	if res.AnalyzedBy != "local" {
		t.Errorf("AnalyzedBy = %q, want \"local\" (rate-limited escalation should fall back to the local result)", res.AnalyzedBy)
	}
	if res.Summary != "local ok" {
		t.Errorf("Summary = %q, want the local model's summary", res.Summary)
	}
}
