package config

import (
	"os"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Loki:       LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
	}
}

func TestValidate_MinimalValid(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a minimal valid config", err)
	}
}

func TestValidate_MissingLokiEndpoint(t *testing.T) {
	c := validConfig()
	c.Loki.Endpoint = ""
	if err := c.Validate(); err == nil {
		t.Error("expected an error when loki.endpoint is empty")
	}
}

func TestValidate_LogSourceNil_LokiEndpointStillRequired(t *testing.T) {
	c := validConfig()
	c.Loki.Endpoint = ""
	c.LogSource = nil
	if err := c.Validate(); err == nil {
		t.Error("expected an error when log_source is nil (defaults to loki) and loki.endpoint is empty")
	}
}

func TestValidate_LogSourceExplicitLoki_LokiEndpointRequired(t *testing.T) {
	c := validConfig()
	c.Loki.Endpoint = ""
	c.LogSource = &LogSourceConfig{Type: "loki"}
	if err := c.Validate(); err == nil {
		t.Error("expected an error when log_source.type is explicitly \"loki\" and loki.endpoint is empty")
	}
}

func TestValidate_LogSourceCloudWatch_LokiEndpointNotRequired(t *testing.T) {
	c := validConfig()
	c.Loki.Endpoint = "" // cloudwatch doesn't need it
	c.LogSource = &LogSourceConfig{
		Type: "cloudwatch",
		CloudWatch: &CloudWatchConfig{
			Region:        "us-east-1",
			LogGroupNames: []string{"/aws/lambda/my-fn"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil when cloudwatch is fully configured", err)
	}
}

func TestValidate_LogSourceCloudWatch_MissingFields(t *testing.T) {
	c := validConfig()
	c.LogSource = &LogSourceConfig{Type: "cloudwatch"} // no CloudWatch block at all
	if err := c.Validate(); err == nil {
		t.Error("expected an error when log_source.type is cloudwatch but log_source.cloudwatch is missing")
	}

	c.LogSource.CloudWatch = &CloudWatchConfig{Region: "us-east-1"} // no log group names
	if err := c.Validate(); err == nil {
		t.Error("expected an error when cloudwatch.log_group_names is empty")
	}
}

func TestValidate_LogSourceGCPLogging_LokiEndpointNotRequired(t *testing.T) {
	c := validConfig()
	c.Loki.Endpoint = ""
	c.LogSource = &LogSourceConfig{
		Type:       "gcp_logging",
		GCPLogging: &GCPLoggingConfig{ProjectID: "my-project"},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil when gcp_logging is fully configured", err)
	}
}

func TestValidate_LogSourceGCPLogging_MissingProjectID(t *testing.T) {
	c := validConfig()
	c.LogSource = &LogSourceConfig{Type: "gcp_logging", GCPLogging: &GCPLoggingConfig{}}
	if err := c.Validate(); err == nil {
		t.Error("expected an error when gcp_logging.project_id is empty")
	}
}

func TestValidate_LogSourceUnknownType(t *testing.T) {
	c := validConfig()
	c.LogSource = &LogSourceConfig{Type: "splunk"}
	if err := c.Validate(); err == nil {
		t.Error("expected an error for an unrecognized log_source.type")
	}
}

func TestValidate_MissingSummarizerEndpoint(t *testing.T) {
	c := validConfig()
	c.Summarizer.Endpoint = ""
	if err := c.Validate(); err == nil {
		t.Error("expected an error when summarizer.endpoint is empty")
	}
}

func TestValidate_AlwaysCloudWithoutCloud(t *testing.T) {
	c := validConfig()
	c.Escalation.AlwaysCloud = []string{"SomeAlert"}
	if err := c.Validate(); err == nil {
		t.Error("expected an error when escalation.always_cloud is set but cloud is nil")
	}
}

func TestValidate_AlwaysCloudWithCloud_OK(t *testing.T) {
	c := validConfig()
	c.Escalation.AlwaysCloud = []string{"SomeAlert"}
	c.Cloud = &CloudConfig{APIKey: "k"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil when cloud is configured", err)
	}
}

func TestValidate_NegativeMaxPerHour(t *testing.T) {
	c := validConfig()
	c.Escalation.MaxPerHour = -1
	if err := c.Validate(); err == nil {
		t.Error("expected an error when escalation.max_per_hour is negative")
	}
}

func TestValidate_WebhookAuthEmptyUsername(t *testing.T) {
	c := validConfig()
	c.WebhookAuth = &WebhookAuthConfig{Username: "", Password: "secret"}
	if err := c.Validate(); err == nil {
		t.Error("expected an error when webhook_auth.username is empty")
	}
}

func TestValidate_WebhookAuthEmptyPassword(t *testing.T) {
	c := validConfig()
	c.WebhookAuth = &WebhookAuthConfig{Username: "user", Password: ""}
	if err := c.Validate(); err == nil {
		t.Error("expected an error when webhook_auth.password is empty")
	}
}

func TestValidate_WebhookAuthBothSet_OK(t *testing.T) {
	c := validConfig()
	c.WebhookAuth = &WebhookAuthConfig{Username: "user", Password: "secret"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil when both webhook_auth fields are set", err)
	}
}

func TestValidate_RAGEnabledMissingFields(t *testing.T) {
	c := validConfig()
	c.RAG = &RAGConfig{Enabled: true}
	if err := c.Validate(); err == nil {
		t.Error("expected an error when rag.enabled is true but postgres_dsn/embedding_endpoint/embedding_model are missing")
	}
}

func TestValidate_RAGEnabledComplete_OK(t *testing.T) {
	c := validConfig()
	c.RAG = &RAGConfig{
		Enabled:           true,
		PostgresDSN:       "postgres://u:p@h/db",
		EmbeddingEndpoint: "http://llm:1234",
		EmbeddingModel:    "bge-m3",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a fully-configured rag block", err)
	}
}

func TestValidate_RAGDisabled_FieldsNotRequired(t *testing.T) {
	c := validConfig()
	c.RAG = &RAGConfig{Enabled: false}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil when rag.enabled is false (fields shouldn't be required)", err)
	}
}

// --- notifications block validation ---

func validNotifications() *NotificationsConfig {
	return &NotificationsConfig{
		Channels: []NotifyChannelConfig{
			{Name: "ops", Type: "telegram", BotToken: "t", ChatID: 1},
			{Name: "itsm", Type: "webhook", URL: "http://itsm/api"},
		},
		Routes: []NotifyRouteConfig{
			{Matchers: map[string]string{"severity": "critical"}, Channels: []string{"ops", "itsm"}},
			{Default: true, Channels: []string{"ops"}},
		},
	}
}

func TestValidate_Notifications_OK(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestValidate_Notifications_NoRoutes(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Routes = nil
	if err := c.Validate(); err == nil {
		t.Error("expected error when notifications has channels but no routes")
	}
}

func TestValidate_Notifications_DuplicateChannelName(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Channels = append(c.Notifications.Channels, NotifyChannelConfig{Name: "ops", Type: "webhook", URL: "http://x"})
	if err := c.Validate(); err == nil {
		t.Error("expected error for duplicate channel name")
	}
}

func TestValidate_Notifications_TelegramMissingToken(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Channels[0].BotToken = ""
	if err := c.Validate(); err == nil {
		t.Error("expected error for telegram channel without bot_token")
	}
}

func TestValidate_Notifications_WebhookMissingURL(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Channels[1].URL = ""
	if err := c.Validate(); err == nil {
		t.Error("expected error for webhook channel without url")
	}
}

func TestValidate_Notifications_UnknownChannelType(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Channels[0].Type = "slack"
	if err := c.Validate(); err == nil {
		t.Error("expected error for unknown channel type")
	}
}

func TestValidate_Notifications_RouteUndefinedChannel(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Routes[0].Channels = []string{"nope"}
	if err := c.Validate(); err == nil {
		t.Error("expected error for route referencing undefined channel")
	}
}

func TestValidate_Notifications_NonDefaultRouteNeedsMatchers(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Routes[0].Matchers = nil
	if err := c.Validate(); err == nil {
		t.Error("expected error for non-default route without matchers")
	}
}

func TestValidate_Notifications_RouteAfterDefaultUnreachable(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Routes = []NotifyRouteConfig{
		{Default: true, Channels: []string{"ops"}},
		{Matchers: map[string]string{"severity": "critical"}, Channels: []string{"ops"}},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for a route after the default route")
	}
}

func TestValidate_Notifications_DefaultWithMatchers(t *testing.T) {
	c := validConfig()
	c.Notifications = validNotifications()
	c.Notifications.Routes[1].Matchers = map[string]string{"x": "y"}
	if err := c.Validate(); err == nil {
		t.Error("expected error for a default route that also has matchers")
	}
}

// --- rag similarity threshold / shutdown grace ---

func TestValidate_RAGSimilarityThresholdOutOfRange(t *testing.T) {
	c := validConfig()
	c.RAG = &RAGConfig{Enabled: true, PostgresDSN: "d", EmbeddingEndpoint: "e", EmbeddingModel: "m", SimilarityThreshold: 1.5}
	if err := c.Validate(); err == nil {
		t.Error("expected error for similarity_threshold > 1")
	}
}

func TestValidate_NegativeShutdownGrace(t *testing.T) {
	c := validConfig()
	c.ShutdownGraceSec = -1
	if err := c.Validate(); err == nil {
		t.Error("expected error for negative shutdown_grace_sec")
	}
}

// --- summarizer timeout_sec ---

func TestValidate_NegativeSummarizerTimeout(t *testing.T) {
	c := validConfig()
	c.Summarizer.TimeoutSec = -1
	if err := c.Validate(); err == nil {
		t.Error("expected error for negative summarizer.timeout_sec")
	}
}

func TestValidate_SummarizerTimeout_ZeroAndPositiveOK(t *testing.T) {
	c := validConfig()
	c.Summarizer.TimeoutSec = 0
	if err := c.Validate(); err != nil {
		t.Errorf("timeout_sec 0 should be valid (client default), got %v", err)
	}
	c.Summarizer.TimeoutSec = 180
	if err := c.Validate(); err != nil {
		t.Errorf("timeout_sec 180 should be valid, got %v", err)
	}
}

func TestValidate_JudgeNil_NoError(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil when judge is not configured", err)
	}
}

func TestValidate_JudgeThresholdOutOfRange(t *testing.T) {
	for _, threshold := range []float64{-0.1, 1.1} {
		c := validConfig()
		c.Judge = &JudgeConfig{APIKey: "k", EscalateThreshold: threshold}
		if err := c.Validate(); err == nil {
			t.Errorf("expected an error for judge.escalate_threshold = %v", threshold)
		}
	}
}

func TestValidate_JudgeThresholdInRange(t *testing.T) {
	for _, threshold := range []float64{0, 0.5, 0.70, 1.0} {
		c := validConfig()
		c.Judge = &JudgeConfig{APIKey: "k", EscalateThreshold: threshold}
		if err := c.Validate(); err != nil {
			t.Errorf("judge.escalate_threshold = %v should be valid, got %v", threshold, err)
		}
	}
}

func TestLoad_ParsesJudgeConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	yaml := "loki:\n  endpoint: \"http://loki:3100\"\nsummarizer:\n  endpoint: \"http://llm:1234\"\n  model: \"m\"\njudge:\n  api_key: \"sk-test\"\n  escalate_threshold: 0.8\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Judge == nil || cfg.Judge.APIKey != "sk-test" || cfg.Judge.EscalateThreshold != 0.8 {
		t.Errorf("Judge = %+v, want APIKey=sk-test EscalateThreshold=0.8", cfg.Judge)
	}
}

func TestLoad_ParsesSummarizerTimeoutSec(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	yaml := "loki:\n  endpoint: \"http://loki:3100\"\nsummarizer:\n  endpoint: \"http://llm:1234\"\n  model: \"m\"\n  timeout_sec: 180\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Summarizer.TimeoutSec != 180 {
		t.Errorf("timeout_sec = %d, want 180", cfg.Summarizer.TimeoutSec)
	}
}
