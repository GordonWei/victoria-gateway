// improvements_test.go covers the 2026-08-31 batch: async webhook mode,
// notification routing wiring, similar-incident references, the
// /incidents web pages, and the tracker selection logic.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/notify"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// captureChannel is a notify.Channel that records messages for
// assertions; safe for concurrent Send calls (async mode delivers from
// goroutines).
type captureChannel struct {
	name string
	mu   sync.Mutex
	msgs []notify.Message
}

func (c *captureChannel) Name() string { return c.name }
func (c *captureChannel) Send(msg notify.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
	return nil
}
func (c *captureChannel) messages() []notify.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]notify.Message{}, c.msgs...)
}

func newCaptureRouter(t *testing.T, ch *captureChannel) *notify.Router {
	t.Helper()
	r, err := notify.NewRouter([]notify.Channel{ch}, []notify.Route{{Default: true, Channels: []string{ch.name}}}, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func TestHandleAlertmanagerWebhook_AsyncMode(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "async 分析結果")
	defer llmSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.async = true
	ch := &captureChannel{name: "cap"}
	h.notifier = newCaptureRouter(t, ch)

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(string(validAlertmanagerPayload(t))))
	rec := httptest.NewRecorder()
	h.handleAlertmanagerWebhook(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status   string `json:"status"`
		Accepted int    `json:"accepted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Status != "accepted" || out.Accepted != 1 {
		t.Errorf("response = %+v, want accepted/1", out)
	}

	// The analysis runs in the background; inFlight is the same signal
	// graceful shutdown drains on.
	h.inFlight.Wait()
	msgs := ch.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 notification after background analysis, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Summary, "async") {
		t.Errorf("notification summary = %q", msgs[0].Summary)
	}
}

func TestSummarizeOne_SimilarIncidents_ThresholdAndURLs(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "摘要")
	defer llmSrv.Close()
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1, 0.2}}}})
	}))
	defer embedSrv.Close()

	created := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.rag = &fakeRAGStore{records: []rag.Record{
		{ID: 8, AlertName: "InstanceDown", Host: "192.0.2.7", Resolution: "r", CreatedAt: created, Similarity: 0.91, GiteaIssueNumber: 12},
		{ID: 9, AlertName: "InstanceDown", Host: "192.0.2.8", Resolution: "r", CreatedAt: created, Similarity: 0.80},
		{ID: 10, AlertName: "OtherAlert", Host: "x", Resolution: "r", CreatedAt: created, Similarity: 0.40},
	}}
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "test-embed", "")
	h.ragTopK = 3
	h.ragShowSimilar = true
	h.ragSimThreshold = 0.75
	h.publicBaseURL = "http://vg.example:8090"
	h.issueURL = func(n int64) string { return fmt.Sprintf("https://gitea.example/o/r/issues/%d", n) }

	res := h.summarizeOne(alertFixture(t))
	if res.Error != "" {
		t.Fatalf("summarizeOne error: %s", res.Error)
	}
	if len(res.similar) != 2 {
		t.Fatalf("similar = %+v, want 2 entries (0.40 filtered out)", res.similar)
	}
	if res.similar[0].URL != "https://gitea.example/o/r/issues/12" {
		t.Errorf("issue-linked record URL = %q", res.similar[0].URL)
	}
	if res.similar[1].URL != "http://vg.example:8090/incidents/9" {
		t.Errorf("non-issue record URL = %q", res.similar[1].URL)
	}
	if res.similar[0].Date != "2026-07-15" {
		t.Errorf("date = %q", res.similar[0].Date)
	}
}

func TestSummarizeOne_SimilarIncidents_DisabledByConfig(t *testing.T) {
	lokiSrv := newFakeLoki(t)
	defer lokiSrv.Close()
	llmSrv := newFakeLLM(t, "摘要")
	defer llmSrv.Close()
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1}}}})
	}))
	defer embedSrv.Close()

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.rag = &fakeRAGStore{records: []rag.Record{{ID: 1, AlertName: "A", Host: "h", Resolution: "r", Similarity: 0.99}}}
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "m", "")
	h.ragShowSimilar = false // rag.show_similar_in_notification: false
	h.ragSimThreshold = 0.75

	res := h.summarizeOne(alertFixture(t))
	if len(res.similar) != 0 {
		t.Errorf("similar should be empty when show_similar_in_notification is false, got %+v", res.similar)
	}
}

// alertFixture is a minimal firing alert for direct summarizeOne calls.
func alertFixture(t *testing.T) aiops.Alert {
	t.Helper()
	return aiops.Alert{
		Status:      "firing",
		Labels:      map[string]string{"alertname": "cpu_high", "host": "test-host"},
		Annotations: map[string]string{"summary": "CPU usage above threshold"},
		StartsAt:    time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
		Fingerprint: "fixture-fp",
	}
}

func TestIncidentsList_RendersConfirmed(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{records: []rag.Record{
		{ID: 3, AlertName: "InstanceDown", Host: "192.0.2.7", Resolution: "舊測試機殘留 target，已下線", CreatedAt: time.Now()},
	}}}
	req := httptest.NewRequest(http.MethodGet, "/incidents", nil)
	rec := httptest.NewRecorder()
	h.handleIncidentsList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"InstanceDown", "192.0.2.7", "/incidents/3"} {
		if !strings.Contains(body, want) {
			t.Errorf("list page missing %q", want)
		}
	}
}

func TestIncidentsList_RejectsBadLimit(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{}}
	req := httptest.NewRequest(http.MethodGet, "/incidents?limit=zero", nil)
	rec := httptest.NewRecorder()
	h.handleIncidentsList(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestIncidentDetail_FoundAndNotFound(t *testing.T) {
	h := &handler{rag: &fakeRAGStore{records: []rag.Record{
		{ID: 5, AlertName: "DiskFull", Host: "h1", Resolution: "清掉 /var/log", Summary: "s", CreatedAt: time.Now()},
	}}}

	req := httptest.NewRequest(http.MethodGet, "/incidents/5", nil)
	rec := httptest.NewRecorder()
	h.handleIncidentDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "清掉 /var/log") {
		t.Error("detail page missing resolution text")
	}

	for _, path := range []string{"/incidents/999", "/incidents/abc"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.handleIncidentDetail(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestBuildTracker_Selection(t *testing.T) {
	gitea := &config.GiteaConfig{Endpoint: "https://g", Token: "t", Owner: "o", Repo: "r"}
	github := &config.GitHubConfig{Token: "t", Owner: "o", Repo: "r"}

	if tr, err := buildTracker(nil); err != nil || tr != nil {
		t.Errorf("nil rag config: got (%v, %v), want (nil, nil)", tr, err)
	}
	if tr, err := buildTracker(&config.RAGConfig{}); err != nil || tr != nil {
		t.Errorf("no tracker configured: got (%v, %v), want (nil, nil)", tr, err)
	}
	if tr, err := buildTracker(&config.RAGConfig{Gitea: gitea}); err != nil || tr == nil {
		t.Errorf("gitea only: got (%v, %v), want a tracker", tr, err)
	}
	if tr, err := buildTracker(&config.RAGConfig{GitHub: github}); err != nil || tr == nil {
		t.Errorf("github only: got (%v, %v), want a tracker", tr, err)
	}
	if _, err := buildTracker(&config.RAGConfig{Gitea: gitea, GitHub: github}); err == nil {
		t.Error("both trackers set: expected an error")
	}
}

func TestBuildNotifier_BackCompat(t *testing.T) {
	// Only the top-level telegram block → enabled, single implicit route.
	cfg := &config.Config{Telegram: config.TelegramConfig{BotToken: "t", ChatID: 1}}
	r, err := buildNotifier(cfg, nil)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if !r.Enabled() {
		t.Error("telegram-only config should yield an enabled router")
	}

	// Nothing configured → inert router, no error.
	r2, err := buildNotifier(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if r2.Enabled() {
		t.Error("empty config should yield a disabled router")
	}

	// notifications with an explicit default + top-level telegram → the
	// top-level block is ignored (no duplicate-channel error, still enabled).
	cfg3 := &config.Config{
		Telegram: config.TelegramConfig{BotToken: "t", ChatID: 1},
		Notifications: &config.NotificationsConfig{
			Channels: []config.NotifyChannelConfig{{Name: "ops", Type: "telegram", BotToken: "t2", ChatID: 2}},
			Routes:   []config.NotifyRouteConfig{{Default: true, Channels: []string{"ops"}}},
		},
	}
	r3, err := buildNotifier(cfg3, nil)
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if !r3.Enabled() {
		t.Error("explicit-default config should yield an enabled router")
	}
}
