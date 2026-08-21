package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/orch-gateway/pkg/aiops"
	"github.com/gordonwei/orch-gateway/pkg/model"
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
