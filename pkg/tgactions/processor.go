package tgactions

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// Outcome is what an Executor did with a press.
type Outcome struct {
	// Result is a short machine-readable word for audit and metrics:
	// "ok", "rate_limited", "no_target", "error", ...
	Result string
	// Toast is shown to the presser (answerCallbackQuery).
	Toast string
	// Reply, if set, is posted to the chat as a reply to the notification,
	// so everyone in the chat sees who did what.
	Reply string
	// Detail is extra audit text.
	Detail string
	// Release makes the button usable again (the press was refused for a
	// reason that can pass later).
	Release bool
}

// Executor carries out a verified press. It runs on the polling goroutine,
// so anything slow (an escalation) must be started in the background and
// reported by other means.
type Executor func(ctx context.Context, kind Kind, payload any, user User) Outcome

// Auditor records one outcome. Actor is "telegram:<user id>".
type Auditor func(ctx context.Context, actor, action, target, detail string)

// Processor checks and dispatches button presses.
type Processor struct {
	Signer  *Signer
	Store   *Store
	Client  *Client
	ChatID  int64
	Allowed map[int64]bool
	Exec    Executor
	Audit   Auditor
	// Count, if set, is told every outcome: action is the kind or
	// "denied", result the outcome or the refusal reason.
	Count func(action, result string)
}

// Refusal reasons, as recorded in audit and metrics.
const (
	DeniedChat      = "wrong_chat"
	DeniedUser      = "user_not_allowed"
	DeniedSignature = "bad_signature"
	DeniedExpired   = "expired"
	DeniedReplay    = "already_used"
)

// Handle processes one press: chat, then user, then signature, then nonce
// — each refusal is answered, audited and counted, and the Executor only
// ever sees presses that passed all four.
func (p *Processor) Handle(ctx context.Context, cq CallbackQuery) {
	actor := fmt.Sprintf("telegram:%d", cq.From.ID)
	var msgID int64
	deny := func(reason, toast string) {
		p.audit(ctx, actor, "telegram.action_denied", "", "reason="+reason)
		p.count("denied", reason)
		p.answer(ctx, cq.ID, toast)
	}
	if cq.Message == nil || cq.Message.Chat.ID != p.ChatID {
		deny(DeniedChat, "這個按鈕不屬於本服務的通知群組。")
		return
	}
	msgID = cq.Message.MessageID
	if !p.Allowed[cq.From.ID] {
		deny(DeniedUser, "你沒有權限使用這些按鈕。")
		return
	}
	nonce, err := p.Signer.Verify(cq.Data)
	if err != nil {
		deny(DeniedSignature, "按鈕資料無效。")
		return
	}
	kind, payload, err := p.Store.Claim(nonce)
	switch {
	case errors.Is(err, ErrUsed):
		deny(DeniedReplay, "這個按鈕已經處理過了。")
		return
	case err != nil:
		deny(DeniedExpired, "這個按鈕已過期（或服務重啟過），請到 /pending 頁面處理。")
		return
	}
	out := p.Exec(ctx, kind, payload, cq.From)
	if out.Release {
		p.Store.Release(nonce)
	}
	detail := "result=" + out.Result
	if out.Detail != "" {
		detail += " " + out.Detail
	}
	p.audit(ctx, actor, "telegram."+string(kind), fmt.Sprintf("message=%d", msgID), detail)
	p.count(string(kind), out.Result)
	p.answer(ctx, cq.ID, out.Toast)
	if out.Reply != "" && p.Client != nil {
		if err := p.Client.SendMessage(ctx, p.ChatID, out.Reply, msgID); err != nil {
			log.Printf("telegram_actions: reply: %v", err)
		}
	}
}

func (p *Processor) answer(ctx context.Context, id, text string) {
	if p.Client == nil {
		return
	}
	if err := p.Client.AnswerCallbackQuery(ctx, id, text); err != nil {
		log.Printf("telegram_actions: answerCallbackQuery: %v", err)
	}
}

func (p *Processor) audit(ctx context.Context, actor, action, target, detail string) {
	if p.Audit != nil {
		p.Audit(ctx, actor, action, target, detail)
	}
}

func (p *Processor) count(action, result string) {
	if p.Count != nil {
		p.Count(action, result)
	}
}
