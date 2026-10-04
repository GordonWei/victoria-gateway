package main

// Telegram action buttons (telegram_actions): what each button does once
// pkg/tgactions has verified the press. See that package's doc for the
// checks that come first (chat, user allowlist, HMAC, single-use nonce).

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/alertmanager"
	"github.com/gordonwei/victoria-gateway/pkg/audit"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/notify"
	"github.com/gordonwei/victoria-gateway/pkg/tgactions"
)

// maxTelegramButtons bounds the nonce store: with three buttons per
// notification, the last thousand notifications keep working buttons.
const maxTelegramButtons = 3000

// silencer is the part of the Alertmanager client the silence button uses.
type silencer interface {
	CreateSilence(ctx context.Context, matchers []alertmanager.Matcher, duration time.Duration, createdBy, comment string) (string, error)
}

type telegramActions struct {
	store       *tgactions.Store
	signer      *tgactions.Signer
	client      *tgactions.Client
	proc        *tgactions.Processor
	silencer    silencer // nil when there is no alertmanager block: no silence button
	silenceFor  time.Duration
	pollTimeout int
}

// escalationInput is what the escalate button needs to rerun an alert on
// the cloud: the same (already masked) alert, logs and RAG context the
// local model saw, and the route's escalation chain.
type escalationInput struct {
	alert      aiops.Alert
	logs       []aiops.LogEntry
	ragContext string
	steps      []escalationStep
}

// actionPayload is what every button of one notification refers to.
type actionPayload struct {
	alertName string
	host      string
	labels    map[string]string
	esc       *escalationInput // nil: no escalate button was offered
}

// newTelegramActions builds the button machinery for h. apiBase "" means
// the real Bot API; tests pass a fake server's URL.
func newTelegramActions(cfg *config.Config, h *handler, apiBase string, sil silencer) *telegramActions {
	tc := cfg.TelegramActions
	ta := &telegramActions{
		store:       tgactions.NewStore(time.Duration(tc.EffectiveButtonTTLSec())*time.Second, maxTelegramButtons, nil),
		signer:      tgactions.NewSigner(tc.HMACSecret),
		client:      tgactions.NewClient(apiBase, cfg.Telegram.BotToken),
		silencer:    sil,
		silenceFor:  time.Duration(tc.EffectiveSilenceDurationSec()) * time.Second,
		pollTimeout: tc.EffectivePollTimeoutSec(),
	}
	allowed := make(map[int64]bool, len(tc.AllowedUserIDs))
	for _, id := range tc.AllowedUserIDs {
		allowed[id] = true
	}
	ta.proc = &tgactions.Processor{
		Signer:  ta.signer,
		Store:   ta.store,
		Client:  ta.client,
		ChatID:  cfg.Telegram.ChatID,
		Allowed: allowed,
		Exec:    h.execTelegramAction,
		Audit: func(ctx context.Context, actor, action, target, detail string) {
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			h.recordAudit(actx, audit.Entry{Actor: actor, Action: action, Target: target, Detail: detail})
		},
		Count: h.metrics.IncTelegramActionsTotal,
	}
	return ta
}

// run polls for presses until ctx is done.
func (ta *telegramActions) run(ctx context.Context) {
	ta.client.Poll(ctx, ta.pollTimeout, ta.proc.Handle, nil)
}

// telegramButtons registers this notification's buttons and returns them,
// or nil when buttons are off or don't apply. Escalate is offered only for
// a local result on a route with an escalation target; silence only with
// an alertmanager block.
func (h *handler) telegramButtons(res alertResult, labels map[string]string) []notify.Action {
	ta := h.tgActions
	if ta == nil || res.AnalyzedBy == "suppressed" {
		return nil
	}
	p := &actionPayload{alertName: res.AlertName, host: res.Host, labels: labels}
	kinds := []tgactions.Kind{tgactions.KindAck}
	if res.escInput != nil && res.AnalyzedBy == "local" && res.Error == "" {
		p.esc = res.escInput
		kinds = append(kinds, tgactions.KindEscalate)
	}
	if ta.silencer != nil && labels["alertname"] != "" {
		kinds = append(kinds, tgactions.KindSilence)
	}
	nonces, err := ta.store.Issue(kinds, p)
	if err != nil {
		log.Printf("telegram_actions: could not issue buttons, sending without them: %v", err)
		return nil
	}
	label := map[tgactions.Kind]string{
		tgactions.KindAck:      "✅ 確認",
		tgactions.KindEscalate: "☁️ 立即升級",
		tgactions.KindSilence:  "🔕 靜默 " + shortDuration(ta.silenceFor),
	}
	out := make([]notify.Action, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, notify.Action{Label: label[k], Data: ta.signer.Sign(nonces[k])})
	}
	return out
}

func shortDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

func telegramWho(u tgactions.User) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return fmt.Sprintf("user %d", u.ID)
}

// execTelegramAction carries out one verified press.
func (h *handler) execTelegramAction(ctx context.Context, kind tgactions.Kind, payload any, user tgactions.User) tgactions.Outcome {
	p, ok := payload.(*actionPayload)
	if !ok {
		return tgactions.Outcome{Result: "error", Toast: "內部錯誤：無法辨識這個按鈕。"}
	}
	subject := fmt.Sprintf("alertname=%s host=%s", p.alertName, p.host)
	switch kind {
	case tgactions.KindAck:
		return tgactions.Outcome{
			Result: "ok", Detail: subject, Toast: "已確認。",
			Reply: fmt.Sprintf("✅ %s 已確認處理 %s（%s）", telegramWho(user), p.alertName, p.host),
		}
	case tgactions.KindEscalate:
		return h.telegramEscalate(p, user, subject)
	case tgactions.KindSilence:
		return h.telegramSilence(ctx, p, user, subject)
	}
	return tgactions.Outcome{Result: "error", Detail: subject, Toast: "不支援的動作。"}
}

func (h *handler) telegramEscalate(p *actionPayload, user tgactions.User, subject string) tgactions.Outcome {
	if p.esc == nil || len(p.esc.steps) == 0 {
		return tgactions.Outcome{Result: "no_target", Detail: subject, Toast: "這則告警沒有可升級的雲端目標。"}
	}
	reason := fmt.Sprintf("telegram button by user %d", user.ID)
	if !h.allowEscalation() {
		h.metrics.IncEscalationRateLimitedTotal(p.esc.steps[0].display)
		h.auditEscalationRateLimited(p.esc.alert, reason)
		return tgactions.Outcome{
			Result: "rate_limited", Detail: subject, Release: true,
			Toast: "已達 escalation.max_per_hour 上限，這個按鈕稍後還能再按。",
		}
	}
	// A cloud call can take minutes; the press is answered now and the
	// result arrives as a normal notification. Tracked in inFlight so a
	// shutdown waits for it like any other analysis.
	esc := p.esc
	h.inFlight.Add(1)
	go func() {
		defer h.inFlight.Done()
		result, step, err := h.runEscalation(esc.steps, esc.alert, esc.logs, esc.ragContext, reason)
		msg := notify.Message{AlertName: p.alertName, Host: p.host}
		if err != nil {
			msg.Error = "立即升級失敗：" + h.maskText(err.Error())
		} else {
			result = h.maskResult(result)
			msg.Summary = result.Summary
			msg.AnalyzedBy = "cloud"
			msg.EscalatedTo = step.display
		}
		h.notifier.Dispatch(msg, p.labels)
	}()
	return tgactions.Outcome{
		Result: "ok", Detail: subject + " started=true", Toast: "已送出升級，結果會另外推播。",
		Reply: fmt.Sprintf("☁️ %s 要求立即升級 %s（%s），分析中…", telegramWho(user), p.alertName, p.host),
	}
}

// silenceIdentityLabels are the labels, besides alertname, that a silence
// matches on when the alert has them: the ones that say which thing is
// alerting, so the silence covers this host or workload, not every alert
// of the same name.
var silenceIdentityLabels = []string{"host", "instance", "namespace", "pod", "deployment", "statefulset"}

func silenceMatchers(labels map[string]string) []alertmanager.Matcher {
	m := []alertmanager.Matcher{alertmanager.NewExactMatcher("alertname", labels["alertname"])}
	for _, k := range silenceIdentityLabels {
		if v := labels[k]; v != "" {
			m = append(m, alertmanager.NewExactMatcher(k, v))
		}
	}
	return m
}

func (h *handler) telegramSilence(ctx context.Context, p *actionPayload, user tgactions.User, subject string) tgactions.Outcome {
	ta := h.tgActions
	if ta == nil || ta.silencer == nil || p.labels["alertname"] == "" {
		return tgactions.Outcome{Result: "no_target", Detail: subject, Toast: "沒有設定 Alertmanager，無法靜默。"}
	}
	dur := ta.silenceFor
	if limit := time.Duration(config.TelegramActionsMaxSilenceSec) * time.Second; dur > limit {
		dur = limit // config validation already enforces this; belt and braces
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id, err := ta.silencer.CreateSilence(sctx, silenceMatchers(p.labels), dur,
		fmt.Sprintf("victoria-gateway (telegram user %d)", user.ID),
		fmt.Sprintf("Silenced for %s from a victoria-gateway Telegram button; expires on its own.", dur))
	if err != nil {
		log.Printf("telegram_actions: create silence for %s: %v", subject, err)
		return tgactions.Outcome{Result: "error", Detail: subject, Release: true, Toast: "建立靜默失敗，請稍後再試或到 Alertmanager 手動處理。"}
	}
	return tgactions.Outcome{
		Result: "ok", Detail: fmt.Sprintf("%s silence_id=%s duration=%s", subject, id, dur), Toast: "已靜默。",
		Reply: fmt.Sprintf("🔕 %s 已靜默 %s（%s）%s，到期自動解除（silence %s）", telegramWho(user), p.alertName, p.host, shortDuration(dur), id),
	}
}
