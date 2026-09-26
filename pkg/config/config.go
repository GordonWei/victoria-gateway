// Package config loads victoria-gateway's YAML config. This service does one
// thing: receive Alertmanager webhooks, pull the surrounding Loki logs, ask
// an OpenAI-compatible LLM endpoint to summarize what happened, and push
// that summary to Telegram. The config shape is flat and matches that.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/glob"
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
	LogSource  *LogSourceConfig `yaml:"log_source"`
	Summarizer LLMConfig        `yaml:"summarizer"`
	Cloud      *CloudConfig     `yaml:"cloud"` // optional: cloud model for escalated alerts
	// CloudFallbacks are tried in order when the legacy cloud block's own
	// escalation call fails (connection error, timeout, non-2xx). Legacy
	// mode only — with hybrid_routes, list several targets in a route's
	// escalation instead. Requires cloud. One escalation, however many of
	// these it ends up trying, counts once against escalation.max_per_hour.
	CloudFallbacks []*CloudConfig   `yaml:"cloud_fallbacks"`
	Escalation     EscalationConfig `yaml:"escalation"` // rules for when to escalate to Cloud
	Judge          *JudgeConfig     `yaml:"judge"`      // optional: additional escalation signal from TypeSafe AI's Jev, see JudgeConfig
	// Alertmanager points at the Alertmanager instance `victoria-gateway
	// suppression-candidates --apply-silences` creates real, time-bounded
	// silences against. Optional — unset means --apply-silences refuses to
	// run; the tool's default print-only behavior needs no config at all.
	// See suppression.go's package doc for why this only ever creates
	// silences (self-expiring), never edits Alertmanager's permanent
	// route.routes — that stays a human's call, same as before this field
	// existed.
	Alertmanager  *AlertmanagerConfig  `yaml:"alertmanager"`
	RAG           *RAGConfig           `yaml:"rag"` // optional: past-incident retrieval
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

	// LogSources, EscalationTargets and HybridRoutes together let one
	// deployment treat cloud accounts as an extension of the on-prem
	// machine room instead of a separate world: each alert is routed by
	// its labels to the log backend it actually lives in (Loki for
	// on-prem hosts, CloudWatch for AWS, Cloud Logging for GCP) and to
	// the escalation target that can actually see it (e.g. AWS DevOps
	// Agent only for AWS alerts — it has no visibility into on-prem
	// hosts). All three are optional and only take effect together:
	// with HybridRoutes unset, LogSource/Cloud above behave exactly as
	// before. Setting HybridRoutes alongside the legacy LogSource or
	// Cloud block is rejected by Validate rather than silently picking
	// one — see HybridRouteConfig.
	LogSources        map[string]*LogSourceConfig `yaml:"log_sources"`
	EscalationTargets map[string]*CloudConfig     `yaml:"escalation_targets"`
	HybridRoutes      []HybridRouteConfig         `yaml:"hybrid_routes"`

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
	// Loki optionally points a type-"loki" entry at its own Loki
	// instance. Only meaningful inside log_sources (a hybrid deployment
	// may have more than one Loki, e.g. one per site); unset falls back
	// to the top-level loki.endpoint. Ignored by the other types, and
	// rejected by Validate on the legacy top-level log_source block (that
	// path always uses loki.endpoint).
	Loki *LokiEndpointConfig `yaml:"loki"`
}

// LokiEndpointConfig is the per-source override for a type-"loki" entry
// in log_sources. Only the endpoint is per-source — loki.lookback_sec and
// loki.limit stay global knobs, same as they already are for CloudWatch
// and GCP.
type LokiEndpointConfig struct {
	Endpoint string `yaml:"endpoint"`
}

// HybridRouteConfig is one hybrid routing rule: the first route whose
// matchers all match the alert's labels decides which named log source
// the alert's logs are fetched from and which named escalation target it
// escalates to. Same evaluation model as notifications.routes: config
// order, glob matchers (pkg/maintenance.MatchLabels), and a `default:
// true` route that matches everything and must be last.
type HybridRouteConfig struct {
	Matchers  map[string]string `yaml:"matchers"`
	LogSource string            `yaml:"log_source"` // name of an entry in log_sources; required
	// Escalation names one entry in escalation_targets, or several in
	// order (`escalation: aws` or `escalation: [aws, default]`): later
	// entries are only tried when an earlier one fails. Empty means alerts
	// on this route never escalate — the local result is final, as if no
	// cloud were configured at all for them.
	Escalation EscalationList `yaml:"escalation"`
	Default    bool           `yaml:"default"`
}

// EscalationList is a route's ordered escalation target names. It
// unmarshals from either a single YAML string or a sequence of strings,
// so configs written before fallbacks existed (`escalation: aws`) keep
// parsing unchanged.
type EscalationList []string

// UnmarshalYAML accepts a scalar (one name; "" means none) or a sequence.
func (l *EscalationList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		if s == "" {
			*l = nil
		} else {
			*l = EscalationList{s}
		}
		return nil
	case yaml.SequenceNode:
		var ss []string
		if err := n.Decode(&ss); err != nil {
			return err
		}
		*l = ss
		return nil
	default:
		return fmt.Errorf("line %d: escalation must be a target name or a list of target names", n.Line)
	}
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

	// Fallbacks are other OpenAI-compatible endpoints tried in order when
	// this one can't be reached (connection error, timeout, non-2xx). A
	// reply that arrives but isn't valid JSON is not a failure — it's
	// shown as-is, the same as without fallbacks. Only meaningful on the
	// top-level summarizer block; a fallback may not have fallbacks of its
	// own.
	Fallbacks []LLMConfig `yaml:"fallbacks"`

	// ProbeTimeoutSec bounds the GET /v1/models health check sent to this
	// backend before every chat call. A host that is asleep or wedged
	// accepts the TCP connection but never answers, so without the check
	// each alert waits out the whole TimeoutSec before moving on to a
	// fallback or the cloud; with it, the backend is skipped after this
	// many seconds. Unset means DefaultSummarizerProbeTimeoutSec, 0 turns
	// the check off. On a fallbacks entry, unset inherits the top-level
	// summarizer's value. A pointer so an explicit 0 is distinguishable
	// from "not written".
	ProbeTimeoutSec *int `yaml:"probe_timeout_sec,omitempty"`

	// BreakerFailures and BreakerCooldownSec set this backend's circuit
	// breaker: after BreakerFailures consecutive unavailable failures
	// (connect error, timeout, 5xx, 429, failed health check) the backend
	// is skipped outright — no health check, no chat — for
	// BreakerCooldownSec seconds, then one alert is let through to test
	// it. Unset means DefaultSummarizerBreakerFailures /
	// DefaultSummarizerBreakerCooldownSec; 0 in either turns the breaker
	// off. A fallbacks entry inherits the top-level values unless it sets
	// its own.
	BreakerFailures    *int `yaml:"breaker_failures,omitempty"`
	BreakerCooldownSec *int `yaml:"breaker_cooldown_sec,omitempty"`
}

// DefaultSummarizerProbeTimeoutSec is summarizer.probe_timeout_sec's
// default. /v1/models is a static list on every local server this has
// been pointed at (LM Studio, Ollama, the MLX shim) and answers in
// milliseconds even while a generation is running, so five seconds only
// ever trips on a host that isn't really there.
const DefaultSummarizerProbeTimeoutSec = 5

// DefaultSummarizerBreakerFailures and DefaultSummarizerBreakerCooldownSec
// are the circuit breaker defaults: two failures in a row is past a
// one-off blip, and two minutes spaces the retries out without leaving a
// backend that came back idle for long.
const (
	DefaultSummarizerBreakerFailures    = 2
	DefaultSummarizerBreakerCooldownSec = 120
)

// SummarizerHealth is one summarizer backend's resolved health-check
// settings, with defaults and fallback inheritance already applied.
type SummarizerHealth struct {
	ProbeTimeoutSec    int
	BreakerFailures    int
	BreakerCooldownSec int
}

// DefaultSummarizerHealth is what a summarizer block with none of the
// health-check fields written resolves to.
var DefaultSummarizerHealth = SummarizerHealth{
	ProbeTimeoutSec:    DefaultSummarizerProbeTimeoutSec,
	BreakerFailures:    DefaultSummarizerBreakerFailures,
	BreakerCooldownSec: DefaultSummarizerBreakerCooldownSec,
}

// Health resolves c's health-check settings: every field c leaves unset
// takes inherit's value. Pass DefaultSummarizerHealth for the top-level
// summarizer block and the top-level block's own resolved Health for
// each of its fallbacks.
func (c LLMConfig) Health(inherit SummarizerHealth) SummarizerHealth {
	h := inherit
	if c.ProbeTimeoutSec != nil {
		h.ProbeTimeoutSec = *c.ProbeTimeoutSec
	}
	if c.BreakerFailures != nil {
		h.BreakerFailures = *c.BreakerFailures
	}
	if c.BreakerCooldownSec != nil {
		h.BreakerCooldownSec = *c.BreakerCooldownSec
	}
	return h
}

// validateHealth rejects negative health-check settings on one
// summarizer block (label is its config path).
func (c LLMConfig) validateHealth(label string) error {
	if c.ProbeTimeoutSec != nil && *c.ProbeTimeoutSec < 0 {
		return fmt.Errorf("%s.probe_timeout_sec must be >= 0 (0 turns the health check off)", label)
	}
	if c.BreakerFailures != nil && *c.BreakerFailures < 0 {
		return fmt.Errorf("%s.breaker_failures must be >= 0 (0 turns the circuit breaker off)", label)
	}
	if c.BreakerCooldownSec != nil && *c.BreakerCooldownSec < 0 {
		return fmt.Errorf("%s.breaker_cooldown_sec must be >= 0 (0 turns the circuit breaker off)", label)
	}
	return nil
}

// CloudConfig is the cloud model endpoint escalated alerts get
// re-analyzed against. Only used when at least one Escalation rule can
// trigger, or the local model's own structured reply asks for escalation
// — see pkg/aiops.ShouldEscalate.
type CloudConfig struct {
	Provider string `yaml:"provider"` // "gemini" (default), "anthropic", "bedrock", "azure-openai", "aws-devops-agent", "openai-compatible", "vertex-ai", or "gcp-cloud-assist"
	Endpoint string `yaml:"endpoint"` // optional; each provider has its own default. Ignored by "bedrock" (region-based, see Region) and by "azure-openai"/"openai-compatible" (required there instead, as the server's base URL). For "vertex-ai" it overrides the host derived from Location
	APIKey   string `yaml:"api_key"`  // ignored by "bedrock", which uses the AWS SDK's own credential chain instead — see model.BedrockClient. Optional for "openai-compatible" (no Authorization header when empty). Must be empty for "vertex-ai" and "gcp-cloud-assist", which authenticate with Application Default Credentials
	Model    string `yaml:"model"`    // e.g. "gemini-2.5-flash", "claude-haiku-4-5", or a Bedrock model ID. Ignored by "azure-openai" — see Deployment

	// TimeoutSec bounds one request to an "openai-compatible" or
	// "vertex-ai" endpoint (0 means 60s), or one HTTP call of a
	// "gcp-cloud-assist" investigation (0 means 30s; the investigation as
	// a whole is bounded by PollTimeoutSec). The older providers keep
	// their own fixed timeouts and ignore this field, so adding it
	// changed nothing for them.
	TimeoutSec int `yaml:"timeout_sec"`

	// Project is the GCP project ID "vertex-ai" bills and authorizes the
	// call against, and the project a "gcp-cloud-assist" investigation
	// runs in and looks at. Ignored by every other provider.
	Project string `yaml:"project"`

	// Location is where "vertex-ai" sends the request: "global" (the
	// default), a region such as "us-central1", or the "us"/"eu"
	// multi-region — see model.VertexAIClient for the endpoint each one
	// maps to and the data-residency trade-off of "global". The Cloud
	// Assist investigations API only serves "global", so
	// "gcp-cloud-assist" accepts nothing else. Ignored by every other
	// provider.
	Location string `yaml:"location"`

	// PollIntervalSec and PollTimeoutSec pace a "gcp-cloud-assist"
	// investigation: how often its run is polled (0 means 15s) and how
	// long the gateway waits for it, counted from the start of the
	// escalation, before giving up and moving on to the next escalation
	// target (0 means 270s, under the default shutdown_grace_sec). The
	// investigation itself keeps running on GCP after a timeout. Ignored
	// by every other provider.
	PollIntervalSec int `yaml:"poll_interval_sec"`
	PollTimeoutSec  int `yaml:"poll_timeout_sec"`

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

// AlertmanagerConfig points at an Alertmanager instance's HTTP API.
// Nothing about the core webhook→Loki→LLM→notify path reads this — it
// exists only for `victoria-gateway suppression-candidates
// --apply-silences`, which is the one command in this repo that writes
// back to Alertmanager at all (as a time-bounded silence via the v2
// silence API, never a config file edit — see pkg/alertmanager's package
// doc).
type AlertmanagerConfig struct {
	Endpoint string `yaml:"endpoint"` // e.g. "http://192.0.2.6:9093"
	// Username/Password are optional HTTP Basic Auth, for an Alertmanager
	// sitting behind one (e.g. a reverse proxy) — most homelab/single-node
	// setups leave these unset.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
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

	// PublicBaseURL, e.g. "http://192.0.2.6:8090", is what /incidents
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

	// AuditLog, when true, records who replaced the maintenance window
	// set, confirmed a pending incident, or applied a suppression
	// candidate as a real Alertmanager silence — see pkg/audit. Off by
	// default: it reuses this same Postgres connection (no new
	// dependency), but still adds a write on every such operation, and a
	// single-operator homelab deployment may not need "who did this"
	// answered by anything more than "well, me." Requires Enabled: true
	// (there is no separate audit-only database).
	AuditLog bool `yaml:"audit_log"`

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
	Endpoint string `yaml:"endpoint"` // e.g. "https://gitea.example.com"
	Token    string `yaml:"token"`
	Owner    string `yaml:"owner"` // repo owner, e.g. "ops-team"
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
// exist in the first place. The one exception is the cloud API keys (see
// ValidateForServe): they usually arrive via env vars that only the
// server's unit sets, and note/sync never call a cloud model.
func (c *Config) Validate() error {
	if len(c.HybridRoutes) > 0 {
		if err := c.validateHybrid(); err != nil {
			return err
		}
	} else {
		if len(c.LogSources) > 0 || len(c.EscalationTargets) > 0 {
			return fmt.Errorf("log_sources/escalation_targets are set but hybrid_routes is empty — they only take effect through hybrid_routes, so either add routes or remove them")
		}
		if err := c.validateLogSource(); err != nil {
			return err
		}
	}
	if c.Summarizer.Endpoint == "" {
		return fmt.Errorf("summarizer.endpoint is not set in config.yaml")
	}
	if c.Summarizer.TimeoutSec < 0 {
		return fmt.Errorf("summarizer.timeout_sec must be >= 0 (0 means the 60s default)")
	}
	if err := c.Summarizer.validateHealth("summarizer"); err != nil {
		return err
	}
	for i, fb := range c.Summarizer.Fallbacks {
		label := fmt.Sprintf("summarizer.fallbacks[%d]", i)
		if fb.Endpoint == "" {
			return fmt.Errorf("%s.endpoint is not set", label)
		}
		if fb.TimeoutSec < 0 {
			return fmt.Errorf("%s.timeout_sec must be >= 0 (0 means the 60s default)", label)
		}
		if err := fb.validateHealth(label); err != nil {
			return err
		}
		if len(fb.Fallbacks) > 0 {
			return fmt.Errorf("%s has fallbacks of its own — list every fallback directly under summarizer.fallbacks instead", label)
		}
	}
	if c.Cloud != nil {
		if err := validateCloudCommon("cloud", c.Cloud); err != nil {
			return err
		}
	}
	if len(c.CloudFallbacks) > 0 {
		if len(c.HybridRoutes) > 0 {
			return fmt.Errorf("cloud_fallbacks is set together with hybrid_routes — with hybrid_routes, list fallback targets in each route's escalation instead (e.g. escalation: [aws, default])")
		}
		if c.Cloud == nil {
			return fmt.Errorf("cloud_fallbacks is set but cloud is not — fallbacks only run after the cloud block's own escalation fails")
		}
		for i, fb := range c.CloudFallbacks {
			label := fmt.Sprintf("cloud_fallbacks[%d]", i)
			if fb == nil {
				return fmt.Errorf("%s is empty", label)
			}
			if err := validateCloudEntry(label, fb); err != nil {
				return err
			}
		}
	}
	// An escalation rule that can never fire (no Cloud configured) is a
	// silent no-op the operator almost certainly didn't intend — fail
	// loudly rather than have alerts quietly never escalate.
	if len(c.Escalation.AlwaysCloud) > 0 && c.Cloud == nil && len(c.EscalationTargets) == 0 {
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
	} else if c.RAG != nil && c.RAG.AuditLog {
		return fmt.Errorf("rag.audit_log is true but rag.enabled is not — audit logging reuses the RAG Postgres connection, so RAG must be enabled too")
	}
	if c.ShutdownGraceSec < 0 {
		return fmt.Errorf("shutdown_grace_sec must be >= 0 (0 means the 300s default)")
	}
	if c.Judge != nil && (c.Judge.EscalateThreshold < 0 || c.Judge.EscalateThreshold > 1) {
		return fmt.Errorf("judge.escalate_threshold must be between 0 and 1 (0 means the 0.70 default)")
	}
	if c.Alertmanager != nil && c.Alertmanager.Endpoint == "" {
		return fmt.Errorf("alertmanager is set but alertmanager.endpoint is empty")
	}
	if err := c.validateNotifications(); err != nil {
		return err
	}
	if err := ValidateMaintenanceWindows(c.MaintenanceWindows); err != nil {
		return err
	}
	return nil
}

// ValidateForServe is Validate plus the checks only the server needs:
// every gemini/anthropic escalation target (cloud, cloud_fallbacks,
// escalation_targets) must have an api_key, and an ADC-authenticated one
// (vertex-ai, gcp-cloud-assist) must not have one. note and sync call plain
// Validate, so running them from a shell without the
// VICTORIA_GATEWAY_*_API_KEY env vars doesn't fail on a key they never
// use.
func (c *Config) ValidateForServe() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Cloud != nil {
		if err := validateCloudAPIKey("cloud", c.Cloud); err != nil {
			return err
		}
	}
	for i, fb := range c.CloudFallbacks {
		if err := validateCloudAPIKey(fmt.Sprintf("cloud_fallbacks[%d]", i), fb); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(c.EscalationTargets))
	for name := range c.EscalationTargets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateCloudAPIKey("escalation_targets."+name, c.EscalationTargets[name]); err != nil {
			return err
		}
	}
	return nil
}

// validateLogSource checks Config.Loki/Config.LogSource together: exactly
// the block matching whichever backend is (implicitly or explicitly)
// selected must be filled in, so a typo'd or half-filled config fails at
// startup instead of surfacing as "logs are always empty" on the first
// alert.
func (c *Config) validateLogSource() error {
	// The per-source Loki endpoint is a log_sources feature. On the
	// legacy block it used to be silently ignored, leaving an operator
	// believing logs came from somewhere they didn't — refuse it instead.
	if c.LogSource != nil && c.LogSource.Loki != nil && c.LogSource.Loki.Endpoint != "" {
		return fmt.Errorf("log_source.loki.endpoint (%s) is only supported under log_sources with hybrid_routes — without hybrid_routes, set the top-level loki.endpoint instead", c.LogSource.Loki.Endpoint)
	}
	if (c.LogSource == nil || c.LogSource.Type == "" || c.LogSource.Type == "loki") && c.Loki.Endpoint == "" {
		return fmt.Errorf("loki.endpoint is not set in config.yaml")
	}
	return validateLogSourceEntry("log_source", c.LogSource, c.Loki.Endpoint)
}

// validateLogSourceEntry checks one log source block — the legacy
// top-level log_source (label "log_source") or one named entry of
// log_sources (label "log_sources.<name>") — against the same rules, so
// the two paths can't drift apart. defaultLokiEndpoint is what a
// type-"loki" entry falls back to when it has no loki.endpoint of its
// own; ls may be nil, which means type "loki".
func validateLogSourceEntry(label string, ls *LogSourceConfig, defaultLokiEndpoint string) error {
	logSourceType := "loki"
	if ls != nil && ls.Type != "" {
		logSourceType = ls.Type
	}

	switch logSourceType {
	case "loki":
		if (ls == nil || ls.Loki == nil || ls.Loki.Endpoint == "") && defaultLokiEndpoint == "" {
			return fmt.Errorf("%s is type \"loki\" but neither %s.loki.endpoint nor the top-level loki.endpoint is set in config.yaml", label, label)
		}
	case "cloudwatch":
		cw := ls.CloudWatch
		if cw == nil || cw.Region == "" || len(cw.LogGroupNames) == 0 {
			return fmt.Errorf("%s.type is \"cloudwatch\" but %s.cloudwatch.region/log_group_names is missing in config.yaml", label, label)
		}
	case "gcp_logging":
		gl := ls.GCPLogging
		if gl == nil || gl.ProjectID == "" {
			return fmt.Errorf("%s.type is \"gcp_logging\" but %s.gcp_logging.project_id is missing in config.yaml", label, label)
		}
	default:
		return fmt.Errorf("%s.type is %q, want \"loki\", \"cloudwatch\", or \"gcp_logging\"", label, logSourceType)
	}
	return nil
}

// validateHybrid checks log_sources/escalation_targets/hybrid_routes as
// one unit. Mixing hybrid_routes with the legacy single log_source or
// cloud block is rejected outright: silently preferring one would leave
// an operator believing the other is in effect, and "which one wins"
// isn't something anyone should have to remember.
func (c *Config) validateHybrid() error {
	if c.LogSource != nil {
		return fmt.Errorf("hybrid_routes and log_source are both set — with hybrid_routes, define every log backend under log_sources and remove the log_source block")
	}
	if c.Cloud != nil {
		return fmt.Errorf("hybrid_routes and cloud are both set — with hybrid_routes, define every escalation model under escalation_targets and remove the cloud block")
	}
	if len(c.LogSources) == 0 {
		return fmt.Errorf("hybrid_routes is set but log_sources is empty")
	}
	for name, ls := range c.LogSources {
		if name == "" {
			return fmt.Errorf("log_sources has an entry with an empty name")
		}
		if err := validateLogSourceEntry("log_sources."+name, ls, c.Loki.Endpoint); err != nil {
			return err
		}
	}
	envNames := map[string]string{}
	for name, t := range c.EscalationTargets {
		if t == nil {
			return fmt.Errorf("escalation_targets.%s is empty", name)
		}
		if err := validateCloudEntry("escalation_targets."+name, t); err != nil {
			return err
		}
		// Two names that differ only in case or punctuation ("aws-prod"
		// vs "aws_prod") would read their API key from the same env var,
		// and which one got it would depend on map iteration order.
		env := EnvName(name)
		if other, ok := envNames[env]; ok {
			a, b := other, name
			if b < a {
				a, b = b, a
			}
			return fmt.Errorf("escalation_targets %q and %q both map to the env var VICTORIA_GATEWAY_ESCALATION_%s_API_KEY — rename one so they differ in more than case or punctuation", a, b, env)
		}
		envNames[env] = name
	}
	usedSources := map[string]bool{}
	usedTargets := map[string]bool{}
	for i, r := range c.HybridRoutes {
		label := fmt.Sprintf("hybrid_routes[%d]", i)
		if r.Default {
			if i != len(c.HybridRoutes)-1 {
				return fmt.Errorf("%s: the default route must be the last route (routes are evaluated in order, so nothing after it could ever match)", label)
			}
			if len(r.Matchers) > 0 {
				return fmt.Errorf("%s: a default route must not have matchers", label)
			}
		} else if len(r.Matchers) == 0 {
			return fmt.Errorf("%s: matchers must not be empty (use default: true for the catch-all route)", label)
		}
		if err := glob.ValidateMatchers(r.Matchers); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if r.LogSource == "" {
			return fmt.Errorf("%s: log_source is required", label)
		}
		if _, ok := c.LogSources[r.LogSource]; !ok {
			return fmt.Errorf("%s: log_source %q is not defined under log_sources", label, r.LogSource)
		}
		usedSources[r.LogSource] = true
		seen := map[string]bool{}
		for _, esc := range r.Escalation {
			if esc == "" {
				return fmt.Errorf("%s: escalation has an empty target name", label)
			}
			if seen[esc] {
				return fmt.Errorf("%s: escalation lists %q more than once", label, esc)
			}
			seen[esc] = true
			if _, ok := c.EscalationTargets[esc]; !ok {
				return fmt.Errorf("%s: escalation %q is not defined under escalation_targets", label, esc)
			}
			usedTargets[esc] = true
		}
	}
	if !c.HybridRoutes[len(c.HybridRoutes)-1].Default {
		return fmt.Errorf("hybrid_routes has no default route — add a last route with default: true so every alert has a log source")
	}
	// Defined-but-unreferenced entries are almost always a typo'd route
	// name (the route then silently falls through to default), so they
	// fail loudly the same way an undefined reference does.
	for name := range c.LogSources {
		if !usedSources[name] {
			return fmt.Errorf("log_sources.%s is defined but no hybrid_routes entry uses it", name)
		}
	}
	for name := range c.EscalationTargets {
		if !usedTargets[name] {
			return fmt.Errorf("escalation_targets.%s is defined but no hybrid_routes entry uses it", name)
		}
	}
	return nil
}

// validateCloudEntry checks the per-provider required fields of one
// escalation target. The legacy top-level cloud block has always had
// these checked at client construction time in runServe instead; hybrid
// targets are checked here so a half-filled target fails at config load
// rather than only when a route first escalates to it.
func validateCloudEntry(label string, c *CloudConfig) error {
	switch c.Provider {
	case "", "gemini", "anthropic":
		// api_key is checked by ValidateForServe only — see there.
	case "bedrock":
		if c.Region == "" || c.Model == "" {
			return fmt.Errorf("%s.provider is \"bedrock\" but %s.region/model is missing", label, label)
		}
	case "azure-openai":
		if c.Endpoint == "" || c.Deployment == "" {
			return fmt.Errorf("%s.provider is \"azure-openai\" but %s.endpoint/deployment is missing", label, label)
		}
	case "aws-devops-agent":
		if c.DevOpsAgent == nil {
			return fmt.Errorf("%s.provider is \"aws-devops-agent\" but %s.aws_devops_agent is not set", label, label)
		}
	case "openai-compatible":
		if c.Endpoint == "" || c.Model == "" {
			return fmt.Errorf("%s.provider is \"openai-compatible\" but %s.endpoint/model is missing", label, label)
		}
	case "vertex-ai":
		if c.Project == "" || c.Model == "" {
			return fmt.Errorf("%s.provider is \"vertex-ai\" but %s.project/model is missing", label, label)
		}
	case "gcp-cloud-assist":
		if c.Project == "" {
			return fmt.Errorf("%s.provider is \"gcp-cloud-assist\" but %s.project is missing", label, label)
		}
	default:
		return fmt.Errorf("%s.provider is %q, want %s", label, c.Provider, CloudProviderList)
	}
	return validateCloudCommon(label, c)
}

// CloudProviderList is every accepted provider value, as quoted in the
// "unknown provider" errors here and in cmd/victoria-gateway's
// buildCloud. A new provider is added here once.
const CloudProviderList = `"gemini", "anthropic", "bedrock", "azure-openai", "aws-devops-agent", "openai-compatible", "vertex-ai", or "gcp-cloud-assist"`

// gcpProjectPattern and gcpLocationPattern are deliberately loose — they
// don't try to be GCP's exact naming rules, only to stop a value that
// would change the request URL's host or path (a "/", "?", "@", space).
// vertexModelPattern does the same for a "vertex-ai" model, which is a
// path segment too: letters, digits, ".", "_", "@" (a pinned version,
// e.g. "claude-opus-4@20250514") and "-".
var (
	gcpProjectPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.:-]*$`)
	gcpLocationPattern = regexp.MustCompile(`^[a-z0-9-]+$`)
	vertexModelPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)
)

// validateCloudCommon checks the fields added alongside the
// openai-compatible/vertex-ai/gcp-cloud-assist providers: timeouts that
// can't be negative, and a GCP project/location (and vertex-ai model)
// that can't reshape the request URL. Unlike
// the per-provider required fields (checked for the legacy cloud block
// in buildCloud), this runs for every block including the legacy one:
// the fields are new, so there is no pre-existing error text to preserve.
func validateCloudCommon(label string, c *CloudConfig) error {
	if c.TimeoutSec < 0 {
		return fmt.Errorf("%s.timeout_sec must be >= 0 (0 means the provider's default)", label)
	}
	if c.PollIntervalSec < 0 || c.PollTimeoutSec < 0 {
		return fmt.Errorf("%s.poll_interval_sec/poll_timeout_sec must be >= 0 (0 means the 15s/270s default)", label)
	}
	if c.Provider == "gcp-cloud-assist" && c.Location != "" && c.Location != "global" {
		return fmt.Errorf("%s.location is %q, but the Gemini Cloud Assist investigations API only serves \"global\" — remove location or set it to global", label, c.Location)
	}
	if c.Project != "" && !gcpProjectPattern.MatchString(c.Project) {
		return fmt.Errorf("%s.project %q is not a valid GCP project ID", label, c.Project)
	}
	if c.Location != "" && !gcpLocationPattern.MatchString(c.Location) {
		return fmt.Errorf("%s.location %q is not a valid location (want e.g. \"global\", \"us-central1\", \"us\")", label, c.Location)
	}
	if c.Provider == "vertex-ai" && c.Model != "" && !vertexModelPattern.MatchString(c.Model) {
		return fmt.Errorf("%s.model %q is not a valid Vertex AI model ID (letters, digits, \".\", \"_\", \"@\" and \"-\" only, e.g. \"gemini-2.5-flash\")", label, c.Model)
	}
	return nil
}

// validateCloudAPIKey rejects a gemini/anthropic block with no api_key:
// both providers answer every call with 401 without one, so the gap would
// otherwise only surface as "every escalation fails" on the first alert.
// The legacy cloud block only gets this check (its other per-provider
// checks stay in buildCloud, unchanged); api_key may also arrive via the
// VICTORIA_GATEWAY_*_API_KEY env vars, which Load applies before
// validation. Only ValidateForServe calls this.
//
// The opposite also lives here: "vertex-ai" and "gcp-cloud-assist"
// authenticate with Application Default Credentials and never send an
// api_key, so one set
// on such a block — typically a VICTORIA_GATEWAY_*_API_KEY left over from
// when the block was provider: gemini — is rejected rather than silently
// ignored, since an operator who sees it set would reasonably assume it's
// in use.
func validateCloudAPIKey(label string, c *CloudConfig) error {
	switch c.Provider {
	case "", "gemini", "anthropic":
		if c.APIKey == "" {
			p := c.Provider
			if p == "" {
				p = "gemini"
			}
			return fmt.Errorf("%s.provider is %q but %s.api_key is empty", label, p, label)
		}
	case "vertex-ai", "gcp-cloud-assist":
		if c.APIKey != "" {
			return fmt.Errorf("%s.provider is %q but %s.api_key is set — this provider authenticates with Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS or the metadata server), not an API key; remove api_key (and any VICTORIA_GATEWAY_*_API_KEY env var for this block)", label, c.Provider, label)
		}
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
		if err := glob.ValidateMatchers(mw.Matchers); err != nil {
			return fmt.Errorf("%s: %w", label, err)
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
		if err := glob.ValidateMatchers(rt.Matchers); err != nil {
			return fmt.Errorf("%s: %w", label, err)
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
	applyEnvOverrides(cfg)
	return cfg, nil
}

// applyEnvOverrides lets a handful of credential fields be supplied via
// environment variables, overriding whatever config.yaml set (or filling
// them in when config.yaml left the surrounding block configured but the
// secret itself blank). This is deliberately narrow — not a general
// env-var-for-every-field mechanism — because it only targets the one gap
// running multiple config.yaml files per environment doesn't already
// close: keeping credentials out of a file at all, for setups where
// config.yaml is templated by CI or mounted read-only and a secret has to
// come from a container orchestrator's own secret store (a K8s Secret
// projected as an env var, Docker Compose's env_file, etc.) instead.
//
// Every var is namespaced VICTORIA_GATEWAY_*, applies only when set to a
// non-empty value (an unset var never blanks out something config.yaml
// already set), and only overrides a field inside a block config.yaml
// already configured (e.g. VICTORIA_GATEWAY_JUDGE_API_KEY does nothing if
// there's no `judge:` block at all) — auto-creating a whole feature's
// config block from one env var would mean an operator setting a
// credential for later use could accidentally turn that feature on, which
// is a bigger behavior change than "override a value" should cause.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("VICTORIA_GATEWAY_SUMMARIZER_API_KEY"); v != "" {
		cfg.Summarizer.APIKey = v
	}
	if v := os.Getenv("VICTORIA_GATEWAY_TELEGRAM_BOT_TOKEN"); v != "" {
		cfg.Telegram.BotToken = v
	}
	if cfg.Cloud != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_CLOUD_API_KEY"); v != "" {
			cfg.Cloud.APIKey = v
		}
	}
	// Fallbacks are addressed by position, e.g. summarizer.fallbacks[0] →
	// VICTORIA_GATEWAY_SUMMARIZER_FALLBACK_0_API_KEY and cloud_fallbacks[1]
	// → VICTORIA_GATEWAY_CLOUD_FALLBACK_1_API_KEY.
	for i := range cfg.Summarizer.Fallbacks {
		if v := os.Getenv(fmt.Sprintf("VICTORIA_GATEWAY_SUMMARIZER_FALLBACK_%d_API_KEY", i)); v != "" {
			cfg.Summarizer.Fallbacks[i].APIKey = v
		}
	}
	for i, fb := range cfg.CloudFallbacks {
		if fb == nil {
			continue
		}
		if v := os.Getenv(fmt.Sprintf("VICTORIA_GATEWAY_CLOUD_FALLBACK_%d_API_KEY", i)); v != "" {
			fb.APIKey = v
		}
	}
	// One var per named escalation target, e.g. escalation_targets.default
	// → VICTORIA_GATEWAY_ESCALATION_DEFAULT_API_KEY. Non-alphanumeric
	// characters in the name become "_" so a name like "aws-prod" still
	// maps to a valid env var name (…_AWS_PROD_API_KEY).
	for name, t := range cfg.EscalationTargets {
		if t == nil {
			continue
		}
		if v := os.Getenv("VICTORIA_GATEWAY_ESCALATION_" + EnvName(name) + "_API_KEY"); v != "" {
			t.APIKey = v
		}
	}
	if cfg.Judge != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_JUDGE_API_KEY"); v != "" {
			cfg.Judge.APIKey = v
		}
	}
	if cfg.Alertmanager != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_ALERTMANAGER_PASSWORD"); v != "" {
			cfg.Alertmanager.Password = v
		}
	}
	if cfg.WebhookAuth != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_WEBHOOK_AUTH_PASSWORD"); v != "" {
			cfg.WebhookAuth.Password = v
		}
	}
	if cfg.WebUIAuth != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_WEBUI_AUTH_PASSWORD"); v != "" {
			cfg.WebUIAuth.Password = v
		}
	}
	if cfg.RAG != nil {
		if v := os.Getenv("VICTORIA_GATEWAY_RAG_POSTGRES_DSN"); v != "" {
			cfg.RAG.PostgresDSN = v
		}
		if cfg.RAG.Gitea != nil {
			if v := os.Getenv("VICTORIA_GATEWAY_GITEA_TOKEN"); v != "" {
				cfg.RAG.Gitea.Token = v
			}
		}
		if cfg.RAG.GitHub != nil {
			if v := os.Getenv("VICTORIA_GATEWAY_GITHUB_TOKEN"); v != "" {
				cfg.RAG.GitHub.Token = v
			}
		}
	}
}

// EnvName upper-cases a config entry name and replaces every character
// that isn't a letter or digit with "_", producing the name segment used
// in per-entry VICTORIA_GATEWAY_* variables.
func EnvName(name string) string {
	b := []byte(strings.ToUpper(name))
	for i, ch := range b {
		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			b[i] = '_'
		}
	}
	return string(b)
}
