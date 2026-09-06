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
	ListenAddr         string               `yaml:"listen_addr"` // e.g. ":8090"
	Loki               LokiConfig           `yaml:"loki"`
	Summarizer         LLMConfig            `yaml:"summarizer"`
	Cloud              *CloudConfig         `yaml:"cloud"`      // optional: cloud model for escalated alerts
	Escalation         EscalationConfig     `yaml:"escalation"` // rules for when to escalate to Cloud
	RAG                *RAGConfig           `yaml:"rag"`        // optional: past-incident retrieval
	Telegram           TelegramConfig       `yaml:"telegram"`
	Notifications      *NotificationsConfig `yaml:"notifications"`       // optional: multi-channel routing; nil keeps the single-Telegram behavior
	WebhookAuth        *WebhookAuthConfig   `yaml:"webhook_auth"`        // optional: require HTTP Basic Auth on the webhook endpoint
	MaintenanceWindows []MaintenanceWindow  `yaml:"maintenance_windows"` // optional: suppress or mute alerts during scheduled windows

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
	Endpoint    string `yaml:"endpoint"`     // e.g. "http://loki:3100"
	LookbackSec int    `yaml:"lookback_sec"` // how far before the alert's startsAt to begin the query window
	Limit       int    `yaml:"limit"`        // max log lines fetched per query
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
	Provider string `yaml:"provider"` // "gemini" (default), "anthropic", or "aws-devops-agent"
	Endpoint string `yaml:"endpoint"` // optional; each provider has its own default
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"` // e.g. "gemini-2.5-flash" or "claude-haiku-4-5"

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
}

// GiteaConfig points at the repo Victoria Gateway files one issue per
// analyzed alert into, and that `victoria-gateway sync` later polls for
// closed issues to pull resolutions back from.
type GiteaConfig struct {
	Endpoint string `yaml:"endpoint"` // e.g. "https://gitea.ngu.tw"
	Token    string `yaml:"token"`
	Owner    string `yaml:"owner"` // repo owner, e.g. "admin"
	Repo     string `yaml:"repo"`  // e.g. "victoria-gateway-incidents"
}

// GitHubConfig is the same idea as GiteaConfig, for anyone using GitHub
// Issues instead of a self-hosted Gitea instance.
type GitHubConfig struct {
	Endpoint string `yaml:"endpoint"` // optional; defaults to https://api.github.com (set for GitHub Enterprise)
	Token    string `yaml:"token"`    // a personal access token with Issues read/write on the target repo
	Owner    string `yaml:"owner"`
	Repo     string `yaml:"repo"` // a dedicated repo, not the code repo
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
	if c.Loki.Endpoint == "" {
		return fmt.Errorf("loki.endpoint is not set in config.yaml")
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
	if err := c.validateNotifications(); err != nil {
		return err
	}
	if err := ValidateMaintenanceWindows(c.MaintenanceWindows); err != nil {
		return err
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
