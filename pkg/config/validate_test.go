package config

import "testing"

func TestValidateHybrid_EnvNameCollision(t *testing.T) {
	c := validHybridConfig()
	c.EscalationTargets["aws-prod"] = &CloudConfig{Provider: "gemini", APIKey: "k"}
	c.EscalationTargets["aws_prod"] = &CloudConfig{Provider: "gemini", APIKey: "k"}
	c.HybridRoutes[0].Escalation = EscalationList{"aws", "aws-prod", "aws_prod"}
	wantErrContaining(t, c.Validate(), `"aws-prod" and "aws_prod" both map to the env var VICTORIA_GATEWAY_ESCALATION_AWS_PROD_API_KEY`)
}

func TestValidateForServe_GeminiAnthropicAPIKeyRequired(t *testing.T) {
	for _, p := range []string{"", "gemini", "anthropic"} {
		c := validConfig()
		c.Cloud = &CloudConfig{Provider: p}
		wantErrContaining(t, c.ValidateForServe(), "cloud.api_key is empty")

		h := validHybridConfig()
		h.EscalationTargets["default"] = &CloudConfig{Provider: p}
		wantErrContaining(t, h.ValidateForServe(), "escalation_targets.default.api_key is empty")

		f := validConfig()
		f.Cloud = &CloudConfig{Provider: "bedrock", Region: "us-east-1", Model: "m"}
		f.CloudFallbacks = []*CloudConfig{{Provider: p}}
		wantErrContaining(t, f.ValidateForServe(), "cloud_fallbacks[0].api_key is empty")
	}
	// bedrock uses the AWS credential chain, never an api_key.
	c := validConfig()
	c.Cloud = &CloudConfig{Provider: "bedrock", Region: "us-east-1", Model: "m"}
	if err := c.ValidateForServe(); err != nil {
		t.Fatalf("bedrock without api_key: %v", err)
	}
}

// sync and note call plain Validate: a config whose cloud keys only exist
// in the server unit's environment must still pass there.
func TestValidate_MissingAPIKeyDoesNotBlockSyncOrNote(t *testing.T) {
	for _, p := range []string{"", "gemini", "anthropic"} {
		c := validConfig()
		c.Cloud = &CloudConfig{Provider: p}
		c.CloudFallbacks = []*CloudConfig{{Provider: p}}
		if err := c.Validate(); err != nil {
			t.Errorf("legacy provider %q without api_key: Validate = %v, want nil", p, err)
		}

		h := validHybridConfig()
		h.EscalationTargets["default"] = &CloudConfig{Provider: p}
		if err := h.Validate(); err != nil {
			t.Errorf("hybrid provider %q without api_key: Validate = %v, want nil", p, err)
		}
	}
}

// ValidateForServe must still run every Validate check first.
func TestValidateForServe_IncludesValidate(t *testing.T) {
	c := validConfig()
	c.Summarizer.Endpoint = ""
	wantErrContaining(t, c.ValidateForServe(), "summarizer.endpoint is not set")
}

func TestValidate_LegacyNestedLokiEndpointRejected(t *testing.T) {
	for _, typ := range []string{"", "loki"} {
		c := validConfig()
		c.LogSource = &LogSourceConfig{Type: typ, Loki: &LokiEndpointConfig{Endpoint: "http://other:3100"}}
		err := c.Validate()
		wantErrContaining(t, err, "log_source.loki.endpoint (http://other:3100) is only supported under log_sources with hybrid_routes")
		wantErrContaining(t, err, "set the top-level loki.endpoint instead")
	}
	// An empty nested block changes nothing and stays accepted.
	c := validConfig()
	c.LogSource = &LogSourceConfig{Loki: &LokiEndpointConfig{}}
	if err := c.Validate(); err != nil {
		t.Errorf("empty log_source.loki: Validate = %v, want nil", err)
	}
	// The same override under log_sources (hybrid) is the supported place.
	h := validHybridConfig()
	for _, ls := range h.LogSources {
		if ls.Type == "" || ls.Type == "loki" {
			ls.Loki = &LokiEndpointConfig{Endpoint: "http://other:3100"}
		}
	}
	if err := h.Validate(); err != nil {
		t.Errorf("hybrid log_sources loki.endpoint: Validate = %v, want nil", err)
	}
}
