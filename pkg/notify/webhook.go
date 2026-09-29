package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// WebhookChannel POSTs each Message as JSON to an arbitrary HTTP
// endpoint — the integration point for anything that isn't Telegram (an
// ITSM intake, a custom bot, another automation). The body mirrors the
// alertResult shape the analysis webhook already returns, so a consumer
// that understands one understands both.
//
// The default body is a fixed JSON shape (webhookBody), which suits a
// consumer written for this service but not a chat product's incoming
// webhook, each of which wants its own schema (Slack: {"text": ...}; a
// Teams Workflows webhook: an Adaptive Card message). SetBodyTemplate
// swaps in a caller-supplied template for that case.
type WebhookChannel struct {
	name    string
	url     string
	method  string
	headers map[string]string
	client  *http.Client
	sleep   func(time.Duration)
	tmpl    *template.Template // nil = the fixed webhookBody JSON
}

// bodyTemplateFuncs is the function set a body_template may use. config
// validates a template against a stub with the same names — keep the two
// in step.
var bodyTemplateFuncs = template.FuncMap{
	// json renders any value as a JSON literal (strings come out quoted
	// and escaped), which is what makes a free-text field safe to drop
	// into a JSON template: {"text": {{json .Text}}}.
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}

// BodyTemplateData is what a body_template is executed against.
type BodyTemplateData struct {
	AlertName      string
	Host           string
	Summary        string
	AnalyzedBy     string
	EscalatedTo    string
	Error          string
	PendingURL     string
	MitigationNote string
	Similar        []SimilarIncident
	// Text is the whole notification pre-rendered as plain text (the same
	// content the Telegram push carries, without its HTML) — the one field
	// most chat webhooks need.
	Text string
}

// ParseBodyTemplate compiles a body_template. Exported so a bad template
// is refused at startup rather than at the first alert.
func ParseBodyTemplate(src string) (*template.Template, error) {
	return template.New("body").Funcs(bodyTemplateFuncs).Option("missingkey=error").Parse(src)
}

// SetBodyTemplate makes the channel send the rendering of t instead of
// the fixed JSON. The result must be valid JSON; Send reports (without
// retrying) when it isn't, which in practice means a template field was
// written without the json function.
func (w *WebhookChannel) SetBodyTemplate(t *template.Template) { w.tmpl = t }

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
	EscalatedTo      string            `json:"escalated_to,omitempty"`
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
	if w.tmpl != nil {
		payload, err := w.renderTemplate(msg)
		if err != nil {
			return err
		}
		return withRetry(3, []time.Duration{1 * time.Second, 3 * time.Second}, w.sleep, func() (bool, error) {
			return w.post(payload)
		})
	}
	body := webhookBody{
		AlertName:   msg.AlertName,
		Host:        msg.Host,
		Summary:     msg.Summary,
		AnalyzedBy:  msg.AnalyzedBy,
		EscalatedTo: msg.EscalatedTo,
		Error:       msg.Error,
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

func (w *WebhookChannel) renderTemplate(msg Message) ([]byte, error) {
	var out bytes.Buffer
	data := BodyTemplateData{
		AlertName:      msg.AlertName,
		Host:           msg.Host,
		Summary:        msg.Summary,
		AnalyzedBy:     msg.AnalyzedBy,
		EscalatedTo:    msg.EscalatedTo,
		Error:          msg.Error,
		PendingURL:     msg.PendingURL,
		MitigationNote: msg.MitigationNote,
		Similar:        msg.Similar,
		Text:           FormatPlainText(msg),
	}
	if err := w.tmpl.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("render body_template for channel %q: %w", w.name, err)
	}
	if !json.Valid(out.Bytes()) {
		return nil, fmt.Errorf("body_template for channel %q rendered something that is not valid JSON (wrap free-text fields in {{json ...}}): %.200s", w.name, out.String())
	}
	return out.Bytes(), nil
}

// FormatPlainText renders a Message as the same notification
// FormatTelegramText builds, minus Telegram's HTML markup and length cap.
// It is the .Text a body_template gets.
func FormatPlainText(msg Message) string {
	var b strings.Builder
	switch {
	case msg.Error != "":
		fmt.Fprintf(&b, "⚠️ %s (%s)\n無法產生摘要：%s", msg.AlertName, msg.Host, msg.Error)
	case msg.AnalyzedBy == "cloud" && msg.EscalatedTo != "":
		fmt.Fprintf(&b, "🔍 %s (%s)\n已升級至 cloud model（%s）深度分析\n\n%s", msg.AlertName, msg.Host, msg.EscalatedTo, msg.Summary)
	case msg.AnalyzedBy == "cloud":
		fmt.Fprintf(&b, "🔍 %s (%s)\n已升級至 cloud model 深度分析\n\n%s", msg.AlertName, msg.Host, msg.Summary)
	default:
		fmt.Fprintf(&b, "🚨 %s (%s)\n\n%s", msg.AlertName, msg.Host, msg.Summary)
	}
	if msg.MitigationNote != "" {
		b.WriteString("\n\n🛠 " + msg.MitigationNote)
	}
	if msg.PendingURL != "" {
		b.WriteString("\n\n✅ 確認這筆：\n" + msg.PendingURL)
	}
	if len(msg.Similar) > 0 {
		b.WriteString("\n\n📎 相似歷史事件：")
		for _, s := range msg.Similar {
			fmt.Fprintf(&b, "\n • %s — %s", s.Ref, s.Date)
			if s.URL != "" {
				b.WriteString("\n   " + s.URL)
			}
		}
	}
	return b.String()
}
