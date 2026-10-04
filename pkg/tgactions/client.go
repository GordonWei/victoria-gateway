package tgactions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// DefaultAPIBase is the Bot API host. Tests point Client at a fake.
const DefaultAPIBase = "https://api.telegram.org"

// User is the subset of a Telegram user this package reads.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Chat is the subset of a Telegram chat this package reads.
type Chat struct {
	ID int64 `json:"id"`
}

// Message is the subset of a Telegram message this package reads.
type Message struct {
	MessageID int64 `json:"message_id"`
	Chat      Chat  `json:"chat"`
}

// CallbackQuery is one button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// Update is one getUpdates entry; only callback queries are requested.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Client is a minimal Bot API client for the three calls buttons need.
type Client struct {
	apiBase string
	token   string
	http    *http.Client
}

// NewClient returns a Client for token. apiBase "" means DefaultAPIBase.
func NewClient(apiBase, token string) *Client {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	// No client-wide timeout: getUpdates holds the connection for the
	// long-poll period. Every call sets its own deadline instead.
	return &Client{apiBase: strings.TrimRight(apiBase, "/"), token: token, http: &http.Client{}}
}

// call posts body to method and decodes "result" into out (may be nil).
// Errors never include the request URL, which contains the bot token.
func (c *Client) call(ctx context.Context, timeout time.Duration, method string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram %s: marshal: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/bot"+c.token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("telegram %s: build request failed", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("telegram %s: %w", method, ctx.Err())
		}
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: decode response (status %d): %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: status %d: %s", method, resp.StatusCode, env.Description)
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, err)
		}
	}
	return nil
}

// GetUpdates long-polls for callback queries after offset.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, time.Duration(timeoutSec+10)*time.Second, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"callback_query"},
	}, &ups)
	return ups, err
}

// AnswerCallbackQuery stops the button's spinner and shows text as a toast.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	return c.call(ctx, 10*time.Second, "answerCallbackQuery", map[string]any{
		"callback_query_id": id,
		"text":              text,
	}, nil)
}

// SendMessage posts plain text (no parse mode) to chatID, as a reply to
// replyTo when it is non-zero.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) error {
	body := map[string]any{"chat_id": chatID, "text": text}
	if replyTo != 0 {
		body["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	return c.call(ctx, 15*time.Second, "sendMessage", body, nil)
}

// Poll calls GetUpdates until ctx is done, handing each callback query to
// handle in order. Errors back off from 1s up to 30s; a 409 (another
// consumer, or a webhook set on the bot) is logged like any other error.
func (c *Client) Poll(ctx context.Context, timeoutSec int, handle func(context.Context, CallbackQuery), sleep func(context.Context, time.Duration)) {
	if sleep == nil {
		sleep = sleepCtx
	}
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := c.GetUpdates(ctx, offset, timeoutSec)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("telegram_actions: getUpdates: %v (retrying in %s)", err, backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				handle(ctx, *u.CallbackQuery)
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
