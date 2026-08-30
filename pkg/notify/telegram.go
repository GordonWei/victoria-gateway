package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

// telegramMaxLen is Telegram sendMessage's documented per-message limit
// (4096 characters after entity parsing). Sending anything longer gets
// the whole message rejected with ok:false — the exact "notification
// silently vanishes" failure this channel exists to prevent, since
// Telegram is the only channel type that reaches a human directly. We
// truncate below a safety margin rather than at the exact limit because
// the limit counts post-parse characters and hunting the exact boundary
// buys nothing over a visible 「已截斷」 marker.
const telegramMaxLen = 4000

// truncationMarker is appended when a message had to be cut. In Chinese
// like the rest of the notification text.
const truncationMarker = "\n…（訊息過長，已截斷；完整內容見 log／issue）"

// TelegramChannel pushes messages to a single chat via the Telegram Bot
// API. It subsumes the old aiops.TelegramNotifier (moved here when
// notification delivery grew routing), adding what the old one lacked:
// message-length truncation and a bounded retry on transient failures.
type TelegramChannel struct {
	name     string
	botToken string
	chatID   int64
	client   *http.Client

	// apiBase and sleep exist for tests: apiBase overrides
	// api.telegram.org, sleep replaces real backoff waits.
	apiBase string
	sleep   func(time.Duration)
}

func NewTelegramChannel(name, botToken string, chatID int64) *TelegramChannel {
	return &TelegramChannel{
		name:     name,
		botToken: botToken,
		chatID:   chatID,
		client:   &http.Client{Timeout: 15 * time.Second},
		apiBase:  "https://api.telegram.org",
	}
}

func (t *TelegramChannel) Name() string { return t.name }

// Send renders msg as Telegram HTML and posts it, retrying transient
// failures (network error, 429, 5xx) twice with short backoff — total
// worst-case delay ~4s, well under the goroutine-per-alert budget and
// never blocking the webhook response (delivery already runs off the
// request path).
func (t *TelegramChannel) Send(msg Message) error {
	text := FormatTelegramText(msg)
	return withRetry(3, []time.Duration{1 * time.Second, 3 * time.Second}, t.sleep, func() (bool, error) {
		return t.post(text)
	})
}

// post does one sendMessage attempt. The bool return classifies the
// failure for withRetry: true means "worth another try" (transport
// error, 429, 5xx), false means the request itself is bad (Telegram
// rejects malformed HTML with a 400 and will keep rejecting it).
func (t *TelegramChannel) post(text string) (retryable bool, err error) {
	body, err := json.Marshal(map[string]any{
		"chat_id":    t.chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	if err != nil {
		return false, fmt.Errorf("marshal telegram payload: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", t.apiBase, t.botToken)
	resp, err := t.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return true, fmt.Errorf("telegram sendMessage request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Telegram reports API-level errors as ok:false in the body, not
	// via status alone — check both rather than trusting a 200.
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return resp.StatusCode >= 500, fmt.Errorf("decode telegram response (status %d): %w", resp.StatusCode, err)
	}
	if !result.OK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return retryable, fmt.Errorf("telegram API error (status %d): %s", resp.StatusCode, result.Description)
	}
	return false, nil
}

// FormatTelegramText renders a Message as the Telegram HTML text this
// service has always sent (moved verbatim from the old notifyTelegram),
// plus the similar-incident section and length truncation. Exported so
// the formatting can be unit-tested without a Telegram round trip.
//
// The alert name/host come from our own labels and are safe, but the
// summary (LLM output) and error (may embed raw log/error text) are
// not — Telegram's HTML parse_mode rejects the whole message on an
// unescaped '<', '>', or '&', so those must be escaped.
func FormatTelegramText(msg Message) string {
	var text string
	switch {
	case msg.Error != "":
		text = fmt.Sprintf("⚠️ <b>%s</b> (%s)\n無法產生摘要：%s",
			html.EscapeString(msg.AlertName), html.EscapeString(msg.Host), html.EscapeString(msg.Error))
	case msg.AnalyzedBy == "cloud":
		text = fmt.Sprintf("🔍 <b>%s</b> (%s)\n<i>已升級至 cloud model 深度分析</i>\n\n%s",
			html.EscapeString(msg.AlertName), html.EscapeString(msg.Host), html.EscapeString(msg.Summary))
	default:
		text = fmt.Sprintf("🚨 <b>%s</b> (%s)\n\n%s",
			html.EscapeString(msg.AlertName), html.EscapeString(msg.Host), html.EscapeString(msg.Summary))
	}

	if len(msg.Similar) > 0 {
		var b strings.Builder
		b.WriteString("\n\n📎 相似歷史事件：")
		for _, s := range msg.Similar {
			fmt.Fprintf(&b, "\n • %s — %s", html.EscapeString(s.Ref), html.EscapeString(s.Date))
			if s.URL != "" {
				b.WriteString("\n   " + html.EscapeString(s.URL))
			}
		}
		text += b.String()
	}

	return truncateTelegramHTML(text, telegramMaxLen)
}

// truncateTelegramHTML cuts text to at most max runes without breaking
// what makes Telegram reject a message: a cut inside an HTML entity
// ("&lt" without its ';') or inside/among unbalanced tags. The tag set
// here is closed (only <b> and <i>, emitted by FormatTelegramText
// itself; everything else is escaped), so balancing means at most
// appending the missing closer(s).
func truncateTelegramHTML(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	marker := []rune(truncationMarker)
	head := string(runes[:max-len(marker)])

	// Never cut mid-entity: an '&' after the last ';' means an entity
	// was opened but its terminator fell past the cut.
	if amp := strings.LastIndex(head, "&"); amp > strings.LastIndex(head, ";") {
		head = head[:amp]
	}
	// Never cut mid-tag: a '<' after the last '>' means a tag bracket
	// was opened but not closed.
	if lt := strings.LastIndex(head, "<"); lt > strings.LastIndex(head, ">") {
		head = head[:lt]
	}
	// Close any tag left open by the cut. Order matters for nesting but
	// this formatter never nests b inside i or vice versa, so appending
	// in fixed order is safe.
	for _, tag := range []string{"i", "b"} {
		if strings.Count(head, "<"+tag+">") > strings.Count(head, "</"+tag+">") {
			head += "</" + tag + ">"
		}
	}
	return head + truncationMarker
}
