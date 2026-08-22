package config

import "testing"

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
