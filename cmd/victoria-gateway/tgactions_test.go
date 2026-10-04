package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/alertmanager"
	"github.com/gordonwei/victoria-gateway/pkg/audit"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
	"github.com/gordonwei/victoria-gateway/pkg/notify"
	"github.com/gordonwei/victoria-gateway/pkg/tgactions"
)

// Every test here runs against fake servers on loopback: a fake Bot API,
// a fake cloud model and an in-memory silencer. Nothing reaches Telegram.

const (
	tgTestToken  = "123456:fake-bot-token-for-tests"
	tgTestChat   = int64(-1001)
	tgTestUser   = int64(42)
	tgTestSecret = "button-secret-for-tests-0123456789abcdef"
)

// fakeTelegram serves queued callback queries from getUpdates (each one
// once) and records answers and replies.
type fakeTelegram struct {
	mu       sync.Mutex
	queue    []tgactions.Update
	nextID   int64
	answers  []string
	replies  []string
	served   int
	srv      *httptest.Server
	unexpect []string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{nextID: 100}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/bot"+tgTestToken+"/")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch method {
		case "getUpdates":
			batch := f.queue
			f.queue = nil
			f.served += len(batch)
			b, _ := json.Marshal(map[string]any{"ok": true, "result": batch})
			if len(batch) == 0 {
				// An empty long poll: don't spin.
				f.mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				f.mu.Lock()
			}
			_, _ = w.Write(b)
		case "answerCallbackQuery":
			f.answers = append(f.answers, body["text"].(string))
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "sendMessage":
			f.replies = append(f.replies, body["text"].(string))
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		default:
			f.unexpect = append(f.unexpect, method)
			_, _ = io.WriteString(w, `{"ok":false,"description":"unexpected"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTelegram) push(user, chat int64, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.queue = append(f.queue, tgactions.Update{UpdateID: f.nextID, CallbackQuery: &tgactions.CallbackQuery{
		ID: "cq", From: tgactions.User{ID: user, Username: "oncall"}, Message: &tgactions.Message{MessageID: 7, Chat: tgactions.Chat{ID: chat}}, Data: data,
	}})
}

func (f *fakeTelegram) snapshot() (answers, replies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answers...), append([]string(nil), f.replies...)
}

type fakeSilencer struct {
	mu    sync.Mutex
	calls []struct {
		matchers []alertmanager.Matcher
		dur      time.Duration
		by       string
	}
	err error
}

func (s *fakeSilencer) CreateSilence(_ context.Context, m []alertmanager.Matcher, d time.Duration, by, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.calls = append(s.calls, struct {
		matchers []alertmanager.Matcher
		dur      time.Duration
		by       string
	}{m, d, by})
	return "sil-1", nil
}

type syncAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *syncAudit) Record(_ context.Context, e audit.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}
func (a *syncAudit) List(context.Context, int) ([]audit.Entry, error) { return nil, nil }
func (a *syncAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		out = append(out, e.Action)
	}
	return out
}

type syncChannel struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (c *syncChannel) Name() string { return "test" }
func (c *syncChannel) Send(m notify.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

type tgRig struct {
	h          *handler
	tg         *fakeTelegram
	sil        *fakeSilencer
	audit      *syncAudit
	ch         *syncChannel
	cloudCalls *int
}

func newTGRig(t *testing.T, maxPerHour int) *tgRig {
	t.Helper()
	calls := 0
	cloud := countingCloud(t, "cloud root cause analysis", http.StatusOK, &calls)
	t.Cleanup(cloud.Close)
	r := &tgRig{tg: newFakeTelegram(t), sil: &fakeSilencer{}, audit: &syncAudit{}, ch: &syncChannel{}, cloudCalls: &calls}
	router, err := notify.NewRouter([]notify.Channel{r.ch}, []notify.Route{{Default: true, Channels: []string{"test"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.h = &handler{
		notifier:   router,
		audit:      r.audit,
		metrics:    &metrics.Counters{},
		escalation: config.EscalationConfig{MaxPerHour: maxPerHour},
	}
	cfg := &config.Config{
		Telegram: config.TelegramConfig{BotToken: tgTestToken, ChatID: tgTestChat},
		TelegramActions: &config.TelegramActionsConfig{
			Enabled: true, AllowedUserIDs: []int64{tgTestUser}, HMACSecret: tgTestSecret,
			SilenceDurationSec: 7200, PollTimeoutSec: 1,
		},
	}
	r.h.tgActions = newTelegramActions(cfg, r.h, r.tg.srv.URL, r.sil)
	return r
}

var tgLabels = map[string]string{"alertname": "cpu_high", "host": "test-host", "severity": "warning"}

// localResult is a local analysis whose route can escalate to the rig's
// fake cloud.
func (r *tgRig) localResult(t *testing.T, cloudURL string) alertResult {
	t.Helper()
	llm := anthropicAt(cloudURL)
	return alertResult{
		AlertName: "cpu_high", Host: "test-host", Summary: "local says cpu", AnalyzedBy: "local",
		escInput: &escalationInput{
			alert: aiops.Alert{Labels: tgLabels, StartsAt: time.Now().Format(time.RFC3339)},
			logs:  []aiops.LogEntry{{Line: "cpu at 97%"}},
			steps: []escalationStep{{display: "anthropic", llm: llm}},
		},
	}
}

func buttonData(t *testing.T, actions []notify.Action, labelPrefix string) string {
	t.Helper()
	for _, a := range actions {
		if strings.HasPrefix(a.Label, labelPrefix) {
			return a.Data
		}
	}
	t.Fatalf("no %q button in %+v", labelPrefix, actions)
	return ""
}

func TestTelegramButtons_DefaultOffNoActions(t *testing.T) {
	ch := &fakeNotifyChannel{name: "test"}
	h := &handler{notifier: newTestRouter(t, ch)}
	h.notifyResult(alertResult{AlertName: "X", Host: "h", Summary: "s", AnalyzedBy: "local"}, tgLabels)
	if len(ch.sent) != 1 || ch.sent[0].Actions != nil {
		t.Fatalf("buttons without telegram_actions: %+v", ch.sent)
	}
}

func TestTelegramButtons_WhichButtons(t *testing.T) {
	r := newTGRig(t, 5)
	cloud := httptest.NewServer(http.NotFoundHandler())
	defer cloud.Close()

	local := r.localResult(t, cloud.URL)
	if got := r.h.telegramButtons(local, tgLabels); len(got) != 3 {
		t.Fatalf("local result with a target: %d buttons, want 3 (%+v)", len(got), got)
	}
	cloudRes := local
	cloudRes.AnalyzedBy = "cloud"
	if got := r.h.telegramButtons(cloudRes, tgLabels); len(got) != 2 || strings.Contains(got[1].Label, "升級") {
		t.Fatalf("cloud result: %+v", got)
	}
	errRes := alertResult{AlertName: "cpu_high", Error: "log query: boom"}
	if got := r.h.telegramButtons(errRes, tgLabels); len(got) != 2 {
		t.Fatalf("failed analysis should still offer ack and silence: %+v", got)
	}
	if got := r.h.telegramButtons(alertResult{AnalyzedBy: "suppressed"}, tgLabels); got != nil {
		t.Fatal("buttons on a suppressed alert")
	}
	r.h.tgActions.silencer = nil
	if got := r.h.telegramButtons(cloudRes, tgLabels); len(got) != 1 {
		t.Fatalf("no alertmanager: %+v", got)
	}
	for _, a := range r.h.telegramButtons(local, tgLabels) {
		if len(a.Data) > 64 {
			t.Errorf("callback data %d bytes > 64", len(a.Data))
		}
	}
	// Muted alerts are never dispatched, so get no buttons either.
	before := r.h.tgActions.store.Len()
	r.h.notifyResult(alertResult{AlertName: "x", muted: true}, tgLabels)
	if r.h.tgActions.store.Len() != before {
		t.Fatal("buttons issued for a muted alert")
	}
}

func TestTelegramActions_EndToEndThroughPolling(t *testing.T) {
	r := newTGRig(t, 5)
	cloud := countingCloud(t, "cloud root cause analysis", http.StatusOK, r.cloudCalls)
	defer cloud.Close()
	buttons := r.h.telegramButtons(r.localResult(t, cloud.URL), tgLabels)
	ack, esc, sil := buttonData(t, buttons, "✅"), buttonData(t, buttons, "☁️"), buttonData(t, buttons, "🔕")
	parts := strings.Split(ack, ":")
	forged := parts[0] + ":" + parts[1] + ":" + strings.Repeat("A", len(parts[2]))

	r.tg.push(999, tgTestChat, ack)        // not on the allowlist
	r.tg.push(tgTestUser, tgTestChat, ack) // ok
	r.tg.push(tgTestUser, tgTestChat, ack) // double click
	r.tg.push(tgTestUser, -5, esc)         // another chat
	r.tg.push(tgTestUser, tgTestChat, forged)
	r.tg.push(tgTestUser, tgTestChat, esc)
	r.tg.push(tgTestUser, tgTestChat, sil)
	const presses = 7

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.h.tgActions.run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		answers, _ := r.tg.snapshot()
		if len(answers) == presses {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d presses answered", len(answers), presses)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	r.h.inFlight.Wait() // the escalation runs in the background

	if *r.cloudCalls != 1 {
		t.Fatalf("cloud called %d times, want 1", *r.cloudCalls)
	}
	r.ch.mu.Lock()
	sent := append([]notify.Message(nil), r.ch.sent...)
	r.ch.mu.Unlock()
	if len(sent) != 1 || sent[0].AnalyzedBy != "cloud" || sent[0].Summary != "cloud root cause analysis" || sent[0].Actions != nil {
		t.Fatalf("escalation result notification = %+v", sent)
	}
	if len(r.sil.calls) != 1 {
		t.Fatalf("silences = %d, want 1", len(r.sil.calls))
	}
	c := r.sil.calls[0]
	if c.dur != 2*time.Hour || len(c.matchers) != 2 || c.matchers[0].Value != "cpu_high" || c.matchers[1] != alertmanager.NewExactMatcher("host", "test-host") {
		t.Fatalf("silence = %+v", c)
	}
	if !strings.Contains(c.by, "42") {
		t.Errorf("createdBy = %q", c.by)
	}

	actions := strings.Join(r.audit.actions(), ",")
	for _, want := range []string{"telegram.ack", "telegram.escalate", "telegram.silence", "escalation.trigger"} {
		if !strings.Contains(actions, want) {
			t.Errorf("audit missing %s: %s", want, actions)
		}
	}
	if n := strings.Count(actions, "telegram.action_denied"); n != 4 {
		t.Errorf("denied presses audited %d times, want 4: %s", n, actions)
	}
	_, replies := r.tg.snapshot()
	if len(replies) != 3 {
		t.Fatalf("chat replies = %v, want ack, escalate and silence", replies)
	}
	wantContains(t, "metrics", scrape(t, r.h.metrics),
		`victoria_gateway_telegram_actions_total{action="denied",result="already_used"} 1`,
		`victoria_gateway_telegram_actions_total{action="denied",result="user_not_allowed"} 1`,
		`victoria_gateway_telegram_actions_total{action="denied",result="wrong_chat"} 1`,
		`victoria_gateway_telegram_actions_total{action="denied",result="bad_signature"} 1`,
		`victoria_gateway_telegram_actions_total{action="escalate",result="ok"} 1`,
		`victoria_gateway_telegram_actions_total{action="silence",result="ok"} 1`)
	r.tg.mu.Lock()
	defer r.tg.mu.Unlock()
	if len(r.tg.unexpect) != 0 {
		t.Errorf("unexpected Bot API calls: %v", r.tg.unexpect)
	}
}

func handle(r *tgRig, user int64, data string) {
	r.h.tgActions.proc.Handle(context.Background(), tgactions.CallbackQuery{
		ID: "cq", From: tgactions.User{ID: user}, Message: &tgactions.Message{MessageID: 7, Chat: tgactions.Chat{ID: tgTestChat}}, Data: data,
	})
}

func TestTelegramActions_EscalateNeedsMaxPerHour(t *testing.T) {
	r := newTGRig(t, 1)
	cloud := countingCloud(t, "cloud answer", http.StatusOK, r.cloudCalls)
	defer cloud.Close()
	if !r.h.allowEscalation() { // the hour's only slot is taken by an alert
		t.Fatal("setup")
	}
	esc := buttonData(t, r.h.telegramButtons(r.localResult(t, cloud.URL), tgLabels), "☁️")

	handle(r, tgTestUser, esc)
	r.h.inFlight.Wait()
	if *r.cloudCalls != 0 {
		t.Fatal("escalated past max_per_hour")
	}
	answers, _ := r.tg.snapshot()
	if !strings.Contains(answers[0], "max_per_hour") {
		t.Fatalf("toast = %q", answers[0])
	}
	if acts := strings.Join(r.audit.actions(), ","); !strings.Contains(acts, "escalation.rate_limited") {
		t.Fatalf("rate limit not audited: %s", acts)
	}

	// Next hour: the same button works, because a rate-limited press
	// releases it.
	r.h.escalationMu.Lock()
	r.h.escalationWindowStart = time.Now().Add(-2 * time.Hour)
	r.h.escalationMu.Unlock()
	handle(r, tgTestUser, esc)
	r.h.inFlight.Wait()
	if *r.cloudCalls != 1 {
		t.Fatalf("cloud calls after the window reset = %d", *r.cloudCalls)
	}
	handle(r, tgTestUser, esc) // and only once
	r.h.inFlight.Wait()
	if *r.cloudCalls != 1 {
		t.Fatal("escalate button worked twice")
	}
}

func TestTelegramActions_ExpiredButton(t *testing.T) {
	r := newTGRig(t, 5)
	now := time.Now()
	clock := func() time.Time { return now }
	store := tgactions.NewStore(time.Hour, 100, clock)
	r.h.tgActions.store, r.h.tgActions.proc.Store = store, store
	ack := buttonData(t, r.h.telegramButtons(alertResult{AlertName: "cpu_high", AnalyzedBy: "local"}, tgLabels), "✅")
	now = now.Add(61 * time.Minute)
	handle(r, tgTestUser, ack)
	answers, replies := r.tg.snapshot()
	if len(replies) != 0 || !strings.Contains(answers[0], "過期") {
		t.Fatalf("expired press: answers=%v replies=%v", answers, replies)
	}
	wantContains(t, "metrics", scrape(t, r.h.metrics), `victoria_gateway_telegram_actions_total{action="denied",result="expired"} 1`)
}

func TestTelegramActions_SilenceFailureCanRetry(t *testing.T) {
	r := newTGRig(t, 5)
	r.sil.err = errors.New("alertmanager down")
	sil := buttonData(t, r.h.telegramButtons(alertResult{AlertName: "cpu_high", AnalyzedBy: "local"}, tgLabels), "🔕")
	handle(r, tgTestUser, sil)
	r.sil.err = nil
	handle(r, tgTestUser, sil)
	if len(r.sil.calls) != 1 {
		t.Fatalf("silence calls = %d, want 1 after a retry", len(r.sil.calls))
	}
	wantContains(t, "metrics", scrape(t, r.h.metrics),
		`victoria_gateway_telegram_actions_total{action="silence",result="error"} 1`,
		`victoria_gateway_telegram_actions_total{action="silence",result="ok"} 1`)
}

func TestSilenceMatchers(t *testing.T) {
	got := silenceMatchers(map[string]string{"alertname": "PodCrash", "namespace": "web", "pod": "api-1", "severity": "page"})
	if len(got) != 3 || got[1].Name != "namespace" || got[2].Name != "pod" {
		t.Fatalf("matchers = %+v (severity must not be matched)", got)
	}
}

// summarizeOne only keeps what the escalate button needs when buttons are
// on, and only for a local result on a route that can escalate.
func TestSummarizeOne_KeepsEscalationInputOnlyForButtons(t *testing.T) {
	loki := newFakeLoki(t)
	defer loki.Close()
	llm := newFakeLLM(t, `{"summary":"local answer","confidence":"high","escalate":false,"reason":"x"}`)
	defer llm.Close()

	h := newTestHandler(t, loki.URL, llm.URL)
	h.cloud = anthropicAt("http://cloud.invalid")
	h.legacyEscalations = []escalationStep{{display: "anthropic", llm: h.cloud}}
	if res := h.summarizeOne(testAlert()); res.escInput != nil {
		t.Fatal("escalation input kept with buttons off")
	}
	r := newTGRig(t, 5)
	h.tgActions = r.h.tgActions
	res := h.summarizeOne(testAlert())
	if res.escInput == nil || len(res.escInput.logs) != 1 || len(res.escInput.steps) != 1 {
		t.Fatalf("escInput = %+v", res.escInput)
	}
	h.legacyEscalations, h.cloud = nil, nil
	if res := h.summarizeOne(testAlert()); res.escInput != nil {
		t.Fatal("escalation input kept for a route with nowhere to escalate")
	}
}

func TestSecurityWarnings_TelegramActionsUnlimitedEscalation(t *testing.T) {
	cfg := &config.Config{Cloud: &config.CloudConfig{Provider: "gemini"}, TelegramActions: &config.TelegramActionsConfig{Enabled: true}}
	if !strings.Contains(strings.Join(securityWarnings(cfg), "\n"), "escalate button is not rate limited") {
		t.Fatal("missing warning")
	}
	cfg.Escalation.MaxPerHour = 3
	if strings.Contains(strings.Join(securityWarnings(cfg), "\n"), "escalate button") {
		t.Fatal("warning with a limit set")
	}
}

func TestImplicitTelegram_ButtonsOnlyWhenEnabled(t *testing.T) {
	cfg := &config.Config{Telegram: config.TelegramConfig{BotToken: "t", ChatID: 1}}
	if implicitTelegram(cfg).ActionsEnabled() {
		t.Fatal("buttons on by default")
	}
	cfg.TelegramActions = &config.TelegramActionsConfig{Enabled: false}
	if implicitTelegram(cfg).ActionsEnabled() {
		t.Fatal("buttons on with enabled: false")
	}
	cfg.TelegramActions.Enabled = true
	if !implicitTelegram(cfg).ActionsEnabled() {
		t.Fatal("buttons off with enabled: true")
	}
}
