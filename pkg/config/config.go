// Package config loads victoria-gateway's YAML config. This service does one
// thing: receive Alertmanager webhooks, pull the surrounding Loki logs, ask
// an OpenAI-compatible LLM endpoint to summarize what happened, and push
// that summary to Telegram. The config shape is flat and matches that.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr string     `yaml:"listen_addr"` // e.g. ":8090"
	Loki       LokiConfig `yaml:"loki"`
	// LogSource, if set, switches which backend summarizeOne fetches an
	// alert's logs from — CloudWatch Logs Insights or GCP Cloud Logging
	// instead of the Loki block above. Nil (the default) or
	// LogSource.Type == "loki" keeps the original behavior unchanged:
	// loki.endpoint is required and used directly. loki.lookback_sec/
	// loki.limit remain the general "how far back"/"how many lines" knobs
	// regardless of which backend is actually active — they aren't
	// Loki-specific, just historically homed on that block.
	LogSource     *LogSourceConfig     `yaml:"log_source"`
	Summarizer    LLMConfig            `yaml:"summarizer"`
	Cloud         *CloudConfig         `yaml:"cloud"`      // optional: cloud model for escalated alerts
	Escalation    EscalationConfig     `yaml:"escalation"` // rules for when to escalate to Cloud
	Judge         *JudgeConfig         `yaml:"judge"`      // optional: additional escalation signal from TypeSafe AI's Jev, see JudgeConfig
	RAG           *RAGConfig           `yaml:"rag"`        // optional: past-incident retrieval
	Telegram      TelegramConfig       `yaml:"telegram"`
	Notifications *NotificationsConfig `yaml:"notifications"` // optional: multi-channel routing; nil keeps the single-Telegram behavior
	WebhookAuth   *WebhookAuthConfig   `yaml:"webhook_auth"`  // optional: require HTTP Basic Auth on the webhook endpoint
	// WebUIAuth, if set, requires HTTP Basic Auth on the read/write web
	// pages (/incidents, /pending, /maintenance-windows) — separately from
	// WebhookAuth, since the caller is a human browser rather than
	// Alertmanager and the risk profile changes once /pending gained a
	// POST confirm form (see cmd/victoria-gateway/auth.go). Nil (the
	// default) leaves these endpoints exactly as unauthenticated as they
	// were before this existed — see the README's "Securing the web UI"
	// section for why that's a real decision an operator has to make, not
	// a safe default this package can pick for them.
	WebUIAuth          *WebhookAuthConfig  `yaml:"webui_auth"`
	MaintenanceWindows []MaintenanceWindow `yaml:"maintenance_windows"` // optional: suppress or mute alerts during scheduled windows

	// WebhookAsync, when true, makes POST /webhook/alertmanager respond
	// 202 Accepted immediately after filtering/dedup and run the analyses
	// in the background, instead of holding the connection open until
	// every alert's Loki+LLM(+cloud) round trip finishes. Alertmanager's
	// own webhook timeout is far shorter than a minutes-long escalation,
	// so in sync mode every slow analysis makes Alertmanager time out and
	// redeliver (absorbed by fingerprint dedup, but noisy). Default false
	// keeps the old behavior — the response body carrying full results is
	// convenient for curl-driven testing — and deployments fronted by a
	// real Alertmanager should turn this on.
	WebhookAsync bool `yaml:"webhook_async"`

	// ShutdownGraceSec bounds how long a SIGTERM'd process waits for
	// in-flight analyses to finish before exiting anyway. Defaults to 300
	// (5 minutes) — long enough for a cloud escalation (339s was observed
	// once; most finish far sooner), short enough that a redeploy isn't
	// hostage to a hung upstream. Remember to raise the container
	// runtime's own kill grace (compose stop_grace_period) alongside it.
	ShutdownGraceSec int `yaml:"shutdown_grace_sec"`
}

// NotificationsConfig declares named delivery channels and the routes
// that pick between them per alert. See pkg/notify for semantics. The
// top-level `telegram` block stays the simple path: with no
// `notifications` block at all, behavior is exactly the pre-routing
// single-chat push.
type NotificationsConfig struct {
	Channels []NotifyChannelConfig `yaml:"channels"`
	Routes   []NotifyRouteConfig   `yaml:"routes"`
}

// NotifyChannelConfig is one named destination. Type selects which of
// the remaining fields matter: "telegram" uses BotToken/ChatID,
// "webhook" uses URL/Method/Headers.
type NotifyChannelConfig struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // "telegram" or "webhook"

	// telegram
	BotToken string `yaml:"bot_token"`
	ChatID   int64  `yaml:"chat_id"`

	// webhook
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`  // defaults to POST
	Headers map[string]string `yaml:"headers"` // sent verbatim, e.g. Authorization
}

// NotifyRouteConfig is one routing rule: first route whose matchers all
// match the alert's labels wins; a `default: true` route matches
// everything and must come last (evaluation is in config order).
type NotifyRouteConfig struct {
	Matchers map[string]string `yaml:"matchers"` // label name → value (same glob syntax as maintenance windows)
	Channels []string          `yaml:"channels"`
	Default  bool              `yaml:"default"`
}

// MaintenanceWindow defines a time window during which matching alerts are
// either completely suppressed (not analyzed, not pushed) or muted
// (analyzed and captured for RAG, but not pushed to Telegram). Useful for
// planned maintenance where you know alerts will fire and don't want to be
// notified or waste LLM calls on expected noise.
//
// JSON tags mirror the YAML tags (same snake_case names) so this struct
// can also be sent/received verbatim by the PUT /maintenance-windows
// admin API — an operator who's written `maintenance_windows:` YAML
// writes the same field names in the JSON body.
type MaintenanceWindow struct {
	Name     string            `yaml:"name" json:"name"`         // human-readable, printed in logs
	Schedule string            `yaml:"schedule" json:"schedule"` // periodic: "SAT 02:00-04:00", "DAILY 04:00-04:30", "1st-SUN 03:00-06:00"
	Start    string            `yaml:"start" json:"start"`       // one-time: ISO8601 start time
	End      string            `yaml:"end" json:"end"`           // one-time: ISO8601 end time
	Matchers map[string]string `yaml:"matchers" json:"matchers"` // label name → value (supports glob with *)
	Action   string            `yaml:"action" json:"action"`     // "suppress" or "mute"

	// Timezone is an optional IANA zone name (e.g. "Asia/Taipei") the
	// `schedule` form is evaluated in. Empty (the default) keeps the
	// original behavior: the process's local time zone. Only meaningful
	// for `schedule`-based windows — a one-time `start`/`end` window is
	// already an absolute instant (RFC3339 carries its own offset), so
	// Timezone doesn't change how it's matched.
	Timezone string `yaml:"timezone" json:"timezone"`
}

// WebhookAuthConfig, if set, makes the webhook handler require HTTP
// Basic Auth matching Username/Password on every request. Nothing about
// POST /webhook/alertmanager is authenticated otherwise — anyone who can
// reach the port can trigger a full analysis (an LLM call, possibly a
// cloud escalation, possibly a filed issue). That's an acceptable
// default on a private/home network, but it's a real exposure if this
// port is ever reachable from anywhere less trusted, so it's documented
// as a "turn this on unless you're sure" setting rather than defaulting
// it on — a default-on secret would just be another thing every existing
// deployment has to go set to keep working.
type WebhookAuthConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// LokiConfig points at the Loki instance to query for context around a
// fired alert.
type LokiConfig struct {
	Endpoint    string `yaml:"endpoint"`     // e.g. "http://loki:3100"; required unless log_source.type is set to something other than "loki"
	LookbackSec int    `yaml:"lookback_sec"` // how far before the alert's startsAt to begin the query window
	Limit       int    `yaml:"limit"`        // max log lines fetched per query
}

// LogSourceConfig selects which backend fetches an alert's logs. See
// Config.LogSource's doc comment for the default-to-Loki behavior when
// this whole block is omitted.
type LogSourceConfig struct {
	// Type is "loki" (default if empty), "cloudwatch", or "gcp_logging".
	Type       string            `yaml:"type"`
	CloudWatch *CloudWatchConfig `yaml:"cloudwatch"`
	GCPLogging *GCPLoggingConfig `yaml:"gcp_logging"`
}

// CloudWatchConfig configures pkg/cloudwatch as the log source. AWS
// credentials come from the SDK's default chain (env vars, ~/.aws/
// credentials, an EC2/ECS/EKS instance role, SSO, ...) — see
// pkg/cloudwatch's package doc for why there's deliberately no access-
// key/secret field here, same reasoning as cloud.provider: "bedrock".
type CloudWatchConfig struct {
	// Region is the AWS region the target log group(s) live in, e.g.
	// "us-east-1".
	Region string `yaml:"region"`
	// LogGroupNames is which log group(s) to search — Logs Insights can
	// query several in one request.
	LogGroupNames []string `yaml:"log_group_names"`
	// QueryTemplate overrides the default @message substring search.
	// Must contain the literal token "{{TERM}}" exactly once. See
	// pkg/cloudwatch's defaultQueryTemplate doc comment for why the right
	// query depends on your own log shape.
	QueryTemplate string `yaml:"query_template,omitempty"`
	// TimeoutSec bounds one query end-to-end, including the
	// StartQuery/GetQueryResults poll loop. 0 defaults to 30.
	TimeoutSec int `yaml:"timeout_sec,omitempty"`
}

// GCPLoggingConfig configures pkg/gcplogging as the log source.
// Credentials come from Application Default Credentials (gcloud auth
// application-default login locally, or the GCE/GKE/Cloud Run metadata
// server in production) — same no-static-credentials-field reasoning as
// CloudWatchConfig.
type GCPLoggingConfig struct {
	// ProjectID is the GCP project whose logs are searched.
	ProjectID string `yaml:"project_id"`
	// FilterTemplate overrides the default SEARCH("{{TERM}}") full-text
	// filter. Must contain the literal token "{{TERM}}" exactly once. See
	// pkg/gcplogging's defaultFilterTemplate doc comment for why a
	// structured field filter usually beats the generic default for
	// JSON-shaped logs.
	FilterTemplate string `yaml:"filter_template,omitempty"`
	// TimeoutSec bounds one query. 0 defaults to 30.
	TimeoutSec int `yaml:"timeout_sec,omitempty"`
}

// LLMConfig is the OpenAI-compatible endpoint the summarizer calls (LM
// Studio, Ollama, vLLM, etc. — anything that speaks /v1/chat/completions).
type LLMConfig struct {
	Endpoint string `yaml:"endpoint"`
	Model    string `yaml:"model"`
	// APIKey is optional, for pointing summarizer at a real cloud
	// OpenAI-compatible endpoint instead of an unauthenticated local
	// server — e.g. Gemini's OpenAI-compatibility layer at
	// generativelanguage.googleapis.com/v1beta/openai, or a LiteLLM
	// proxy. Sent as "Authorization: Bearer <key>"; omit for local
	// backends that don't need one.
	APIKey string `yaml:"api_key"`

	// TimeoutSec bounds one chat-completion call; 0 keeps the client's
	// 60s default. Exists because a local model server that unloaded its
	// model on idle (LM Studio JIT mode) can spend well over 60s
	// reloading it before the first token — observed in production
	// 2026-08-31, where the first analysis after an idle period failed
	// at exactly the 60s mark and the retry landed on a warm model. Set
	// this above the cold-load time (e.g. 180) to make that first
	// analysis succeed instead.
	TimeoutSec int `yaml:"timeout_sec"`
}

// CloudConfig is the cloud model endpoint escalated alerts get
// re-analyzed against. Only used when at least one Escalation rule can
// trigger, or the local model's own structured reply asks for escalation
// — see pkg/aiops.ShouldEscalate.
type CloudConfig struct {
	Provider string `yaml:"provider"` // "gemini" (default), "anthropic", "bedrock", "azure-openai", or "aws-devops-agent"
	Endpoint string `yaml:"endpoint"` // optional; each provider has its own default. Ignored by "bedrock" (region-based, see Region) and by "azure-openai" (required there instead, as the resource base URL)
	APIKey   string `yaml:"api_key"`  // ignored by "bedrock", which uses the AWS SDK's own credential chain instead — see model.BedrockClient
	Model    string `yaml:"model"`    // e.g. "gemini-2.5-flash", "claude-haiku-4-5", or a Bedrock model ID. Ignored by "azure-openai" — see Deployment

	// Region is the AWS region "bedrock" calls Bedrock in, e.g.
	// "us-east-1". Bedrock model availability varies by region. Ignored
	// by every other provider.
	Region string `yaml:"region"`

	// Deployment is the Azure deployment name "azure-openai" sends
	// requests to — not the underlying model name; see
	// model.AzureOpenAIClientConfig.Deployment for why these are
	// different things in Azure OpenAI. Ignored by every other provider.
	Deployment string `yaml:"deployment"`

	// APIVersion is the api-version query parameter "azure-openai" sends.
	// Optional; empty uses model.AzureOpenAIClient's own current default
	// rather than a value duplicated (and liable to go stale) here.
	// Ignored by every other provider.
	APIVersion string `yaml:"api_version"`

	// DevOpsAgent configures the "aws-devops-agent" provider. Ignored by
	// every other provider — see
	// model.DevOpsAgentClient for why this one escalates to an
	// AWS-account-aware investigation agent instead of a chat completion,
	// and why that only makes sense when the alert being escalated is
	// itself about AWS infrastructure.
	DevOpsAgent *DevOpsAgentConfig `yaml:"aws_devops_agent"`
}

// DevOpsAgentConfig points at a local `aws-devops-agent mcp` installation
// (pip install -e '.[mcp]' of aws-samples/sample-aws-devops-agent-acp-mcp)
// and the AgentSpace it should investigate against. The AgentSpace itself
// — its AWS account association and IAM role — is provisioned out of band;
// see that repo's ONBOARDING.md. This config only says how to reach it.
type DevOpsAgentConfig struct {
	BinaryPath string `yaml:"binary_path"` // defaults to "aws-devops-agent" resolved via PATH
	UserID     string `yaml:"user_id"`     // DEVOPS_AGENT_USER_ID
	Region     string `yaml:"region"`      // defaults to "us-east-1"
	SpaceID    string `yaml:"space_id"`    // DEVOPS_AGENT_SPACE_ID
	Priority   string `yaml:"priority"`    // CRITICAL/HIGH/MEDIUM/LOW/MINIMAL, defaults to HIGH
}

// EscalationConfig lists alerts that must always be re-analyzed by Cloud
// regardless of what the local model's own confidence/escalate signal
// says. This exists because a small local model's self-reported
// confidence isn't reliably calibrated — an operator who knows "this
// specific alert is always complex/sensitive in our environment" should
// be able to force escalation deterministically rather than hoping the
// local model notices.
type EscalationConfig struct {
	AlwaysCloud []string `yaml:"always_cloud"` // alertname values, matched case-insensitively
	// MaxPerHour caps how many alerts can escalate to Cloud within a
	// rolling hour, 0 (default) meaning unlimited. Exists because nothing
	// else bounds cloud spend if the local model's self-reported
	// escalate signal misfires broadly, or always_cloud ends up matching
	// more alerts than intended — an alert that would have escalated but
	// hits the cap stays on the local result instead of failing.
	MaxPerHour int `yaml:"max_per_hour"`
}

// JudgeConfig enables an independent second opinion from TypeSafe AI's
// Jev model (see pkg/judge) on top of the local summarizer's own
// self-reported escalate signal. Nil (or APIKey unset) disables this
// entirely — the existing aiops.ShouldEscalate logic (self-report OR
// escalation.always_cloud) is unaffected either way. When enabled, Jev is
// purely additive: it can only turn a non-escalating alert into an
// escalating one (EscalateProbability crossing EscalateThreshold), never
// suppress an escalation the existing logic already decided on, and a
// failed/unreachable Jev call falls back to the existing result rather
// than blocking anything — see cmd/victoria-gateway's combineEscalation.
type JudgeConfig struct {
	APIKey string `yaml:"api_key"` // TypeSafe AI System One API key; unset disables Jev entirely
	// EscalateThreshold is the minimum Noul probability (0..1) from
	// JudgeEscalation that triggers escalation on its own. 0 (the
	// default sentinel) means "use 0.70" — the action-threshold order of
	// magnitude TypeSafe's own llm_guardrails cookbook recommends;
	// nothing about that number is specific to this deployment's alert
	// mix, so revisit it once enough real Confirmed incidents exist to
	// check Jev's calibration against actual outcomes.
	EscalateThreshold float64 `yaml:"escalate_threshold"`
}

// RAGConfig controls optional retrieval of past incidents to ground the
// summarizer prompt. Nil (or Enabled: false) means victoria-gateway behaves
// exactly as it did before this existed — RAG is opt-in, not a
// requirement to run the service.
type RAGConfig struct {
	Enabled           bool   `yaml:"enabled"`
	PostgresDSN       string `yaml:"postgres_dsn"`       // e.g. "postgres://user:pass@host:5432/dbname"
	EmbeddingEndpoint string `yaml:"embedding_endpoint"` // OpenAI-compatible /v1/embeddings endpoint
	EmbeddingModel    string `yaml:"embedding_model"`    // e.g. "bge-m3"
	EmbeddingAPIKey   string `yaml:"embedding_api_key"`  // optional, only for an endpoint requiring auth
	TopK              int    `yaml:"top_k"`              // how many past incidents to retrieve; defaults to 3

	// ShowSimilarInNotification controls whether notifications carry a
	// "相似歷史事件" section linking past confirmed incidents. Nil means
	// true (on whenever RAG is on) — a *bool so an explicit `false` is
	// distinguishable from "not set".
	ShowSimilarInNotification *bool `yaml:"show_similar_in_notification"`

	// SimilarityThreshold is the minimum cosine similarity (0..1) a past
	// incident needs to be shown in a notification; defaults to 0.75.
	// This gates only what humans see — the summarizer prompt still
	// receives all top_k retrieved records, where a weaker match is
	// context, not a claim.
	SimilarityThreshold float64 `yaml:"similarity_threshold"`

	// MaskLogExcerpt, when true, redacts substrings in a captured log
	// excerpt that look like credential assignments (password=, token:,
	// Authorization: Bearer ...) before it's stored, using
	// pkg/mask.RedactLikelyCredentials — a shape-preserving, irreversible
	// rewrite. Off by default: for most alerts the actual content of an
	// error message is the root-cause signal, not something to hide, and
	// masking indiscriminately would defeat that. See the README's "What
	// data this stores, and where it goes" section before turning this on.
	MaskLogExcerpt bool `yaml:"mask_log_excerpt"`

	// PublicBaseURL, e.g. "http://172.16.100.6:8090", is what /incidents
	// links in notifications are prefixed with — the address a human's
	// browser can actually reach, which the process can't reliably guess
	// from its own listen_addr behind Docker port mapping. Empty means
	// links to records without a tracker issue render as a bare
	// "/incidents/{id}" path.
	PublicBaseURL string `yaml:"public_base_url"`

	// Gitea/GitHub: at most one should be set. Whichever is, every
	// analyzed alert files an issue there and captures a Pending record
	// alongside it (see pkg/rag.Store). With neither set, RAG still works
	// for Search/the `note` CLI, there's just no automatic capture path —
	// every record has to be added by hand.
	Gitea  *GiteaConfig  `yaml:"gitea"`
	GitHub *GitHubConfig `yaml:"github"`

	// CloseIssueOnWebConfirm, when true, makes confirming a record via
	// the /pending web form also close its linked tracker issue (posting
	// the typed-in resolution as the closing comment) — so the two
	// confirmation paths (web form, closing the issue yourself) stay in
	// sync regardless of which one you actually use. Off by default:
	// some setups want the issue tracker to stay the sole source of
	// truth for "is this really resolved" and treat the web form as a
	// faster way to just look, not to close things on the tracker's
	// behalf. No effect if neither gitea nor github is configured.
	CloseIssueOnWebConfirm bool `yaml:"close_issue_on_web_confirm"`
}

// GiteaConfig points at the repo Victoria Gateway files one issue per
// analyzed alert into, and that `victoria-gateway sync` later polls for
// closed issues to pull resolutions back from.
type GiteaConfig struct {
	Endpoint string `yaml:"endpoint"` // e.g. "https://gitea.ngu.tw"
	Token    string `yaml:"token"`
	Owner    string `yaml:"owner"` // repo owner, e.g. "admin"
	Repo     string `yaml:"repo"`  // e.g. "victoria-gateway-incidents"

	// WebhookSecret, if set, enables POST /webhook/gitea-issues: Gitea's
	// "Issues" webhook event, verified against this shared secret
	// (X-Gitea-Signature, HMAC-SHA256 over the raw body), triggers an
	// immediate resync of that one issue instead of waiting for the next
	// `victoria-gateway sync` cron tick. Configure a matching webhook on
	// this repo (Settings → Webhooks → Gitea, trigger "Issues") pointed
	// at this address. Leave unset to rely on cron `sync` alone — the
	// endpoint refuses all requests without a configured secret, it
	// never runs unauthenticated.
	WebhookSecret string `yaml:"webhook_secret"`
}

// GitHubConfig is the same idea as GiteaConfig, for anyone using GitHub
// Issues instead of a self-hosted Gitea instance.
type GitHubConfig struct {
	Endpoint string `yaml:"endpoint"` // optional; defaults to https://api.github.com (set for GitHub Enterprise)
	Token    string `yaml:"token"`    // a personal access token with Issues read/write on the target repo
	Owner    string `yaml:"owner"`
	Repo     string `yaml:"repo"` // a dedicated repo, not the code repo

	// WebhookSecret is the GitHub equivalent of GiteaConfig.WebhookSecret
	// — enables POST /webhook/github-issues, verified against
	// X-Hub-Signature-256. Configure a webhook on the repo (Settings →
	// Webhooks, content type application/json, event "Issues") with a
	// matching secret.
	WebhookSecret string `yaml:"webhook_secret"`
}

// TelegramConfig, if BotToken is set, makes the webhook handler push each
// alert's LLM summary to this chat via the Telegram Bot API. Without it,
// the summary is still computed but only ever ends up in the webhook's
// HTTP response body, which Alertmanager discards (it only checks the
// status code).
type TelegramConfig struct {
	BotToken string `yaml:"bot_token"`
	ChatID   int64  `yaml:"chat_id"`
}

// Validate checks the parts of Config that Load's YAML unmarshal can't —
// required fields, and combinations that parse fine but don't make sense
// together (e.g. an escalation rule with no cloud to escalate to). All
// three entry points (runServe, runNote, runSync) share one config.yaml,
// so this checks the fields the server needs even when called from note
// or sync — a real deployment's config.yaml already has them set, since
// the server has to be running for note/sync's captured/synced records to
// exist in the first place.
func (c *Config) Validate() error {
	if err := c.validateLogSource(); err != nil {
		return err
	}
	if c.Summarizer.Endpoint == "" {
		return fmt.Errorf("summarizer.endpoint is not set in config.yaml")
	}
	if c.Summarizer.TimeoutSec < 0 {
		return fmt.Errorf("summarizer.timeout_sec must be >= 0 (0 means the 60s default)")
	}
	// An escalation rule that can never fire (no Cloud configured) is a
	// silent no-op the operator almost certainly didn't intend — fail
	// loudly rather than have alerts quietly never escalate.
	if len(c.Escalation.AlwaysCloud) > 0 && c.Cloud == nil {
		return fmt.Errorf("escalation.always_cloud is set but cloud is not configured in config.yaml")
	}
	if c.Escalation.MaxPerHour < 0 {
		return fmt.Errorf("escalation.max_per_hour must be >= 0 (0 means unlimited)")
	}
	// An empty username/password would still technically "match" via
	// checkWebhookAuth's constant-time comparison if a caller explicitly
	// sent empty Basic Auth credentials — that's not the "require a real
	// secret" behavior an operator setting webhook_auth actually wants.
	if c.WebhookAuth != nil && (c.WebhookAuth.Username == "" || c.WebhookAuth.Password == "") {
		return fmt.Errorf("webhook_auth is set but username/password is empty — set both, or remove the webhook_auth block to leave the endpoint unauthenticated")
	}
	if c.WebUIAuth != nil && (c.WebUIAuth.Username == "" || c.WebUIAuth.Password == "") {
		return fmt.Errorf("webui_auth is set but username/password is empty — set both, or remove the webui_auth block to leave the web pages unauthenticated")
	}
	if c.RAG != nil && c.RAG.Enabled {
		if c.RAG.PostgresDSN == "" || c.RAG.EmbeddingEndpoint == "" || c.RAG.EmbeddingModel == "" {
			return fmt.Errorf("rag.enabled is true but postgres_dsn/embedding_endpoint/embedding_model is missing in config.yaml")
		}
		if c.RAG.SimilarityThreshold < 0 || c.RAG.SimilarityThreshold > 1 {
			return fmt.Errorf("rag.similarity_threshold must be between 0 and 1 (0 means the 0.75 default)")
		}
	}
	if c.ShutdownGraceSec < 0 {
		return fmt.Errorf("shutdown_grace_sec must be >= 0 (0 means the 300s default)")
	}
	if c.Judge != nil && (c.Judge.EscalateThreshold < 0 || c.Judge.EscalateThreshold > 1) {
		return fmt.Errorf("judge.escalate_threshold must be between 0 and 1 (0 means the 0.70 default)")
	}
	if err := c.validateNotifications(); err != nil {
		return err
	}
	if err := ValidateMaintenanceWindows(c.MaintenanceWindows); err != nil {
		return err
	}
	return nil
}

// validateLogSource checks Config.Loki/Config.LogSource together: exactly
// the block matching whichever backend is (implicitly or explicitly)
// selected must be filled in, so a typo'd or half-filled config fails at
// startup instead of surfacing as "logs are always empty" on the first
// alert.
func (c *Config) validateLogSource() error {
	logSourceType := "loki"
	if c.LogSource != nil && c.LogSource.Type != "" {
		logSourceType = c.LogSource.Type
	}

	switch logSourceType {
	case "loki":
		if c.Loki.Endpoint == "" {
			return fmt.Errorf("loki.endpoint is not set in config.yaml")
		}
	case "cloudwatch":
		cw := c.LogSource.CloudWatch
		if cw == nil || cw.Region == "" || len(cw.LogGroupNames) == 0 {
			return fmt.Errorf("log_source.type is \"cloudwatch\" but log_source.cloudwatch.region/log_group_names is missing in config.yaml")
		}
	case "gcp_logging":
		gl := c.LogSource.GCPLogging
		if gl == nil || gl.ProjectID == "" {
			return fmt.Errorf("log_source.type is \"gcp_logging\" but log_source.gcp_logging.project_id is missing in config.yaml")
		}
	default:
		return fmt.Errorf("log_source.type is %q, want \"loki\", \"cloudwatch\", or \"gcp_logging\"", logSourceType)
	}
	return nil
}

// ValidateMaintenanceWindows checks a set of maintenance window
// definitions in isolation — the same rules config.yaml's
// `maintenance_windows:` block is held to via Config.Validate, factored
// out so the PUT /maintenance-windows admin API (see
// cmd/victoria-gateway/maintenance_api.go) can validate a submitted batch
// before ever touching the live windows, without constructing a whole
// fake Config.
func ValidateMaintenanceWindows(defs []MaintenanceWindow) error {
	for i, mw := range defs {
		label := fmt.Sprintf("maintenance_windows[%d]", i)
		if mw.Name != "" {
			label = fmt.Sprintf("maintenance_windows[%d] (%s)", i, mw.Name)
		}
		hasSchedule := mw.Schedule != ""
		hasStartEnd := mw.Start != "" || mw.End != ""
		if hasSchedule && hasStartEnd {
			return fmt.Errorf("%s: cannot set both schedule and start/end — use one or the other", label)
		}
		if !hasSchedule && !hasStartEnd {
			return fmt.Errorf("%s: must set either schedule or start/end", label)
		}
		if hasStartEnd && (mw.Start == "" || mw.End == "") {
			return fmt.Errorf("%s: both start and end are required for a one-time window", label)
		}
		if len(mw.Matchers) == 0 {
			return fmt.Errorf("%s: matchers must not be empty (refusing to match all alerts)", label)
		}
		if mw.Action != "suppress" && mw.Action != "mute" {
			return fmt.Errorf("%s: action must be \"suppress\" or \"mute\", got %q", label, mw.Action)
		}
		if mw.Timezone != "" {
			if _, err := time.LoadLocation(mw.Timezone); err != nil {
				return fmt.Errorf("%s: invalid timezone %q: %w", label, mw.Timezone, err)
			}
		}
	}
	return nil
}

// validateNotifications checks the notifications block's internal
// consistency: channel names unique and typed correctly, routes
// referencing only defined channels, and the default route (if any)
// unique and last — a default that isn't last would shadow every route
// after it, which is a config the operator can't have meant.
func (c *Config) validateNotifications() error {
	n := c.Notifications
	if n == nil {
		return nil
	}
	if len(n.Channels) == 0 {
		return fmt.Errorf("notifications is set but has no channels")
	}
	if len(n.Routes) == 0 {
		return fmt.Errorf("notifications is set but has no routes — every channel needs at least one route to ever receive anything")
	}
	names := make(map[string]bool, len(n.Channels))
	for i, ch := range n.Channels {
		label := fmt.Sprintf("notifications.channels[%d]", i)
		if ch.Name != "" {
			label = fmt.Sprintf("notifications.channels[%d] (%s)", i, ch.Name)
		}
		if ch.Name == "" {
			return fmt.Errorf("%s: name is required", label)
		}
		if names[ch.Name] {
			return fmt.Errorf("%s: duplicate channel name %q", label, ch.Name)
		}
		names[ch.Name] = true
		switch ch.Type {
		case "telegram":
			if ch.BotToken == "" || ch.ChatID == 0 {
				return fmt.Errorf("%s: type telegram requires bot_token and chat_id", label)
			}
		case "webhook":
			if ch.URL == "" {
				return fmt.Errorf("%s: type webhook requires url", label)
			}
		default:
			return fmt.Errorf("%s: type must be \"telegram\" or \"webhook\", got %q", label, ch.Type)
		}
	}
	sawDefault := false
	for i, rt := range n.Routes {
		label := fmt.Sprintf("notifications.routes[%d]", i)
		if sawDefault {
			return fmt.Errorf("%s: unreachable — an earlier route has default: true, which matches everything", label)
		}
		if rt.Default {
			if len(rt.Matchers) > 0 {
				return fmt.Errorf("%s: a default route must not also have matchers (it matches everything)", label)
			}
			sawDefault = true
		} else if len(rt.Matchers) == 0 {
			return fmt.Errorf("%s: matchers must not be empty (or mark the route default: true)", label)
		}
		if len(rt.Channels) == 0 {
			return fmt.Errorf("%s: channels must not be empty", label)
		}
		for _, name := range rt.Channels {
			if !names[name] {
				return fmt.Errorf("%s: references undefined channel %q", label, name)
			}
		}
	}
	return nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := &Config{
		ListenAddr: ":8090",
		Loki:       LokiConfig{LookbackSec: 300, Limit: 200},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}
