package aiops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TelegramNotifier pushes text messages to a single chat via the Telegram
// Bot API. It exists because the webhook handler's HTTP response body
// (the LLM summary) is never read by anything — Alertmanager only checks
// the status code — so without an explicit push, the summary is computed
// and then silently discarded.
type TelegramNotifier struct {
	botToken string
	chatID   int64
	client   *http.Client
}

func NewTelegramNotifier(botToken string, chatID int64) *TelegramNotifier {
	return &TelegramNotifier{
		botToken: botToken,
		chatID:   chatID,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether a bot token was configured. Callers use this to
// skip the push entirely rather than attempting a call that's guaranteed
// to fail against an empty token.
func (t *TelegramNotifier) Enabled() bool {
	return t != nil && t.botToken != ""
}

// SendMessage posts text to the configured chat. Telegram's sendMessage
// only fails the whole payload on malformed HTML, not on API-level errors
// (those come back as ok:false in the response body), so this checks both
// the HTTP status and the ok field rather than trusting a 200 alone.
func (t *TelegramNotifier) SendMessage(text string) error {
	if !t.Enabled() {
		return fmt.Errorf("telegram notifier not configured (empty bot_token)")
	}

	body, err := json.Marshal(map[string]any{
		"chat_id":    t.chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	if err != nil {
		return fmt.Errorf("marshal telegram payload: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	resp, err := t.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram sendMessage request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode telegram response (status %d): %w", resp.StatusCode, err)
	}
	if !result.OK {
		return fmt.Errorf("telegram API error (status %d): %s", resp.StatusCode, result.Description)
	}
	return nil
}
