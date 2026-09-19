package notify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Host:      "172.16.100.7",
		Summary:   "process 掛了",
		Similar: []SimilarIncident{
			{Ref: "#12 NodeExporterDown (172.16.100.6)", Date: "2026-07-15", URL: "https://gitea.example/issues/12"},
			{Ref: "incident/8 — InstanceDown (172.16.100.7)", Date: "2026-06-20", URL: "/incidents/8"},
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
		Host:       "172.16.100.6",
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
		Host:      "172.16.100.6",
		Summary:   "disk is filling up",
	})
	if strings.Contains(text, "確認這筆") {
		t.Errorf("expected no pending-link section when PendingURL is empty:\n%s", text)
	}
}

func TestFormatTelegramText_PendingURLAndSimilar_BothPresent(t *testing.T) {
	text := FormatTelegramText(Message{
		AlertName:  "DiskSpace",
		Host:       "172.16.100.6",
		Summary:    "disk is filling up",
		PendingURL: "https://vg.example/pending/42",
		Similar: []SimilarIncident{
			{Ref: "#12 DiskSpace (172.16.100.6)", Date: "2026-07-15", URL: "https://gitea.example/issues/12"},
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
		Host:       "172.16.100.7",
		Summary:    "summary",
		AnalyzedBy: "local",
		Similar:    []SimilarIncident{{Ref: "#1 A (h)", Date: "2026-01-01", URL: "u"}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.AlertName != "InstanceDown" || got.AnalyzedBy != "local" || got.Host != "172.16.100.7" {
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
