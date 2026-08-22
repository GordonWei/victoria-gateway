// Command victoria-gateway is an HTTP server that receives Alertmanager
// webhooks, pulls the surrounding Loki logs for the alerting host, asks a
// local LLM to summarize what's going on, and pushes that summary to
// Telegram. See pkg/aiops and config.yaml.
//
// Two entry points share this binary: `victoria-gateway [flags]` (default)
// runs the server; `victoria-gateway note [flags]` records a confirmed
// incident resolution into the RAG store — see note.go.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "note" {
		runNote(os.Args[2:])
		return
	}
	runServe(os.Args[1:])
}

func runServe(args []string) {
	fs := flag.NewFlagSet("victoria-gateway", flag.ExitOnError)
	configPath := os.Getenv("VICTORIA_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "/etc/victoria-gateway/config.yaml"
	}
	fs.StringVar(&configPath, "config", configPath, "path to config.yaml")
	port := fs.String("port", "", "override listen port, e.g. 8090 (defaults to config's listen_addr)")
	fs.Parse(args)

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
	// An escalation rule that can never fire (no Cloud configured) is a
	// silent no-op the operator almost certainly didn't intend — fail
	// loudly at startup rather than have alerts quietly never escalate.
	if len(cfg.Escalation.AlwaysCloud) > 0 && cfg.Cloud == nil {
		fmt.Fprintln(os.Stderr, "❌ escalation.always_cloud is set but cloud is not configured in config.yaml")
		os.Exit(1)
	}
	if cfg.RAG != nil && cfg.RAG.Enabled {
		if cfg.RAG.PostgresDSN == "" || cfg.RAG.EmbeddingEndpoint == "" || cfg.RAG.EmbeddingModel == "" {
			fmt.Fprintln(os.Stderr, "❌ rag.enabled is true but postgres_dsn/embedding_endpoint/embedding_model is missing in config.yaml")
			os.Exit(1)
		}
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

	var cloud model.LLM
	if cfg.Cloud != nil {
		switch cfg.Cloud.Provider {
		case "", "gemini":
			cloud = model.NewGeminiClient(model.GeminiClientConfig{
				Endpoint: cfg.Cloud.Endpoint,
				APIKey:   cfg.Cloud.APIKey,
				Model:    cfg.Cloud.Model,
			})
		case "anthropic":
			cloud = model.NewAnthropicClient(model.AnthropicClientConfig{
				Endpoint: cfg.Cloud.Endpoint,
				APIKey:   cfg.Cloud.APIKey,
				Model:    cfg.Cloud.Model,
			})
		default:
			fmt.Fprintf(os.Stderr, "❌ unknown cloud.provider %q (must be \"gemini\" or \"anthropic\")\n", cfg.Cloud.Provider)
			os.Exit(1)
		}
	}

	h := &handler{
		loki:       lokiClient,
		summarizer: summarizer,
		telegram:   telegram,
		cloud:      cloud,
		escalation: cfg.Escalation,
		lookback:   lookback,
		limit:      limit,
	}

	ragEnabled := cfg.RAG != nil && cfg.RAG.Enabled
	if ragEnabled {
		store, err := rag.OpenPostgres(cfg.RAG.PostgresDSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ rag: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()
		h.rag = store
		h.ragEmbedder = rag.NewEmbedder(cfg.RAG.EmbeddingEndpoint, cfg.RAG.EmbeddingModel)
		h.ragTopK = cfg.RAG.TopK
		if h.ragTopK <= 0 {
			h.ragTopK = 3
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/alertmanager", h.handleAlertmanagerWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	fmt.Printf("🚀 victoria-gateway listening on %s (POST /webhook/alertmanager)\n", addr)
	fmt.Printf("   loki: %s | summarizer: %s (%s) | telegram: %v | cloud: %v | rag: %v\n",
		cfg.Loki.Endpoint, cfg.Summarizer.Endpoint, cfg.Summarizer.Model, telegram.Enabled(), cloud != nil, ragEnabled)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "❌ serve: %v\n", err)
		os.Exit(1)
	}
}

type handler struct {
	loki       *aiops.Client
	summarizer *aiops.Summarizer
	telegram   *aiops.TelegramNotifier
	cloud      model.LLM // nil if no cloud escalation target configured
	escalation config.EscalationConfig
	lookback   time.Duration
	limit      int

	rag         rag.Store     // nil if RAG is disabled
	ragEmbedder *rag.Embedder // nil if RAG is disabled
	ragTopK     int
}

// alertResult is one entry in the JSON array returned by
// handleAlertmanagerWebhook — one per alert in the incoming payload.
// Exactly one of Summary/Error is set.
type alertResult struct {
	AlertName  string `json:"alert_name"`
	Host       string `json:"host,omitempty"`
	Summary    string `json:"summary,omitempty"`
	AnalyzedBy string `json:"analyzed_by,omitempty"` // "local" or "cloud"
	Error      string `json:"error,omitempty"`
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

	ragContext := h.retrieveRAGContext(alert, logs)

	local, err := h.summarizer.Summarize(alert, logs, ragContext)
	if err != nil {
		res.Error = fmt.Sprintf("summarize: %v", err)
		return res
	}
	if local.ParseFailed {
		log.Printf("aiops: local model reply for alert %q was not valid structured JSON; showing raw reply", res.AlertName)
	}

	result := local
	res.AnalyzedBy = "local"

	if escalate, reason := aiops.ShouldEscalate(res.AlertName, local, h.escalation.AlwaysCloud); escalate && h.cloud != nil {
		cloudResult, cloudErr := aiops.SummarizeWithLLM(h.cloud, alert, logs, ragContext)
		if cloudErr != nil {
			// Escalation failing isn't fatal to the alert — the local
			// summary is still a real (if less confident) answer, and an
			// operator seeing it plus this log line knows to double-check
			// it themselves rather than getting nothing.
			log.Printf("aiops: cloud escalation failed for alert %q (%s): %v", res.AlertName, reason, cloudErr)
		} else {
			log.Printf("aiops: alert %q escalated to cloud (%s)", res.AlertName, reason)
			result = cloudResult
			res.AnalyzedBy = "cloud"
		}
	}

	res.Summary = result.Summary
	return res
}

// retrieveRAGContext looks up past incidents similar to this alert, if
// RAG is enabled. It's best-effort: any failure (embedding endpoint down,
// Postgres unreachable) is logged and treated as "no context found"
// rather than failing the alert — a broken RAG store shouldn't take down
// the summarizer's core job.
func (h *handler) retrieveRAGContext(alert aiops.Alert, logs []aiops.LogEntry) string {
	if h.rag == nil || h.ragEmbedder == nil {
		return ""
	}

	host, _ := alert.Host()
	description := alert.Annotations["description"]
	if description == "" {
		description = alert.Annotations["summary"]
	}
	logLines := make([]string, len(logs))
	for i, l := range logs {
		logLines[i] = l.Line
	}
	queryText := rag.BuildQueryText(alert.Labels["alertname"], host, description, logLines)

	embedding, err := h.ragEmbedder.Embed(queryText)
	if err != nil {
		log.Printf("aiops: rag embed failed, continuing without past-incident context: %v", err)
		return ""
	}

	records, err := h.rag.Search(context.Background(), embedding, h.ragTopK)
	if err != nil {
		log.Printf("aiops: rag search failed, continuing without past-incident context: %v", err)
		return ""
	}
	return rag.FormatContext(records)
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
	switch {
	case res.Error != "":
		text = fmt.Sprintf("⚠️ <b>%s</b> (%s)\n無法產生摘要：%s",
			html.EscapeString(res.AlertName), html.EscapeString(res.Host), html.EscapeString(res.Error))
	case res.AnalyzedBy == "cloud":
		text = fmt.Sprintf("🔍 <b>%s</b> (%s)\n<i>已升級至 cloud model 深度分析</i>\n\n%s",
			html.EscapeString(res.AlertName), html.EscapeString(res.Host), html.EscapeString(res.Summary))
	default:
		text = fmt.Sprintf("🚨 <b>%s</b> (%s)\n\n%s",
			html.EscapeString(res.AlertName), html.EscapeString(res.Host), html.EscapeString(res.Summary))
	}

	if err := h.telegram.SendMessage(text); err != nil {
		log.Printf("aiops: telegram push failed for alert %q: %v", res.AlertName, err)
	}
}
