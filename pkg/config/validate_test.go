package config

import "testing"

func TestValidateHybrid_EnvNameCollision(t *testing.T) {
	c := validHybridConfig()
	c.EscalationTargets["aws-prod"] = &CloudConfig{Provider: "gemini", APIKey: "k"}
	c.EscalationTargets["aws_prod"] = &CloudConfig{Provider: "gemini", APIKey: "k"}
	wantErrContaining(t, c.Validate(), `"aws-prod" and "aws_prod" both map to the env var VICTORIA_GATEWAY_ESCALATION_AWS_PROD_API_KEY`)
}

func TestValidate_GeminiAnthropicAPIKeyRequired(t *testing.T) {
	for _, p := range []string{"", "gemini", "anthropic"} {
		c := validConfig()
		c.Cloud = &CloudConfig{Provider: p}
		wantErrContaining(t, c.Validate(), "cloud.api_key is empty")

		h := validHybridConfig()
		h.EscalationTargets["default"] = &CloudConfig{Provider: p}
		wantErrContaining(t, h.Validate(), "escalation_targets.default.api_key is empty")
	}
	// bedrock uses the AWS credential chain, never an api_key.
	c := validConfig()
	c.Cloud = &CloudConfig{Provider: "bedrock", Region: "us-east-1", Model: "m"}
	if err := c.Validate(); err != nil {
		t.Fatalf("bedrock without api_key: %v", err)
	}
}
