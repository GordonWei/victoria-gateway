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
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/maintenance"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/notify"
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
	_ = fs.Parse(args) // flag.ExitOnError already exits the process on a parse error

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
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
		APIKey:   cfg.Summarizer.APIKey,
		Timeout:  time.Duration(cfg.Summarizer.TimeoutSec) * time.Second,
	})
	summarizer := aiops.NewSummarizer(llm)

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
		case "aws-devops-agent":
			da := cfg.Cloud.DevOpsAgent
			if da == nil {
				fmt.Fprintln(os.Stderr, "❌ cloud.provider is \"aws-devops-agent\" but cloud.aws_devops_agent is not set")
				os.Exit(1)
			}
			cloud = model.NewDevOpsAgentClient(model.DevOpsAgentClientConfig{
				BinaryPath: da.BinaryPath,
				UserID:     da.UserID,
				Region:     da.Region,
				SpaceID:    da.SpaceID,
				Priority:   da.Priority,
			})
		default:
			fmt.Fprintf(os.Stderr, "❌ unknown cloud.provider %q (must be \"gemini\", \"anthropic\", or \"aws-devops-agent\")\n", cfg.Cloud.Provider)
			os.Exit(1)
		}
	}

	h := &handler{
		loki:        lokiClient,
		summarizer:  summarizer,
		cloud:       cloud,
		escalation:  cfg.Escalation,
		lookback:    lookback,
		limit:       limit,
		webhookAuth: cfg.WebhookAuth,
		metrics:     &metrics.Counters{},
		async:       cfg.WebhookAsync,
	}

	notifier, err := buildNotifier(cfg, h.metrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	h.notifier = notifier

	if len(cfg.MaintenanceWindows) > 0 {
		mw, err := maintenance.ParseWindows(cfg.MaintenanceWindows)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
		h.maintenanceWindows = mw
	}

	ragEnabled := cfg.RAG != nil && cfg.RAG.Enabled
	if ragEnabled {
		store, err := rag.OpenPostgres(cfg.RAG.PostgresDSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ rag: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = store.Close() }()
		h.rag = store
		h.ragEmbedder = rag.NewEmbedder(cfg.RAG.EmbeddingEndpoint, cfg.RAG.EmbeddingModel, cfg.RAG.EmbeddingAPIKey)
		h.ragTopK = cfg.RAG.TopK
		if h.ragTopK <= 0 {
			h.ragTopK = 3
		}
		h.ragShowSimilar = cfg.RAG.ShowSimilarInNotification == nil || *cfg.RAG.ShowSimilarInNotification
		h.ragSimThreshold = cfg.RAG.SimilarityThreshold
		if h.ragSimThreshold == 0 {
			h.ragSimThreshold = 0.75
		}
		h.publicBaseURL = strings.TrimRight(cfg.RAG.PublicBaseURL, "/")
		h.issueURL = issueURLBuilder(cfg.RAG)
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
	if ragEnabled {
		mux.HandleFunc("/incidents", h.handleIncidentsList)
		mux.HandleFunc("/incidents/", h.handleIncidentDetail)
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// A slow or stalled client mustn't pin a goroutine forever.
		// ReadTimeout is generous because Alertmanager payloads are small
		// and local — it's a stall guard, not a pacing device. No
		// WriteTimeout: in sync mode the response legitimately takes as
		// long as the slowest analysis (minutes on a cloud escalation).
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	fmt.Printf("🚀 victoria-gateway listening on %s (POST /webhook/alertmanager, async=%v)\n", addr, cfg.WebhookAsync)
	fmt.Printf("   loki: %s | summarizer: %s (%s) | notify: %v | cloud: %v | rag: %v\n",
		cfg.Loki.Endpoint, cfg.Summarizer.Endpoint, cfg.Summarizer.Model, h.notifier.Enabled(), cloud != nil, ragEnabled)

	// Graceful shutdown: SIGTERM/SIGINT stops accepting new requests,
	// then waits (bounded) for in-flight analyses — which may be running
	// as background goroutines in async mode, invisible to
	// http.Server.Shutdown — before exiting. Without this, a redeploy's
	// SIGTERM killed analyses mid-flight: a wasted LLM/cloud call, and
	// possibly a RAG capture or issue half-done.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		fmt.Fprintf(os.Stderr, "❌ serve: %v\n", err)
		os.Exit(1)
	case <-ctx.Done():
	}

	grace := time.Duration(cfg.ShutdownGraceSec) * time.Second
	if grace <= 0 {
		grace = 5 * time.Minute
	}
	log.Printf("shutdown: signal received, draining (grace %s)", grace)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: server close: %v", err)
	}

	done := make(chan struct{})
	go func() { h.inFlight.Wait(); close(done) }()
	select {
	case <-done:
		log.Printf("shutdown: all in-flight analyses finished")
	case <-time.After(grace):
		log.Printf("shutdown: grace period expired with analyses still in flight, exiting anyway")
	}
}

// buildNotifier assembles the notification router from config, honoring
// the backward-compat contract in the design doc: no `notifications`
// block means the top-level `telegram` block behaves exactly as before
// (every alert to one chat); with both, the top-level telegram becomes
// the implicit default route unless an explicit default exists, in which
// case it's ignored with a warning.
func buildNotifier(cfg *config.Config, m *metrics.Counters) (*notify.Router, error) {
	onResult := func(channel string, err error) {
		m.IncNotifyPush(channel, err != nil)
		if err != nil && channel == implicitTelegramChannel {
			// Keep the legacy counter moving for dashboards built before
			// per-channel metrics existed.
			m.IncTelegramPushFailuresTotal()
		}
	}

	var channels []notify.Channel
	var routes []notify.Route

	if cfg.Notifications != nil {
		for _, ch := range cfg.Notifications.Channels {
			switch ch.Type {
			case "telegram":
				channels = append(channels, notify.NewTelegramChannel(ch.Name, ch.BotToken, ch.ChatID))
			case "webhook":
				channels = append(channels, notify.NewWebhookChannel(ch.Name, ch.URL, ch.Method, ch.Headers))
			default:
				// config.Validate already rejected anything else.
				return nil, fmt.Errorf("notifications: unknown channel type %q", ch.Type)
			}
		}
		hasExplicitDefault := false
		for _, rt := range cfg.Notifications.Routes {
			routes = append(routes, notify.Route{Matchers: rt.Matchers, Channels: rt.Channels, Default: rt.Default})
			if rt.Default {
				hasExplicitDefault = true
			}
		}
		if cfg.Telegram.BotToken != "" {
			if hasExplicitDefault {
				log.Printf("notify: top-level telegram block ignored — notifications.routes already has a default route")
			} else {
				channels = append(channels, notify.NewTelegramChannel(implicitTelegramChannel, cfg.Telegram.BotToken, cfg.Telegram.ChatID))
				routes = append(routes, notify.Route{Default: true, Channels: []string{implicitTelegramChannel}})
			}
		}
	} else if cfg.Telegram.BotToken != "" {
		channels = append(channels, notify.NewTelegramChannel(implicitTelegramChannel, cfg.Telegram.BotToken, cfg.Telegram.ChatID))
		routes = append(routes, notify.Route{Default: true, Channels: []string{implicitTelegramChannel}})
	}

	return notify.NewRouter(channels, routes, onResult)
}

// implicitTelegramChannel names the channel synthesized from the
// top-level `telegram` config block. Underscore-prefixed so it can't
// collide with an operator-defined channel name (config validation
// requires those to be non-empty, but doesn't reserve names).
const implicitTelegramChannel = "_telegram"

// issueURLBuilder returns a function mapping a tracker issue number to a
// browsable URL, or nil when no tracker (or no URL scheme we can trust)
// is configured. Gitea's browse URL is derivable from its endpoint;
// github.com's is fixed; a GitHub Enterprise API endpoint is not
// reliably mappable to its web UI, so that case yields no URL rather
// than a guessed-wrong one.
func issueURLBuilder(ragCfg *config.RAGConfig) func(int64) string {
	switch {
	case ragCfg.Gitea != nil:
		base := strings.TrimRight(ragCfg.Gitea.Endpoint, "/")
		owner, repo := ragCfg.Gitea.Owner, ragCfg.Gitea.Repo
		return func(n int64) string {
			return fmt.Sprintf("%s/%s/%s/issues/%d", base, owner, repo, n)
		}
	case ragCfg.GitHub != nil && ragCfg.GitHub.Endpoint == "":
		owner, repo := ragCfg.GitHub.Owner, ragCfg.GitHub.Repo
		return func(n int64) string {
			return fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n)
		}
	default:
		return nil
	}
}

type handler struct {
	loki        *aiops.Client
	summarizer  *aiops.Summarizer
	notifier    *notify.Router // nil-safe; nil or channel-less means "no pushes"
	cloud       model.LLM      // nil if no cloud escalation target configured
	escalation  config.EscalationConfig
	lookback    time.Duration
	limit       int
	webhookAuth *config.WebhookAuthConfig // nil if the webhook endpoint requires no auth
	async       bool                      // respond 202 and analyze in the background (config.webhook_async)

	rag             rag.Store     // nil if RAG is disabled
	ragEmbedder     *rag.Embedder // nil if RAG is disabled
	ragTopK         int
	ragShowSimilar  bool
	ragSimThreshold float64
	publicBaseURL   string             // prefix for /incidents links in notifications; "" renders bare paths
	issueURL        func(int64) string // issue number → browse URL; nil if not derivable
	tracker         tracker.Tracker    // nil if no issue tracker (Gitea/GitHub) is configured

	metrics *metrics.Counters // nil-safe; see pkg/metrics

	maintenanceWindows []maintenance.Window

	// inFlight counts running analyses (sync or async) so shutdown can
	// drain them — http.Server.Shutdown only waits for open requests,
	// which in async mode have long since returned 202.
	inFlight sync.WaitGroup

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
	AnalyzedBy string `json:"analyzed_by,omitempty"` // "local" or "cloud" or "suppressed"
	Error      string `json:"error,omitempty"`

	muted   bool                     // not exported to JSON; controls whether notification is skipped
	similar []notify.SimilarIncident // past confirmed incidents above the similarity threshold, for notifications
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
	defer func() { _ = r.Body.Close() }()

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

	// Async mode: the filtering/dedup above already decided what will be
	// processed, so Alertmanager gets its answer now — a 202 with counts —
	// instead of waiting out the slowest analysis (minutes, on a cloud
	// escalation) and timing out. The analyses run as tracked background
	// goroutines; their results reach humans via notification channels,
	// which was always the real delivery path (the sync response body is
	// read by nothing but curl).
	if h.async {
		for _, alert := range toProcess {
			h.inFlight.Add(1)
			go func(alert aiops.Alert) {
				defer h.inFlight.Done()
				res := h.summarizeOne(alert)
				h.notifyResult(res, alert.Labels)
			}(alert)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(struct {
			Status   string `json:"status"`
			Accepted int    `json:"accepted"`
		}{Status: "accepted", Accepted: len(toProcess)}); err != nil {
			log.Printf("aiops: encode response: %v", err)
		}
		return
	}

	// Each goroutine only ever writes its own index, so results needs no
	// mutex — index isolation, not locking, is what makes this safe.
	results := make([]alertResult, len(toProcess))
	var wg sync.WaitGroup
	for i, alert := range toProcess {
		wg.Add(1)
		h.inFlight.Add(1)
		go func(i int, alert aiops.Alert) {
			defer wg.Done()
			defer h.inFlight.Done()
			res := h.summarizeOne(alert)
			results[i] = res
			h.notifyResult(res, alert.Labels)
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
	analysisStart := time.Now()
	defer func() {
		h.metrics.ObserveAnalysisDuration(time.Since(analysisStart))
		if res.Error != "" {
			h.metrics.IncAlertsErrorTotal()
		}
	}()

	res.AlertName = alert.Labels["alertname"]

	// Maintenance window check: before doing any Loki/LLM work, see if
	// this alert is suppressed (skip entirely) or muted (analyze but
	// don't push). Check uses the alert's full label set so matchers can
	// reference any label, not just host/alertname.
	if len(h.maintenanceWindows) > 0 {
		action, windowName := maintenance.CheckAll(h.maintenanceWindows, time.Now(), alert.Labels)
		switch action {
		case maintenance.ActionSuppress:
			log.Printf("aiops: alert %q suppressed by maintenance window %q", res.AlertName, windowName)
			h.metrics.IncMaintenanceSuppressedTotal()
			res.Summary = fmt.Sprintf("suppressed by maintenance window %q", windowName)
			res.AnalyzedBy = "suppressed"
			return
		case maintenance.ActionMute:
			log.Printf("aiops: alert %q muted by maintenance window %q (will analyze but not push)", res.AlertName, windowName)
			h.metrics.IncMaintenanceMutedTotal()
			res.muted = true
		}
	}

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

	lokiStart := time.Now()
	logs, err := h.loki.QueryRange(host, start.Add(-h.lookback), time.Now(), h.limit)
	h.metrics.ObserveLokiQueryDuration(time.Since(lokiStart))
	if err != nil {
		res.Error = fmt.Sprintf("loki query: %v", err)
		return
	}

	ragContext, similar := h.retrieveRAGContext(alert, logs)
	res.similar = similar

	localStart := time.Now()
	local, err := h.summarizer.Summarize(alert, logs, ragContext)
	h.metrics.ObserveLocalLLMDuration(time.Since(localStart))
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
		cloudStart := time.Now()
		cloudResult, cloudErr := aiops.SummarizeWithLLM(h.cloud, alert, logs, ragContext)
		h.metrics.ObserveCloudLLMDuration(time.Since(cloudStart))
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
// RAG is enabled. It returns two views of one search: the formatted
// prompt context (all top-K hits — a weak match is still context for the
// model) and the similar-incident references worth showing a human
// (filtered by the similarity threshold, since a notification link is a
// claim, not a hint). It's best-effort: any failure (embedding endpoint
// down, Postgres unreachable) is logged and treated as "nothing found"
// rather than failing the alert — a broken RAG store shouldn't take down
// the summarizer's core job.
func (h *handler) retrieveRAGContext(alert aiops.Alert, logs []aiops.LogEntry) (string, []notify.SimilarIncident) {
	if h.rag == nil || h.ragEmbedder == nil {
		return "", nil
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

	searchStart := time.Now()
	defer func() { h.metrics.ObserveRAGSearchDuration(time.Since(searchStart)) }()

	embedding, err := h.ragEmbedder.Embed(queryText)
	if err != nil {
		log.Printf("aiops: rag embed failed, continuing without past-incident context: %v", err)
		h.metrics.IncRAGSearchFailuresTotal()
		return "", nil
	}

	records, err := h.rag.Search(context.Background(), embedding, h.ragTopK)
	if err != nil {
		log.Printf("aiops: rag search failed, continuing without past-incident context: %v", err)
		h.metrics.IncRAGSearchFailuresTotal()
		return "", nil
	}
	return rag.FormatContext(records), h.similarIncidents(records)
}

// similarIncidents converts search hits above the similarity threshold
// into notification references — issue URL when the record has one,
// /incidents/{id} (prefixed with public_base_url if configured)
// otherwise, so every confirmed record is reachable one way or the
// other.
func (h *handler) similarIncidents(records []rag.Record) []notify.SimilarIncident {
	if !h.ragShowSimilar {
		return nil
	}
	var out []notify.SimilarIncident
	for _, r := range records {
		if r.Similarity < h.ragSimThreshold {
			continue
		}
		s := notify.SimilarIncident{Date: r.CreatedAt.Format("2006-01-02")}
		if r.GiteaIssueNumber != 0 && h.issueURL != nil {
			s.Ref = fmt.Sprintf("#%d %s (%s)", r.GiteaIssueNumber, r.AlertName, r.Host)
			s.URL = h.issueURL(r.GiteaIssueNumber)
		} else {
			s.Ref = fmt.Sprintf("incident/%d — %s (%s)", r.ID, r.AlertName, r.Host)
			s.URL = fmt.Sprintf("%s/incidents/%d", h.publicBaseURL, r.ID)
		}
		out = append(out, s)
	}
	return out
}

// notifyResult pushes one alert's outcome through the notification
// router. This is the only place the summary actually reaches a human —
// the webhook's HTTP response body is read by nothing (Alertmanager
// discards it), so without this push the LLM's answer would be computed
// and then thrown away. Failures inside channels are logged and counted,
// never returned: a broken push shouldn't change the webhook's HTTP
// response to Alertmanager, which would just make Alertmanager retry
// pointlessly.
func (h *handler) notifyResult(res alertResult, labels map[string]string) {
	if !h.notifier.Enabled() {
		return
	}
	if res.muted {
		return
	}
	h.notifier.Dispatch(notify.Message{
		AlertName:  res.AlertName,
		Host:       res.Host,
		Summary:    res.Summary,
		AnalyzedBy: res.AnalyzedBy,
		Error:      res.Error,
		Similar:    res.similar,
	}, labels)
}
