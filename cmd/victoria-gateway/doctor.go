package main

// doctor.go implements `victoria-gateway doctor`: load and validate the
// config, then probe every dependency it names and print one line per
// check (OK / WARN / FAIL / SKIP). It never runs the server and, by
// default, has no side effects anywhere: probes only read (Loki /ready,
// GET /v1/models, Telegram getMe, a tracker's repo info, Alertmanager
// status, a Postgres schema query, one embedding of a fixed string).
// Two things that cost money or reach people are opt-in: --probe-cloud
// sends one tiny chat request to each chat-style escalation target, and
// --send-test pushes a message marked [TEST] to every notification
// channel. Investigation targets (aws-devops-agent, gcp-cloud-assist) are
// never probed, since even a minimal request starts a billed
// investigation. Every line of output passes through the doctor's
// redactor, so secrets from the config never reach the terminal.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/mask"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/notify"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

type doctorStatus string

const (
	statusOK   doctorStatus = "OK"
	statusWarn doctorStatus = "WARN"
	statusFail doctorStatus = "FAIL"
	statusSkip doctorStatus = "SKIP"
)

// checkResult is one line of doctor output.
type checkResult struct {
	Category string
	Name     string
	Status   doctorStatus
	Detail   string
	Took     time.Duration
}

type doctorOptions struct {
	probeCloud     bool
	probeCloudLogs bool
	sendTest       bool
	timeout        time.Duration // per check
}

// doctorStore is the slice of the RAG store the doctor reads.
type doctorStore interface {
	HasDupOf(ctx context.Context) (bool, error)
	DistinctEmbeddingModels(ctx context.Context) ([]string, error)
	Close() error
}

// doctorEnv holds every point where a probe touches the outside world, so
// tests can point them at httptest servers and count calls to the ones
// that must stay untouched without an explicit flag.
type doctorEnv struct {
	http           *http.Client
	telegramAPI    string
	openStore      func(dsn string) (doctorStore, error)
	buildCloud     func(label string, c *config.CloudConfig) (model.LLM, error)
	buildLogSource func(label string, ls *config.LogSourceConfig, defaultLoki string) (aiops.LogSource, error)
	newChannel     func(ch config.NotifyChannelConfig) (notify.Channel, error)
	now            func() time.Time
}

func defaultDoctorEnv() doctorEnv {
	return doctorEnv{
		http:        &http.Client{},
		telegramAPI: "https://api.telegram.org",
		openStore: func(dsn string) (doctorStore, error) {
			s, err := rag.OpenPostgres(dsn)
			if err != nil {
				return nil, err
			}
			return s, nil
		},
		buildCloud:     buildCloud,
		buildLogSource: buildLogSourceEntry,
		newChannel:     newNotifyChannel,
		now:            time.Now,
	}
}

// newNotifyChannel builds one configured channel the way buildNotifier does.
func newNotifyChannel(ch config.NotifyChannelConfig) (notify.Channel, error) {
	switch ch.Type {
	case "telegram":
		return notify.NewTelegramChannel(ch.Name, ch.BotToken, ch.ChatID), nil
	case "webhook":
		wh := notify.NewWebhookChannel(ch.Name, ch.URL, ch.Method, ch.Headers)
		if ch.BodyTemplate != "" {
			tmpl, err := notify.ParseBodyTemplate(ch.BodyTemplate)
			if err != nil {
				return nil, fmt.Errorf("body_template: %w", err)
			}
			wh.SetBodyTemplate(tmpl)
		}
		return wh, nil
	}
	return nil, fmt.Errorf("unknown channel type %q", ch.Type)
}

func runDoctorCmd(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := os.Getenv("VICTORIA_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "/etc/victoria-gateway/config.yaml"
	}
	fs.StringVar(&configPath, "config", configPath, "path to config.yaml")
	var opts doctorOptions
	fs.BoolVar(&opts.probeCloud, "probe-cloud", false, "also send one tiny (billed) chat request to each chat-style cloud escalation target")
	fs.BoolVar(&opts.probeCloudLogs, "probe-cloud-logs", false, "also run a one-minute read-only query against CloudWatch / GCP Cloud Logging sources")
	fs.BoolVar(&opts.sendTest, "send-test", false, "also push a [TEST] message to every notification channel")
	fs.DurationVar(&opts.timeout, "timeout", 15*time.Second, "time limit for each check")
	_ = fs.Parse(args)
	os.Exit(doctor(configPath, opts, defaultDoctorEnv(), os.Stdout))
}

// doctor runs every check, prints the report and returns the exit code:
// 1 if anything failed, 0 otherwise (WARN and SKIP don't fail).
func doctor(configPath string, opts doctorOptions, env doctorEnv, w io.Writer) int {
	if opts.timeout <= 0 {
		opts.timeout = 15 * time.Second
	}
	_, _ = fmt.Fprintf(w, "victoria-gateway doctor (%s, commit %s)\n", version, commit)
	cfg, err := config.Load(configPath)
	if err == nil {
		err = cfg.ValidateForServe()
	}
	if err != nil {
		// No config means nothing to redact against yet; mask what we can.
		printResults(w, []checkResult{{Category: "config", Name: configPath, Status: statusFail, Detail: mask.RedactLikelyCredentials(err.Error())}}, newRedactor(nil))
		return 1
	}
	results := runDoctorChecks(cfg, opts, env)
	results = append([]checkResult{{Category: "config", Name: configPath, Status: statusOK, Detail: "loaded and valid"}}, results...)
	printResults(w, results, newRedactor(cfg))
	for _, r := range results {
		if r.Status == statusFail {
			return 1
		}
	}
	return 0
}

// runDoctorChecks runs the checks in a fixed order: config warnings, log
// sources, local models, Postgres, embedding, notifications, tracker,
// Alertmanager, cloud targets.
func runDoctorChecks(cfg *config.Config, opts doctorOptions, env doctorEnv) []checkResult {
	var out []checkResult
	for _, w := range securityWarnings(cfg) {
		out = append(out, checkResult{Category: "config", Name: "exposure", Status: statusWarn, Detail: strings.TrimSpace(strings.TrimPrefix(w, "⚠️"))})
	}
	run := func(category, name string, fn func(ctx context.Context) (doctorStatus, string)) {
		out = append(out, runCheck(category, name, opts.timeout, fn))
	}

	// 1. Log sources.
	if len(cfg.LogSources) > 0 {
		names := make([]string, 0, len(cfg.LogSources))
		for n := range cfg.LogSources {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ls := cfg.LogSources[n]
			run("log source", "log_sources."+n, func(ctx context.Context) (doctorStatus, string) {
				return probeLogSource(ctx, env, opts, "log_sources."+n, ls, cfg.Loki.Endpoint)
			})
		}
	} else {
		ls := cfg.LogSource
		if ls != nil && ls.Loki != nil {
			cp := *ls
			cp.Loki = nil
			ls = &cp
		}
		run("log source", "log_source ("+logSourceType(cfg)+")", func(ctx context.Context) (doctorStatus, string) {
			return probeLogSource(ctx, env, opts, "log_source", ls, cfg.Loki.Endpoint)
		})
	}

	// 2. Local summarizer and its fallbacks.
	llms := append([]config.LLMConfig{cfg.Summarizer}, cfg.Summarizer.Fallbacks...)
	for i, lc := range llms {
		name := "summarizer"
		if i > 0 {
			name = fmt.Sprintf("summarizer.fallbacks[%d]", i-1)
		}
		lc := lc
		run("local model", name+" "+lc.Model, func(ctx context.Context) (doctorStatus, string) {
			c := model.NewOpenAIClient(model.OpenAIClientConfig{Endpoint: lc.Endpoint, Model: lc.Model, Backend: "doctor", APIKey: lc.APIKey})
			if err := c.Probe(ctx); err != nil {
				return statusFail, err.Error()
			}
			return statusOK, "GET /v1/models answered at " + lc.Endpoint
		})
	}

	// 3-4. RAG: Postgres and the embedding endpoint.
	if cfg.RAG != nil && cfg.RAG.Enabled {
		rc := cfg.RAG
		run("postgres", "rag.postgres_dsn", func(ctx context.Context) (doctorStatus, string) {
			return probePostgres(ctx, env, rc)
		})
		run("embedding", rc.EmbeddingModel, func(context.Context) (doctorStatus, string) {
			vec, err := rag.NewEmbedder(rc.EmbeddingEndpoint, rc.EmbeddingModel, rc.EmbeddingAPIKey).Embed("victoria-gateway doctor")
			if err != nil {
				return statusFail, err.Error()
			}
			if len(vec) == 0 {
				return statusFail, "endpoint returned an empty embedding"
			}
			return statusOK, fmt.Sprintf("%d-dimensional embedding from %s", len(vec), rc.EmbeddingEndpoint)
		})
	} else {
		out = append(out, checkResult{Category: "postgres", Name: "rag", Status: statusSkip, Detail: "rag.enabled is off"})
	}

	// 5. Notification channels.
	out = append(out, notificationChecks(cfg, opts, env)...)

	// 6. Tracker.
	if cfg.RAG != nil && cfg.RAG.Enabled && (cfg.RAG.Gitea != nil || cfg.RAG.GitHub != nil) {
		rc := cfg.RAG
		run("tracker", trackerName(rc), func(ctx context.Context) (doctorStatus, string) {
			return probeTracker(ctx, env, rc)
		})
	} else {
		out = append(out, checkResult{Category: "tracker", Name: "tracker", Status: statusSkip, Detail: "no tracker configured"})
	}

	// 7. Alertmanager API.
	if am := cfg.Alertmanager; am != nil && am.Endpoint != "" {
		run("alertmanager", am.Endpoint, func(ctx context.Context) (doctorStatus, string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(am.Endpoint, "/")+"/api/v2/status", nil)
			if err != nil {
				return statusFail, err.Error()
			}
			if am.Username != "" {
				req.SetBasicAuth(am.Username, am.Password)
			}
			return httpCheck(env.http, req, "GET /api/v2/status")
		})
	} else {
		out = append(out, checkResult{Category: "alertmanager", Name: "alertmanager", Status: statusSkip, Detail: "no alertmanager block configured"})
	}

	// 8. Cloud escalation targets.
	out = append(out, cloudChecks(cfg, opts, env)...)
	return out
}

// runCheck runs fn under a per-check deadline. A probe that ignores its
// context (some clients have no context parameter) is still cut off at
// the deadline and reported as a timeout; its goroutine is abandoned,
// which is fine for a one-shot command.
func runCheck(category, name string, timeout time.Duration, fn func(ctx context.Context) (doctorStatus, string)) checkResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	type res struct {
		s doctorStatus
		d string
	}
	done := make(chan res, 1)
	start := time.Now()
	go func() {
		s, d := fn(ctx)
		done <- res{s, d}
	}()
	select {
	case r := <-done:
		return checkResult{Category: category, Name: name, Status: r.s, Detail: r.d, Took: time.Since(start)}
	case <-ctx.Done():
		return checkResult{Category: category, Name: name, Status: statusFail, Detail: fmt.Sprintf("timed out after %s", timeout), Took: time.Since(start)}
	}
}

// httpCheck sends req and maps any 2xx to OK and anything else to FAIL.
func httpCheck(c *http.Client, req *http.Request, what string) (doctorStatus, string) {
	resp, err := c.Do(req)
	if err != nil {
		return statusFail, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusFail, fmt.Sprintf("%s returned %d: %s", what, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return statusOK, fmt.Sprintf("%s returned %d", what, resp.StatusCode)
}

func probeLogSource(ctx context.Context, env doctorEnv, opts doctorOptions, label string, ls *config.LogSourceConfig, defaultLoki string) (doctorStatus, string) {
	typ := "loki"
	if ls != nil && ls.Type != "" {
		typ = ls.Type
	}
	if typ == "loki" {
		endpoint := defaultLoki
		if ls != nil && ls.Loki != nil && ls.Loki.Endpoint != "" {
			endpoint = ls.Loki.Endpoint
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/ready", nil)
		if err != nil {
			return statusFail, err.Error()
		}
		return httpCheck(env.http, req, "Loki GET /ready")
	}
	if !opts.probeCloudLogs {
		return statusSkip, typ + " not queried; pass --probe-cloud-logs for a one-minute read-only query"
	}
	src, err := env.buildLogSource(label, ls, defaultLoki)
	if err != nil {
		return statusFail, err.Error()
	}
	end := env.now()
	if _, err := src.QueryRange(ctx, aiops.LogIdentity{Host: "victoria-gateway-doctor"}, end.Add(-time.Minute), end, 1); err != nil {
		return statusFail, err.Error()
	}
	return statusOK, typ + " answered a one-minute query"
}

func probePostgres(ctx context.Context, env doctorEnv, rc *config.RAGConfig) (doctorStatus, string) {
	store, err := env.openStore(rc.PostgresDSN)
	if err != nil {
		return statusFail, err.Error()
	}
	defer func() { _ = store.Close() }()
	hasDup, err := store.HasDupOf(ctx)
	if err != nil {
		return statusFail, err.Error()
	}
	models, err := store.DistinctEmbeddingModels(ctx)
	if err != nil {
		return statusFail, err.Error()
	}
	var warns []string
	if !hasDup {
		warns = append(warns, "incidents has no dup_of column (run pkg/rag/migrate_0003_dup_of.sql to enable batch confirm)")
	}
	if msg, ok := rag.CheckEmbeddingModelDrift(rc.EmbeddingModel, models); !ok {
		warns = append(warns, msg)
	}
	if len(warns) > 0 {
		return statusWarn, "reachable; " + strings.Join(warns, "; ")
	}
	return statusOK, "reachable, schema up to date, no embedding model drift"
}

func trackerName(rc *config.RAGConfig) string {
	if rc.GitHub != nil {
		return "github " + rc.GitHub.Owner + "/" + rc.GitHub.Repo
	}
	return "gitea " + rc.Gitea.Owner + "/" + rc.Gitea.Repo
}

// probeTracker reads the repository's metadata; it never creates or edits
// an issue.
func probeTracker(ctx context.Context, env doctorEnv, rc *config.RAGConfig) (doctorStatus, string) {
	var reqURL, auth string
	if g := rc.GitHub; g != nil {
		base := g.Endpoint
		if base == "" {
			base = "https://api.github.com"
		}
		reqURL = strings.TrimRight(base, "/") + "/repos/" + url.PathEscape(g.Owner) + "/" + url.PathEscape(g.Repo)
		auth = "Bearer " + g.Token
	} else {
		g := rc.Gitea
		reqURL = strings.TrimRight(g.Endpoint, "/") + "/api/v1/repos/" + url.PathEscape(g.Owner) + "/" + url.PathEscape(g.Repo)
		auth = "token " + g.Token
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return statusFail, err.Error()
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	return httpCheck(env.http, req, "GET repo")
}

// notificationChecks verifies Telegram bot tokens with getMe (no message
// sent) and, only with --send-test, pushes a [TEST] message to every
// channel. Webhook channels have no side-effect-free probe.
func notificationChecks(cfg *config.Config, opts doctorOptions, env doctorEnv) []checkResult {
	var chans []config.NotifyChannelConfig
	if cfg.Notifications != nil {
		chans = append(chans, cfg.Notifications.Channels...)
	}
	if cfg.Telegram.BotToken != "" {
		chans = append(chans, config.NotifyChannelConfig{Name: implicitTelegramChannel, Type: "telegram", BotToken: cfg.Telegram.BotToken, ChatID: cfg.Telegram.ChatID})
	}
	if len(chans) == 0 {
		return []checkResult{{Category: "notify", Name: "notifications", Status: statusSkip, Detail: "no notification channel configured"}}
	}
	var out []checkResult
	for _, ch := range chans {
		ch := ch
		if opts.sendTest {
			out = append(out, runCheck("notify", ch.Name+" ("+ch.Type+")", opts.timeout, func(context.Context) (doctorStatus, string) {
				c, err := env.newChannel(ch)
				if err != nil {
					return statusFail, err.Error()
				}
				err = c.Send(notify.Message{
					AlertName:  "[TEST] victoria-gateway doctor",
					Host:       "victoria-gateway",
					Summary:    "[TEST] victoria-gateway doctor --send-test：這是一則測試訊息，可以忽略。",
					AnalyzedBy: "local",
				})
				if err != nil {
					return statusFail, err.Error()
				}
				return statusOK, "[TEST] message sent"
			}))
			continue
		}
		if ch.Type != "telegram" {
			out = append(out, checkResult{Category: "notify", Name: ch.Name + " (" + ch.Type + ")", Status: statusSkip, Detail: "no side-effect-free probe; pass --send-test to push a [TEST] message"})
			continue
		}
		out = append(out, runCheck("notify", ch.Name+" (telegram)", opts.timeout, func(ctx context.Context) (doctorStatus, string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(env.telegramAPI, "/")+"/bot"+ch.BotToken+"/getMe", nil)
			if err != nil {
				return statusFail, err.Error()
			}
			return httpCheck(env.http, req, "getMe")
		}))
	}
	return out
}

// cloudChecks lists every configured escalation target. Nothing is sent
// without --probe-cloud, and investigation targets are never sent
// anything.
func cloudChecks(cfg *config.Config, opts doctorOptions, env doctorEnv) []checkResult {
	type target struct {
		label string
		c     *config.CloudConfig
	}
	var targets []target
	if cfg.Cloud != nil {
		targets = append(targets, target{"cloud", cfg.Cloud})
	}
	for i, fb := range cfg.CloudFallbacks {
		targets = append(targets, target{fmt.Sprintf("cloud_fallbacks[%d]", i), fb})
	}
	names := make([]string, 0, len(cfg.EscalationTargets))
	for n := range cfg.EscalationTargets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		targets = append(targets, target{"escalation_targets." + n, cfg.EscalationTargets[n]})
	}
	if len(targets) == 0 {
		return []checkResult{{Category: "cloud", Name: "escalation", Status: statusSkip, Detail: "no cloud escalation target configured"}}
	}
	var out []checkResult
	for _, t := range targets {
		t := t
		name := t.label + " (" + providerName(t.c) + ")"
		switch {
		case providerName(t.c) == "aws-devops-agent" || providerName(t.c) == "gcp-cloud-assist":
			out = append(out, checkResult{Category: "cloud", Name: name, Status: statusSkip, Detail: "investigation target, never probed (any request starts a billed investigation)"})
		case !opts.probeCloud:
			out = append(out, checkResult{Category: "cloud", Name: name, Status: statusSkip, Detail: "not called; pass --probe-cloud to send one tiny billed request"})
		default:
			out = append(out, runCheck("cloud", name, opts.timeout, func(context.Context) (doctorStatus, string) {
				llm, err := env.buildCloud(t.label, t.c)
				if err != nil {
					return statusFail, err.Error()
				}
				reply, err := llm.Chat([]model.Message{{Role: "user", Content: "Reply with the single word OK."}}, &model.ChatOptions{MaxTokens: 16})
				if err != nil {
					return statusFail, err.Error()
				}
				if strings.TrimSpace(reply) == "" {
					return statusFail, "empty reply"
				}
				return statusOK, "answered a one-line request (model " + llm.ModelName() + ")"
			}))
		}
	}
	return out
}

func printResults(w io.Writer, results []checkResult, red *redactor) {
	counts := map[doctorStatus]int{}
	for _, r := range results {
		counts[r.Status]++
		took := ""
		if r.Took > 0 {
			took = fmt.Sprintf(" (%s)", r.Took.Round(time.Millisecond))
		}
		_, _ = fmt.Fprintf(w, "[%-4s] %-12s %s: %s%s\n", r.Status, r.Category, red.redact(r.Name), red.redact(r.Detail), took)
	}
	_, _ = fmt.Fprintf(w, "%d OK, %d WARN, %d FAIL, %d SKIP\n", counts[statusOK], counts[statusWarn], counts[statusFail], counts[statusSkip])
}

// redactor removes every secret the config holds from doctor output. Error
// messages from HTTP clients quote request URLs (a Telegram bot token is
// part of its URL), DSNs and headers, so redaction is by value: each
// secret string is replaced wherever it appears, then the generic
// credential masker runs over what remains.
type redactor struct{ secrets []string }

func newRedactor(cfg *config.Config) *redactor {
	if cfg == nil {
		return &redactor{}
	}
	var s []string
	add := func(v string) {
		if len(v) >= 4 {
			s = append(s, v)
		}
	}
	addDSN := func(dsn string) {
		add(dsn)
		if u, err := url.Parse(dsn); err == nil && u.User != nil {
			if p, ok := u.User.Password(); ok {
				add(p)
			}
		}
		// key=value DSN form: password=...
		for _, f := range strings.Fields(dsn) {
			if v, ok := strings.CutPrefix(f, "password="); ok {
				add(strings.Trim(v, `'"`))
			}
		}
	}
	addLLM := func(c config.LLMConfig) { add(c.APIKey) }
	addLLM(cfg.Summarizer)
	for _, fb := range cfg.Summarizer.Fallbacks {
		addLLM(fb)
	}
	clouds := append([]*config.CloudConfig{cfg.Cloud}, cfg.CloudFallbacks...)
	for _, t := range cfg.EscalationTargets {
		clouds = append(clouds, t)
	}
	for _, c := range clouds {
		if c != nil {
			add(c.APIKey)
		}
	}
	if cfg.Judge != nil {
		add(cfg.Judge.APIKey)
	}
	add(cfg.Telegram.BotToken)
	if cfg.Notifications != nil {
		for _, ch := range cfg.Notifications.Channels {
			add(ch.BotToken)
			if u, err := url.Parse(ch.URL); err == nil && (u.Path != "" || u.RawQuery != "") {
				// A chat webhook URL's path/query is usually the credential.
				add(strings.TrimPrefix(u.RequestURI(), "/"))
			}
			for _, v := range ch.Headers {
				add(v)
				// "Bearer xyz" / "token xyz": the credential alone, too.
				if _, cred, ok := strings.Cut(v, " "); ok {
					add(cred)
				}
			}
		}
	}
	for _, a := range []*config.WebhookAuthConfig{cfg.WebhookAuth, cfg.WebUIAuth, cfg.MetricsAuth} {
		if a != nil {
			add(a.Password)
		}
	}
	if cfg.Alertmanager != nil {
		add(cfg.Alertmanager.Password)
	}
	if r := cfg.RAG; r != nil {
		addDSN(r.PostgresDSN)
		add(r.EmbeddingAPIKey)
		if r.Gitea != nil {
			add(r.Gitea.Token)
			add(r.Gitea.WebhookSecret)
		}
		if r.GitHub != nil {
			add(r.GitHub.Token)
			add(r.GitHub.WebhookSecret)
		}
	}
	if cfg.MCP != nil && cfg.MCP.HTTP != nil {
		add(cfg.MCP.HTTP.BearerToken)
	}
	if cfg.TelegramActions != nil {
		add(cfg.TelegramActions.HMACSecret)
	}
	// Longest first, so a DSN is replaced whole before its password is.
	sort.Slice(s, func(i, j int) bool { return len(s[i]) > len(s[j]) })
	return &redactor{secrets: s}
}

func (r *redactor) redact(text string) string {
	for _, s := range r.secrets {
		text = strings.ReplaceAll(text, s, "****")
		// net/url escapes some characters when it prints a URL in an error.
		if esc := url.PathEscape(s); esc != s {
			text = strings.ReplaceAll(text, esc, "****")
		}
	}
	return mask.RedactLikelyCredentials(text)
}
