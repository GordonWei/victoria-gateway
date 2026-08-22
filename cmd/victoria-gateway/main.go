// Command victoria-gateway is an HTTP server that receives Alertmanager
// webhooks, pulls the surrounding Loki logs for the alerting host, asks a
// local LLM to summarize what's going on, and pushes that summary to
// Telegram. See pkg/aiops and config.yaml.
//
// Three entry points share this binary: `victoria-gateway [flags]`
// (default) runs the server; `victoria-gateway note [flags]` records a
// confirmed incident resolution into the RAG store directly, or confirms
// an existing pending one by id — see note.go; `victoria-gateway sync`
// pulls resolutions back from closed Gitea issues linked to pending
// records — see sync.go.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
	"github.com/gordonwei/victoria-gateway/pkg/tracker"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "note":
			runNote(os.Args[2:])
			return
		case "sync":
			runSync(os.Args[2:])
			return
		}
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
	if cfg.Escalation.MaxPerHour < 0 {
		fmt.Fprintln(os.Stderr, "❌ escalation.max_per_hour must be >= 0 (0 means unlimited)")
		os.Exit(1)
	}
	// An empty username/password would still technically "match" via
	// checkWebhookAuth's constant-time comparison if a caller explicitly
	// sent empty Basic Auth credentials — that's not the "require a real
	// secret" behavior an operator setting webhook_auth actually wants.
	if cfg.WebhookAuth != nil && (cfg.WebhookAuth.Username == "" || cfg.WebhookAuth.Password == "") {
		fmt.Fprintln(os.Stderr, "❌ webhook_auth is set but username/password is empty — set both, or remove the webhook_auth block to leave the endpoint unauthenticated")
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
		loki:        lokiClient,
		summarizer:  summarizer,
		telegram:    telegram,
		cloud:       cloud,
		escalation:  cfg.Escalation,
		lookback:    lookback,
		limit:       limit,
		webhookAuth: cfg.WebhookAuth,
		metrics:     &metrics.Counters{},
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
		t, err := buildTracker(cfg.RAG)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
		h.tracker = t
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/alertmanager", h.handleAlertmanagerWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", h.metrics.Handler())

	fmt.Printf("🚀 victoria-gateway listening on %s (POST /webhook/alertmanager)\n", addr)
	fmt.Printf("   loki: %s | summarizer: %s (%s) | telegram: %v | cloud: %v | rag: %v\n",
		cfg.Loki.Endpoint, cfg.Summarizer.Endpoint, cfg.Summarizer.Model, telegram.Enabled(), cloud != nil, ragEnabled)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "❌ serve: %v\n", err)
		os.Exit(1)
	}
}

type handler struct {
	loki        *aiops.Client
	summarizer  *aiops.Summarizer
	telegram    *aiops.TelegramNotifier
	cloud       model.LLM // nil if no cloud escalation target configured
	escalation  config.EscalationConfig
	lookback    time.Duration
	limit       int
	webhookAuth *config.WebhookAuthConfig // nil if the webhook endpoint requires no auth

	rag         rag.Store     // nil if RAG is disabled
	ragEmbedder *rag.Embedder // nil if RAG is disabled
	ragTopK     int
	tracker     tracker.Tracker // nil if no issue tracker (Gitea/GitHub) is configured

	metrics *metrics.Counters // nil-safe; see pkg/metrics

	dedupMu  sync.Mutex
	recentFP map[string]time.Time // fingerprint -> last-processed time, see claimFingerprint

	escalationMu          sync.Mutex
	escalationWindowStart time.Time
	escalationCount       int
}

// allowEscalation reports whether an escalation to Cloud is still within
// escalation.max_per_hour, claiming a slot if so. escalation.MaxPerHour
// <= 0 means unlimited. The window is fixed (not sliding): it resets to a
// fresh hour the first time an escalation is attempted after the previous
// window expired, rather than tracking each escalation's own expiry —
// simpler, and "at most N per rolling-ish hour" is all this needs to be a
// useful spend guardrail, not a precise rate limiter.
func (h *handler) allowEscalation() bool {
	if h.escalation.MaxPerHour <= 0 {
		return true
	}
	h.escalationMu.Lock()
	defer h.escalationMu.Unlock()
	now := time.Now()
	if now.Sub(h.escalationWindowStart) >= time.Hour {
		h.escalationWindowStart = now
		h.escalationCount = 0
	}
	if h.escalationCount >= h.escalation.MaxPerHour {
		return false
	}
	h.escalationCount++
	return true
}

// checkWebhookAuth reports whether the request is authorized to hit the
// webhook — always true when webhookAuth isn't configured (the default;
// see WebhookAuthConfig's doc comment on why that's not itself a
// contradiction). Uses constant-time comparison so a timing side-channel
// can't be used to guess the password one byte at a time.
func (h *handler) checkWebhookAuth(r *http.Request) bool {
	if h.webhookAuth == nil {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.webhookAuth.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(h.webhookAuth.Password)) == 1
	return userOK && passOK
}

// dedupWindow bounds how long a fingerprint is considered "already
// handled" after being claimed. It only needs to be longer than
// Alertmanager's own retry cadence for one notification attempt (its
// webhook timeout times a couple of retries, plus group_interval) — not
// how long the alert might stay firing. A still-firing alert that
// genuinely persists past this window and gets redelivered (e.g. a
// repeat_interval re-notification) is treated as a fresh episode and
// re-analyzed; that's an acceptable tradeoff for not needing an "is this
// alert still active" signal from Alertmanager, which the webhook
// payload alone doesn't reliably give.
//
// recentFP lives only in process memory (see the handler struct below) —
// a restart clears it. In the narrow window right after a restart, a
// retry that lands inside what would have been the old dedup window can
// reprocess and re-file an issue. Not persisted on purpose: the fix would
// mean every deployment needs a durable store just for this, when RAG
// (the one store this service already has) is itself optional. A rare,
// harmless-worst-case duplicate on restart is the accepted tradeoff.
const dedupWindow = 10 * time.Minute

// claimFingerprint reports whether this is the first time this alert
// fingerprint has been seen within dedupWindow (and records it as seen
// now) — false means the caller should skip it as a duplicate delivery
// of an episode already being handled. An empty fingerprint always
// claims true (defensive: real Alertmanager payloads always carry one,
// but nothing here should silently drop an alert just because a
// synthetic/test payload omitted it).
func (h *handler) claimFingerprint(fp string) bool {
	if fp == "" {
		return true
	}
	h.dedupMu.Lock()
	defer h.dedupMu.Unlock()
	if h.recentFP == nil {
		h.recentFP = make(map[string]time.Time)
	}
	now := time.Now()
	if seenAt, ok := h.recentFP[fp]; ok && now.Sub(seenAt) < dedupWindow {
		return false
	}
	h.recentFP[fp] = now
	for k, t := range h.recentFP {
		if now.Sub(t) >= dedupWindow {
			delete(h.recentFP, k)
		}
	}
	return true
}

// clearFingerprint drops the dedup entry for a fingerprint whose alert
// just resolved, so if the same alert fires again later as a genuinely
// new episode it isn't mistaken for a duplicate of the old one.
func (h *handler) clearFingerprint(fp string) {
	h.dedupMu.Lock()
	defer h.dedupMu.Unlock()
	delete(h.recentFP, fp)
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
	if !h.checkWebhookAuth(r) {
		h.metrics.IncWebhookAuthRejectedTotal()
		w.Header().Set("WWW-Authenticate", `Basic realm="victoria-gateway"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

	// Filter first (cheap, and must stay a plain sequential loop —
	// claimFingerprint/clearFingerprint mutate shared dedup state and
	// deciding what to skip has to happen before any concurrent work
	// starts, not interleaved with it), then fan the surviving alerts out
	// concurrently. A single payload can carry several alerts, and each
	// one blocks on Loki + a local LLM call + possibly a cloud escalation
	// + RAG/tracker calls — running them one at a time meant a payload of
	// N alerts took roughly N times as long to answer Alertmanager, which
	// is exactly the kind of latency that made Alertmanager's own webhook
	// timeout matter in the first place.
	var toProcess []aiops.Alert
	for _, alert := range payload.Alerts {
		// A "resolved" delivery means the problem stopped, not that
		// there's something new to diagnose — analyzing it would just
		// waste an LLM call and file a second Gitea issue for the same
		// episode. Alertmanager's own telegram_configs (in
		// alertmanager.yml, separate from this webhook) already tells
		// the human it resolved; this only clears the dedup entry so a
		// later, genuinely new firing of the same alert isn't treated as
		// a duplicate of the old episode.
		if alert.Status == "resolved" {
			h.clearFingerprint(alert.Fingerprint)
			h.metrics.IncResolvedSkippedTotal()
			continue
		}

		// Alertmanager retries a notification attempt it considers
		// failed (its own webhook timeout, or its dispatch loop
		// superseding a still-in-flight attempt) by POSTing the same
		// still-firing alert again — without this, each retry runs the
		// full analysis again and files another Gitea issue for what is
		// the same episode. claimFingerprint is a short-window dedup (see
		// its doc comment), not permanent — the same alertname firing
		// again as a genuinely new episode later still gets analyzed.
		if !h.claimFingerprint(alert.Fingerprint) {
			log.Printf("aiops: alert %q (fingerprint=%s) already processed recently, skipping duplicate delivery", alert.Labels["alertname"], alert.Fingerprint)
			h.metrics.IncDedupSkippedTotal()
			continue
		}

		toProcess = append(toProcess, alert)
	}

	// Each goroutine only ever writes its own index, so results needs no
	// mutex — index isolation, not locking, is what makes this safe.
	results := make([]alertResult, len(toProcess))
	var wg sync.WaitGroup
	for i, alert := range toProcess {
		wg.Add(1)
		go func(i int, alert aiops.Alert) {
			defer wg.Done()
			res := h.summarizeOne(alert)
			results[i] = res
			h.notifyTelegram(res)
		}(i, alert)
	}
	wg.Wait()

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
func (h *handler) summarizeOne(alert aiops.Alert) (res alertResult) {
	h.metrics.IncAlertsTotal()
	defer func() {
		if res.Error != "" {
			h.metrics.IncAlertsErrorTotal()
		}
	}()

	res.AlertName = alert.Labels["alertname"]

	host, ok := alert.Host()
	if !ok {
		res.Error = fmt.Sprintf("alert has no \"host\" or \"instance\" label (fingerprint=%s)", alert.Fingerprint)
		return
	}
	res.Host = host

	start, err := alert.StartTime()
	if err != nil {
		res.Error = fmt.Sprintf("parse startsAt: %v", err)
		return
	}

	logs, err := h.loki.QueryRange(host, start.Add(-h.lookback), time.Now(), h.limit)
	if err != nil {
		res.Error = fmt.Sprintf("loki query: %v", err)
		return
	}

	ragContext := h.retrieveRAGContext(alert, logs)

	local, err := h.summarizer.Summarize(alert, logs, ragContext)
	if err != nil {
		res.Error = fmt.Sprintf("summarize: %v", err)
		return
	}
	if local.ParseFailed {
		log.Printf("aiops: local model reply for alert %q was not valid structured JSON; showing raw reply", res.AlertName)
	}

	result := local
	res.AnalyzedBy = "local"

	escalate, reason := aiops.ShouldEscalate(res.AlertName, local, h.escalation.AlwaysCloud)
	if escalate && h.cloud != nil && !h.allowEscalation() {
		log.Printf("aiops: alert %q would escalate (%s) but escalation.max_per_hour is already reached this hour; staying on the local result", res.AlertName, reason)
		h.metrics.IncEscalationRateLimitedTotal()
		escalate = false
	}
	if escalate && h.cloud != nil {
		cloudResult, cloudErr := aiops.SummarizeWithLLM(h.cloud, alert, logs, ragContext)
		if cloudErr != nil {
			// Escalation failing isn't fatal to the alert — the local
			// summary is still a real (if less confident) answer, and an
			// operator seeing it plus this log line knows to double-check
			// it themselves rather than getting nothing.
			log.Printf("aiops: cloud escalation failed for alert %q (%s): %v", res.AlertName, reason, cloudErr)
			h.metrics.IncEscalationFailuresTotal()
		} else {
			log.Printf("aiops: alert %q escalated to cloud (%s)", res.AlertName, reason)
			result = cloudResult
			res.AnalyzedBy = "cloud"
			h.metrics.IncEscalationsTotal()
		}
	}

	res.Summary = result.Summary
	h.captureIncident(alert, logs, result, res.AnalyzedBy)
	return
}

// captureIncident records what was just analyzed as a Pending RAG record
// — not retrievable by Search until someone confirms it (via `note
// --id` directly, or `sync` reading a linked Gitea issue's closing
// comment). This runs on every successfully analyzed alert when RAG is
// enabled, whether or not Gitea is configured: capture always happens,
// filing an issue is just one (optional) way to eventually get a
// resolution back. Best-effort like retrieveRAGContext — failures are
// logged, never fail the alert itself.
func (h *handler) captureIncident(alert aiops.Alert, logs []aiops.LogEntry, result aiops.SummarizeResult, analyzedBy string) {
	if h.rag == nil || h.ragEmbedder == nil {
		return
	}

	host, _ := alert.Host()
	logLines := make([]string, len(logs))
	for i, l := range logs {
		logLines[i] = l.Line
	}
	// Embedded on the final analysis result rather than the raw
	// pre-analysis alert text retrieveRAGContext uses for its query — the
	// summary is a richer description of what this incident actually was,
	// which is what a future similar incident should match against.
	embedding, err := h.ragEmbedder.Embed(rag.BuildQueryText(alert.Labels["alertname"], host, result.Summary, logLines))
	if err != nil {
		log.Printf("aiops: rag capture embed failed, incident not recorded: %v", err)
		h.metrics.IncRAGCaptureFailuresTotal()
		return
	}

	var issueNumber int64
	if h.tracker != nil {
		title := fmt.Sprintf("[%s] %s", alert.Labels["alertname"], host)
		body := fmt.Sprintf(
			"**分析結果（%s）：**\n\n%s\n\n---\n此 Issue 由 Victoria Gateway 自動建立。調查完後請在關閉前留一則留言說明實際原因/怎麼修的，`victoria-gateway sync` 會把它讀回 RAG 資料庫，供未來類似告警參考。",
			analyzedBy, result.Summary)
		n, err := h.tracker.CreateIssue(context.Background(), title, body)
		if err != nil {
			log.Printf("aiops: create issue failed, capturing without a linked issue: %v", err)
			h.metrics.IncTrackerCreateIssueFailuresTotal()
		} else {
			issueNumber = n
		}
	}

	logExcerpt := strings.Join(logLines, "\n")
	const maxLogExcerpt = 4000 // keep the stored row a reasonable size; the full log is in Loki anyway
	if len(logExcerpt) > maxLogExcerpt {
		logExcerpt = logExcerpt[len(logExcerpt)-maxLogExcerpt:]
	}

	rec := rag.Record{
		AlertName:        alert.Labels["alertname"],
		Host:             host,
		LogExcerpt:       logExcerpt,
		Summary:          result.Summary,
		GiteaIssueNumber: issueNumber,
	}
	id, err := h.rag.InsertPending(context.Background(), rec, embedding)
	if err != nil {
		log.Printf("aiops: rag insert pending failed: %v", err)
		h.metrics.IncRAGCaptureFailuresTotal()
		return
	}
	h.metrics.IncRAGCaptureTotal()
	if issueNumber != 0 {
		log.Printf("aiops: captured pending incident id=%d, filed gitea issue #%d", id, issueNumber)
	} else {
		log.Printf("aiops: captured pending incident id=%d (no gitea issue)", id)
	}
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
		h.metrics.IncRAGSearchFailuresTotal()
		return ""
	}

	records, err := h.rag.Search(context.Background(), embedding, h.ragTopK)
	if err != nil {
		log.Printf("aiops: rag search failed, continuing without past-incident context: %v", err)
		h.metrics.IncRAGSearchFailuresTotal()
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
		h.metrics.IncTelegramPushFailuresTotal()
	}
}
