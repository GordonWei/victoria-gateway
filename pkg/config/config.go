// Package config loads victoria-gateway's YAML config. This service does one
// thing: receive Alertmanager webhooks, pull the surrounding Loki logs, ask
// an OpenAI-compatible LLM endpoint to summarize what happened, and push
// that summary to Telegram. The config shape is flat and matches that.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr  string             `yaml:"listen_addr"` // e.g. ":8090"
	Loki        LokiConfig         `yaml:"loki"`
	Summarizer  LLMConfig          `yaml:"summarizer"`
	Cloud       *CloudConfig       `yaml:"cloud"`      // optional: cloud model for escalated alerts
	Escalation  EscalationConfig   `yaml:"escalation"` // rules for when to escalate to Cloud
	RAG         *RAGConfig         `yaml:"rag"`        // optional: past-incident retrieval
	Telegram    TelegramConfig     `yaml:"telegram"`
	WebhookAuth *WebhookAuthConfig `yaml:"webhook_auth"` // optional: require HTTP Basic Auth on the webhook endpoint
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
}

// CloudConfig is the cloud model endpoint escalated alerts get
// re-analyzed against. Only used when at least one Escalation rule can
// trigger, or the local model's own structured reply asks for escalation
// — see pkg/aiops.ShouldEscalate.
type CloudConfig struct {
	Provider string `yaml:"provider"` // "gemini" (default) or "anthropic"
	Endpoint string `yaml:"endpoint"` // optional; each provider has its own default
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"` // e.g. "gemini-2.5-flash" or "claude-haiku-4-5"
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
	Endpoint string `yaml:"endpoint"` // e.g. "https://gitea.example.com"
	Token    string `yaml:"token"`
	Owner    string `yaml:"owner"` // repo owner, e.g. "ops-team"
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
