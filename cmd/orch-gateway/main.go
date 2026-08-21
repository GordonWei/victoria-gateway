// Command orch-gateway is an HTTP server that receives Alertmanager
// webhooks, pulls the surrounding Loki logs for the alerting host, asks a
// local LLM to summarize what's going on, and pushes that summary to
// Telegram. See pkg/aiops and config.yaml.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gordonwei/orch-gateway/pkg/aiops"
	"github.com/gordonwei/orch-gateway/pkg/config"
	"github.com/gordonwei/orch-gateway/pkg/model"
)

func main() {
	configPath := os.Getenv("ORCH_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "/etc/orch-gateway/config.yaml"
	}
	flag.StringVar(&configPath, "config", configPath, "path to config.yaml")
	port := flag.String("port", "", "override listen port, e.g. 8090 (defaults to config's listen_addr)")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if cfg.Loki.Endpoint == "" {
		fmt.Fprintln(os.Stderr, "❌ loki.endpoint is not set in config.yaml")
		os.Exit(1)
	}
	if cfg.Summarizer.Endpoint == "" {
		fmt.Fprintln(os.Stderr, "❌ summarizer.endpoint is not set in config.yaml")
		os.Exit(1)
	}

	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":8090"
	}
	if *port != "" {
		addr = ":" + *port
	}

	lookback := time.Duration(cfg.Loki.LookbackSec) * time.Second
	if lookback <= 0 {
		lookback = 5 * time.Minute
	}
	limit := cfg.Loki.Limit
	if limit <= 0 {
		limit = 200
	}

	lokiClient := aiops.NewClient(cfg.Loki.Endpoint)
	llm := model.NewOpenAIClient(model.OpenAIClientConfig{
		Endpoint: cfg.Summarizer.Endpoint,
		Model:    cfg.Summarizer.Model,
		Backend:  "aiops-summarizer",
	})
	summarizer := aiops.NewSummarizer(llm)
	telegram := aiops.NewTelegramNotifier(cfg.Telegram.BotToken, cfg.Telegram.ChatID)

	h := &handler{
		loki:       lokiClient,
		summarizer: summarizer,
		telegram:   telegram,
		lookback:   lookback,
		limit:      limit,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/alertmanager", h.handleAlertmanagerWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	fmt.Printf("🚀 orch-gateway listening on %s (POST /webhook/alertmanager)\n", addr)
	fmt.Printf("   loki: %s | summarizer: %s (%s) | telegram: %v\n",
		cfg.Loki.Endpoint, cfg.Summarizer.Endpoint, cfg.Summarizer.Model, telegram.Enabled())
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "❌ serve: %v\n", err)
		os.Exit(1)
	}
}

type handler struct {
	loki       *aiops.Client
	summarizer *aiops.Summarizer
	telegram   *aiops.TelegramNotifier
	lookback   time.Duration
	limit      int
}

// alertResult is one entry in the JSON array returned by
// handleAlertmanagerWebhook — one per alert in the incoming payload.
// Exactly one of Summary/Error is set.
type alertResult struct {
	AlertName string `json:"alert_name"`
	Host      string `json:"host,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (h *handler) handleAlertmanagerWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	payload, err := aiops.ParseWebhook(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	results := make([]alertResult, 0, len(payload.Alerts))
	for _, alert := range payload.Alerts {
		res := h.summarizeOne(alert)
		results = append(results, res)
		h.notifyTelegram(res)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Results []alertResult `json:"results"`
	}{Results: results}); err != nil {
		log.Printf("aiops: encode response: %v", err)
	}
}

// summarizeOne handles a single alert end to end. Failures for one alert
// (bad host label, Loki unreachable, LLM error) are captured in the result
// rather than aborting the whole webhook request — a payload can carry
// multiple alerts, and one bad one shouldn't blank out the rest.
func (h *handler) summarizeOne(alert aiops.Alert) alertResult {
	res := alertResult{AlertName: alert.Labels["alertname"]}

	host, ok := alert.Host()
	if !ok {
		res.Error = fmt.Sprintf("alert has no \"host\" or \"instance\" label (fingerprint=%s)", alert.Fingerprint)
		return res
	}
	res.Host = host

	start, err := alert.StartTime()
	if err != nil {
		res.Error = fmt.Sprintf("parse startsAt: %v", err)
		return res
	}

	logs, err := h.loki.QueryRange(host, start.Add(-h.lookback), time.Now(), h.limit)
	if err != nil {
		res.Error = fmt.Sprintf("loki query: %v", err)
		return res
	}

	summary, err := h.summarizer.Summarize(alert, logs)
	if err != nil {
		res.Error = fmt.Sprintf("summarize: %v", err)
		return res
	}

	res.Summary = summary
	return res
}

// notifyTelegram pushes one alert's outcome to Telegram, if a bot token is
// configured. This is the only place the summary actually reaches a human
// — the webhook's HTTP response body is read by nothing (Alertmanager
// discards it), so without this push the LLM's answer would be computed
// and then thrown away. Failures here are logged, not returned: a broken
// Telegram push shouldn't change the webhook's HTTP response to
// Alertmanager, which would just make Alertmanager retry pointlessly.
func (h *handler) notifyTelegram(res alertResult) {
	if !h.telegram.Enabled() {
		return
	}

	// The alert name/host come from our own labels and are safe, but the
	// summary (LLM output) and error (may embed raw log/error text) are
	// not — Telegram's HTML parse_mode rejects the whole message on an
	// unescaped '<', '>', or '&', so those two must be escaped.
	var text string
	if res.Error != "" {
		text = fmt.Sprintf("⚠️ <b>%s</b> (%s)\n無法產生摘要：%s",
			html.EscapeString(res.AlertName), html.EscapeString(res.Host), html.EscapeString(res.Error))
	} else {
		text = fmt.Sprintf("🚨 <b>%s</b> (%s)\n\n%s",
			html.EscapeString(res.AlertName), html.EscapeString(res.Host), html.EscapeString(res.Summary))
	}

	if err := h.telegram.SendMessage(text); err != nil {
		log.Printf("aiops: telegram push failed for alert %q: %v", res.AlertName, err)
	}
}
