package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// WebhookChannel POSTs each Message as JSON to an arbitrary HTTP
// endpoint — the integration point for anything that isn't Telegram (an
// ITSM intake, a custom bot, another automation). The body mirrors the
// alertResult shape the analysis webhook already returns, so a consumer
// that understands one understands both.
type WebhookChannel struct {
	name    string
	url     string
	method  string
	headers map[string]string
	client  *http.Client
	sleep   func(time.Duration)
}

// NewWebhookChannel builds a webhook channel. method defaults to POST;
// headers (e.g. an Authorization bearer) are sent verbatim on every
// request.
func NewWebhookChannel(name, url, method string, headers map[string]string) *WebhookChannel {
	if method == "" {
		method = http.MethodPost
	}
	return &WebhookChannel{
		name:    name,
		url:     url,
		method:  method,
		headers: headers,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (w *WebhookChannel) Name() string { return w.name }

// webhookBody is the JSON shape delivered to the endpoint. Field names
// deliberately match the analysis webhook's alertResult JSON.
type webhookBody struct {
	AlertName        string            `json:"alert_name"`
	Host             string            `json:"host,omitempty"`
	Summary          string            `json:"summary,omitempty"`
	AnalyzedBy       string            `json:"analyzed_by,omitempty"`
	Error            string            `json:"error,omitempty"`
	SimilarIncidents []similarIncident `json:"similar_incidents,omitempty"`
}

type similarIncident struct {
	Ref  string `json:"ref"`
	Date string `json:"date"`
	URL  string `json:"url,omitempty"`
}

// Send delivers msg with the same bounded retry policy as Telegram
// (transient failures only) — the design doc originally specced
// webhook channels as fire-and-forget, but once retry moved into the
// channel layer there's no reason a webhook consumer deserves less
// delivery effort than a chat message.
func (w *WebhookChannel) Send(msg Message) error {
	body := webhookBody{
		AlertName:  msg.AlertName,
		Host:       msg.Host,
		Summary:    msg.Summary,
		AnalyzedBy: msg.AnalyzedBy,
		Error:      msg.Error,
	}
	for _, s := range msg.Similar {
		body.SimilarIncidents = append(body.SimilarIncidents, similarIncident(s))
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	return withRetry(3, []time.Duration{1 * time.Second, 3 * time.Second}, w.sleep, func() (bool, error) {
		return w.post(payload)
	})
}

func (w *WebhookChannel) post(payload []byte) (retryable bool, err error) {
	req, err := http.NewRequest(w.method, w.url, bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return true, fmt.Errorf("webhook request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Read a little of the body for the log line; the endpoint's
		// error text is usually the only clue to a misconfigured route.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return retryable, fmt.Errorf("webhook endpoint returned %d: %s", resp.StatusCode, string(snippet))
	}
	return false, nil
}
