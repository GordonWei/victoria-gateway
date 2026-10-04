package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChannel records what it was asked to send and can be told to fail.
type fakeChannel struct {
	name  string
	err   error
	sent  []Message
	calls int
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Send(msg Message) error {
	f.calls++
	f.sent = append(f.sent, msg)
	return f.err
}

func TestRouter_FirstMatchWins(t *testing.T) {
	critical := &fakeChannel{name: "critical"}
	archive := &fakeChannel{name: "archive"}
	r, err := NewRouter(
		[]Channel{critical, archive},
		[]Route{
			{Matchers: map[string]string{"severity": "critical"}, Channels: []string{"critical"}},
			{Default: true, Channels: []string{"archive"}},
		}, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	r.Dispatch(Message{AlertName: "A"}, map[string]string{"severity": "critical"})
	r.Dispatch(Message{AlertName: "B"}, map[string]string{"severity": "warning"})

	if critical.calls != 1 || critical.sent[0].AlertName != "A" {
		t.Errorf("critical channel got %v", critical.sent)
	}
	if archive.calls != 1 || archive.sent[0].AlertName != "B" {
		t.Errorf("archive channel got %v", archive.sent)
	}
}

func TestRouter_GlobAndANDMatchers(t *testing.T) {
	ch := &fakeChannel{name: "ops"}
	r, err := NewRouter([]Channel{ch}, []Route{
		{Matchers: map[string]string{"severity": "critical", "alertname": "Instance*"}, Channels: []string{"ops"}},
	}, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	// Both matchers hit → delivered.
	r.Dispatch(Message{AlertName: "hit"}, map[string]string{"severity": "critical", "alertname": "InstanceDown"})
	// alertname matches but severity doesn't → AND semantics say no.
	r.Dispatch(Message{AlertName: "miss"}, map[string]string{"severity": "warning", "alertname": "InstanceDown"})

	if ch.calls != 1 || ch.sent[0].AlertName != "hit" {
		t.Errorf("ops channel got %v", ch.sent)
	}
}

func TestRouter_NoMatchNoDefault_Drops(t *testing.T) {
	ch := &fakeChannel{name: "ops"}
	r, err := NewRouter([]Channel{ch}, []Route{
		{Matchers: map[string]string{"severity": "critical"}, Channels: []string{"ops"}},
	}, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	r.Dispatch(Message{AlertName: "X"}, map[string]string{"severity": "info"})
	if ch.calls != 0 {
		t.Errorf("expected no delivery, got %d", ch.calls)
	}
}

func TestRouter_MultiChannel_OneFailureDoesNotStopOthers(t *testing.T) {
	bad := &fakeChannel{name: "bad", err: fmt.Errorf("boom")}
	good := &fakeChannel{name: "good"}
	var results []string
	r, err := NewRouter([]Channel{bad, good}, []Route{
		{Default: true, Channels: []string{"bad", "good"}},
	}, func(channel string, err error) {
		outcome := "ok"
		if err != nil {
			outcome = "fail"
		}
		results = append(results, channel+":"+outcome)
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	r.Dispatch(Message{AlertName: "X"}, nil)

	if good.calls != 1 {
		t.Errorf("good channel not called despite bad channel failing")
	}
	want := []string{"bad:fail", "good:ok"}
	if fmt.Sprint(results) != fmt.Sprint(want) {
		t.Errorf("onResult saw %v, want %v", results, want)
	}
}

func TestNewRouter_RejectsUndefinedChannel(t *testing.T) {
	_, err := NewRouter([]Channel{&fakeChannel{name: "a"}}, []Route{
		{Default: true, Channels: []string{"nope"}},
	}, nil)
	if err == nil {
		t.Error("expected error for route referencing undefined channel")
	}
}

func TestRouter_NilAndEmptyAreInert(t *testing.T) {
	var nilRouter *Router
	if nilRouter.Enabled() {
		t.Error("nil router must not be Enabled")
	}
	nilRouter.Dispatch(Message{}, nil) // must not panic

	empty, err := NewRouter(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if empty.Enabled() {
		t.Error("empty router must not be Enabled")
	}
}

func TestFormatTelegramText_SimilarSection(t *testing.T) {
	text := FormatTelegramText(Message{
		AlertName: "NodeExporterDown",
		Host:      "192.0.2.7",
		Summary:   "process 掛了",
		Similar: []SimilarIncident{
			{Ref: "#12 NodeExporterDown (192.0.2.6)", Date: "2026-07-15", URL: "https://gitea.example/issues/12"},
			{Ref: "incident/8 — InstanceDown (192.0.2.7)", Date: "2026-06-20", URL: "/incidents/8"},
		},
	})
	for _, want := range []string{"相似歷史事件", "#12 NodeExporterDown", "https://gitea.example/issues/12", "/incidents/8"} {
		if !strings.Contains(text, want) {
			t.Errorf("formatted text missing %q:\n%s", want, text)
		}
	}
}

func TestFormatTelegramText_PendingURL(t *testing.T) {
	text := FormatTelegramText(Message{
		AlertName:  "DiskSpace",
		Host:       "192.0.2.6",
		Summary:    "disk is filling up",
		PendingURL: "https://vg.example/pending/42",
	})
	if !strings.Contains(text, "https://vg.example/pending/42") {
		t.Errorf("formatted text missing the pending link:\n%s", text)
	}
	if !strings.Contains(text, "確認這筆") {
		t.Errorf("formatted text missing the pending-link label:\n%s", text)
	}
}

func TestFormatTelegramText_NoPendingURL_NoExtraSection(t *testing.T) {
	text := FormatTelegramText(Message{
		AlertName: "DiskSpace",
		Host:      "192.0.2.6",
		Summary:   "disk is filling up",
	})
	if strings.Contains(text, "確認這筆") {
		t.Errorf("expected no pending-link section when PendingURL is empty:\n%s", text)
	}
}

func TestFormatTelegramText_PendingURLAndSimilar_BothPresent(t *testing.T) {
	text := FormatTelegramText(Message{
		AlertName:  "DiskSpace",
		Host:       "192.0.2.6",
		Summary:    "disk is filling up",
		PendingURL: "https://vg.example/pending/42",
		Similar: []SimilarIncident{
			{Ref: "#12 DiskSpace (192.0.2.6)", Date: "2026-07-15", URL: "https://gitea.example/issues/12"},
		},
	})
	pendingIdx := strings.Index(text, "https://vg.example/pending/42")
	similarIdx := strings.Index(text, "相似歷史事件")
	if pendingIdx == -1 || similarIdx == -1 {
		t.Fatalf("expected both sections present:\n%s", text)
	}
	if pendingIdx > similarIdx {
		t.Errorf("expected the pending link (actionable on this alert) before the similar-incidents section (informational only):\n%s", text)
	}
}

func TestFormatTelegramText_TruncatesLongSummary(t *testing.T) {
	text := FormatTelegramText(Message{
		AlertName: "X",
		Host:      "h",
		Summary:   strings.Repeat("很長的分析內容 ", 2000),
	})
	if n := len([]rune(text)); n > telegramMaxLen {
		t.Errorf("formatted text is %d runes, over the %d cap", n, telegramMaxLen)
	}
	if !strings.Contains(text, "已截斷") {
		t.Error("truncated text missing the truncation marker")
	}
}

func TestTruncateTelegramHTML_DoesNotCutEntityOrTag(t *testing.T) {
	// Force the cut to land inside "&amp;" — the truncator must back off
	// to before the '&'.
	padding := strings.Repeat("a", 50)
	text := padding + "&amp;" + strings.Repeat("b", 100)
	cut := truncateTelegramHTML(text, 52+len([]rune(truncationMarker)))
	if strings.Contains(cut, "&a") && !strings.Contains(cut, "&amp;") {
		t.Errorf("cut landed inside an entity: %q", cut)
	}

	// A cut right after an opening <b> must close it.
	text2 := "<b>" + strings.Repeat("c", 200) + "</b>"
	cut2 := truncateTelegramHTML(text2, 50+len([]rune(truncationMarker)))
	if strings.Count(cut2, "<b>") != strings.Count(cut2, "</b>") {
		t.Errorf("unbalanced <b> after truncation: %q", cut2)
	}
}

func TestTelegramChannel_RetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"description":"internal"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ch := NewTelegramChannel("tg", "token", 1)
	ch.apiBase = srv.URL
	ch.sleep = func(time.Duration) {}

	if err := ch.Send(Message{AlertName: "X", Host: "h", Summary: "s"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 attempts (1 failure + 1 success), got %d", calls.Load())
	}
}

func TestTelegramChannel_DoesNotRetryBadRequest(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"can't parse entities"}`))
	}))
	defer srv.Close()

	ch := NewTelegramChannel("tg", "token", 1)
	ch.apiBase = srv.URL
	ch.sleep = func(time.Duration) {}

	if err := ch.Send(Message{AlertName: "X"}); err == nil {
		t.Fatal("expected error from a 400 response")
	}
	if calls.Load() != 1 {
		t.Errorf("a 400 must not be retried, got %d attempts", calls.Load())
	}
}

func TestWebhookChannel_PostsExpectedBody(t *testing.T) {
	var got webhookBody
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhookChannel("itsm", srv.URL, "", map[string]string{"Authorization": "Bearer x"})
	err := ch.Send(Message{
		AlertName:  "InstanceDown",
		Host:       "192.0.2.7",
		Summary:    "summary",
		AnalyzedBy: "local",
		Similar:    []SimilarIncident{{Ref: "#1 A (h)", Date: "2026-01-01", URL: "u"}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.AlertName != "InstanceDown" || got.AnalyzedBy != "local" || got.Host != "192.0.2.7" {
		t.Errorf("unexpected body: %+v", got)
	}
	if len(got.SimilarIncidents) != 1 || got.SimilarIncidents[0].Ref != "#1 A (h)" {
		t.Errorf("similar incidents not delivered: %+v", got.SimilarIncidents)
	}
	if gotAuth != "Bearer x" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
}

func TestWebhookChannel_RetriesOn5xx_FailsOn4xx(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ch := NewWebhookChannel("hook", srv.URL, "", nil)
	ch.sleep = func(time.Duration) {}
	if err := ch.Send(Message{AlertName: "X"}); err != nil {
		t.Fatalf("Send after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected retry after 502, got %d attempts", calls.Load())
	}

	var calls4 atomic.Int64
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls4.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv4.Close()
	ch4 := NewWebhookChannel("hook", srv4.URL, "", nil)
	ch4.sleep = func(time.Duration) {}
	if err := ch4.Send(Message{AlertName: "X"}); err == nil {
		t.Fatal("expected error from 403")
	}
	if calls4.Load() != 1 {
		t.Errorf("a 403 must not be retried, got %d attempts", calls4.Load())
	}
}

func TestFormatTelegramText_EscalatedTo(t *testing.T) {
	got := FormatTelegramText(Message{AlertName: "A", Host: "h", Summary: "s", AnalyzedBy: "cloud", EscalatedTo: "aws<prod>"})
	if !strings.Contains(got, "已升級至 cloud model（aws&lt;prod&gt;）深度分析") {
		t.Errorf("text = %q, want the escaped target name", got)
	}
	// Without a target name the line is exactly what it always was.
	got = FormatTelegramText(Message{AlertName: "A", Host: "h", Summary: "s", AnalyzedBy: "cloud"})
	if !strings.Contains(got, "<i>已升級至 cloud model 深度分析</i>") {
		t.Errorf("text = %q, want the unchanged legacy line", got)
	}
}

func TestWebhookChannel_CarriesEscalatedTo(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
	}))
	defer srv.Close()
	ch := NewWebhookChannel("itsm", srv.URL, "", nil)
	if err := ch.Send(Message{AlertName: "A", AnalyzedBy: "cloud", EscalatedTo: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	if raw["escalated_to"] != "bedrock" || raw["analyzed_by"] != "cloud" {
		t.Errorf("body = %v", raw)
	}
	raw = nil
	if err := ch.Send(Message{AlertName: "A", AnalyzedBy: "local"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["escalated_to"]; ok {
		t.Errorf("escalated_to present on a local result: %v", raw)
	}
}

func TestFormatTelegramText_MitigationNote(t *testing.T) {
	text := FormatTelegramText(Message{
		AlertName:      "LambdaErrors",
		Host:           "checkout",
		Summary:        "bad deploy",
		AnalyzedBy:     "cloud",
		EscalatedTo:    "aws",
		MitigationNote: "已附建議處置（AWS DevOps Agent mitigation plan）於 issue #42：https://git.example/issues/42?a=1&b=2",
		PendingURL:     "https://vg.example/pending/7",
	})
	want := "\n\n🛠 已附建議處置（AWS DevOps Agent mitigation plan）於 issue #42：https://git.example/issues/42?a=1&amp;b=2"
	if !strings.Contains(text, want) {
		t.Errorf("formatted text missing the escaped mitigation note:\n%s", text)
	}
	if strings.Index(text, "🛠") > strings.Index(text, "確認這筆") {
		t.Errorf("mitigation note should come before the confirm link:\n%s", text)
	}
	if strings.Contains(FormatTelegramText(Message{AlertName: "a", Summary: "s"}), "🛠") {
		t.Error("note rendered without a MitigationNote")
	}
}

// ── body_template: chat-product webhook schemas ─────────────────────
//
// Local httptest servers only. These prove the request body a Slack- or
// Teams-shaped template produces; they say nothing about whether the real
// services accept it.

func newTemplateChannel(t *testing.T, url, tmpl string) *WebhookChannel {
	t.Helper()
	parsed, err := ParseBodyTemplate(tmpl)
	if err != nil {
		t.Fatalf("ParseBodyTemplate: %v", err)
	}
	ch := NewWebhookChannel("chat", url, "", nil)
	ch.SetBodyTemplate(parsed)
	ch.sleep = func(time.Duration) {}
	return ch
}

func TestWebhookChannel_BodyTemplate_SlackShape(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := newTemplateChannel(t, srv.URL, `{"text": {{json .Text}}}`)
	// A summary with quotes, a newline and angle brackets must survive as
	// one valid JSON string.
	msg := Message{AlertName: "InstanceDown", Host: "web01", Summary: "disk \"full\"\nsee <details> & retry", AnalyzedBy: "local", PendingURL: "http://gw/pending/7"}
	if err := ch.Send(msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var got struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not the {\"text\":...} shape Slack expects: %v\n%s", err, raw)
	}
	for _, want := range []string{"InstanceDown", "web01", "disk \"full\"\nsee <details> & retry", "http://gw/pending/7"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("text missing %q:\n%s", want, got.Text)
		}
	}
}

func TestWebhookChannel_BodyTemplate_TeamsAdaptiveCardShape(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tmpl := `{"type":"message","attachments":[{"contentType":"application/vnd.microsoft.card.adaptive","content":{"type":"AdaptiveCard","$schema":"http://adaptivecards.io/schemas/adaptive-card.json","version":"1.4","body":[{"type":"TextBlock","text":{{json .Text}},"wrap":true}]}}]}`
	ch := newTemplateChannel(t, srv.URL, tmpl)
	if err := ch.Send(Message{AlertName: "A", Host: "h", Summary: "s", AnalyzedBy: "cloud", EscalatedTo: "gemini"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var got struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string `json:"contentType"`
			Content     struct {
				Type string `json:"type"`
				Body []struct {
					Text string `json:"text"`
				} `json:"body"`
			} `json:"content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if got.Type != "message" || len(got.Attachments) != 1 || got.Attachments[0].Content.Type != "AdaptiveCard" {
		t.Fatalf("unexpected card envelope: %s", raw)
	}
	if body := got.Attachments[0].Content.Body; len(body) != 1 || !strings.Contains(body[0].Text, "gemini") {
		t.Errorf("card text should carry the notification: %s", raw)
	}
}

func TestWebhookChannel_BodyTemplate_NotValidJSON_NotRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	// Forgot {{json ...}}: a summary with a quote would break the JSON.
	ch := newTemplateChannel(t, srv.URL, `{"text": "{{.Summary}}"}`)
	err := ch.Send(Message{AlertName: "A", Summary: `say "hi"`})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("Send error = %v, want a not-valid-JSON error", err)
	}
	if calls.Load() != 0 {
		t.Errorf("an invalid body must not be sent at all, got %d requests", calls.Load())
	}
}

func TestWebhookChannel_BodyTemplate_UnknownFieldFails(t *testing.T) {
	ch := newTemplateChannel(t, "http://unused.invalid", `{"text": {{json .NoSuchField}}}`)
	if err := ch.Send(Message{AlertName: "A"}); err == nil {
		t.Fatal("expected an error for a field BodyTemplateData does not have")
	}
}

func TestWebhookChannel_NoTemplate_StillFixedJSON(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	ch := NewWebhookChannel("itsm", srv.URL, "", nil)
	if err := ch.Send(Message{AlertName: "A", Host: "h"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(string(raw), `"alert_name":"A"`) {
		t.Errorf("default body changed: %s", raw)
	}
}

func TestFormatPlainText_MatchesTelegramContentWithoutMarkup(t *testing.T) {
	msg := Message{AlertName: "A", Host: "h", Summary: "a < b", AnalyzedBy: "cloud", EscalatedTo: "gemini",
		MitigationNote: "plan in #3", PendingURL: "http://gw/pending/1",
		Similar: []SimilarIncident{{Ref: "#1 A (h)", Date: "2026-01-01", URL: "http://gw/i/1"}}}
	got := FormatPlainText(msg)
	for _, want := range []string{"🔍 A (h)", "gemini", "a < b", "🛠 plan in #3", "✅ 確認這筆：\nhttp://gw/pending/1", "📎 相似歷史事件：", "#1 A (h) — 2026-01-01", "http://gw/i/1"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain text missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<b>") || strings.Contains(got, "&lt;") {
		t.Errorf("plain text should carry no HTML markup:\n%s", got)
	}
}

func TestTelegramChannel_ActionsOnlyWhenEnabled(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	msg := Message{AlertName: "X", Host: "h", Summary: "s", Actions: []Action{{Label: "✅ 確認", Data: "v1:n:m"}, {Label: "🔕", Data: "v1:o:p"}}}
	plain := NewTelegramChannel("tg", "token", 1)
	plain.apiBase = srv.URL
	if err := plain.Send(msg); err != nil {
		t.Fatal(err)
	}
	withButtons := NewTelegramChannel("tg", "token", 1)
	withButtons.apiBase = srv.URL
	withButtons.EnableActions()
	if err := withButtons.Send(msg); err != nil {
		t.Fatal(err)
	}
	if err := withButtons.Send(Message{AlertName: "Y"}); err != nil { // no actions: no markup
		t.Fatal(err)
	}
	if _, ok := bodies[0]["reply_markup"]; ok {
		t.Error("buttons rendered on a channel without EnableActions (default must be unchanged)")
	}
	if _, ok := bodies[2]["reply_markup"]; ok {
		t.Error("empty reply_markup sent for a message without actions")
	}
	kb, _ := json.Marshal(bodies[1]["reply_markup"])
	if string(kb) != `{"inline_keyboard":[[{"callback_data":"v1:n:m","text":"✅ 確認"},{"callback_data":"v1:o:p","text":"🔕"}]]}` {
		t.Errorf("reply_markup = %s", kb)
	}
}
