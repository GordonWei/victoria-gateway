package config

import (
	"strconv"
	"testing"
)

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

func TestValidate_OpenAICompatible(t *testing.T) {
	h := validHybridConfig()
	h.EscalationTargets["default"] = &CloudConfig{Provider: "openai-compatible", Endpoint: "http://vllm:8000"}
	wantErrContaining(t, h.Validate(), `escalation_targets.default.provider is "openai-compatible" but escalation_targets.default.endpoint/model is missing`)

	h.EscalationTargets["default"] = &CloudConfig{Provider: "openai-compatible", Model: "m"}
	wantErrContaining(t, h.Validate(), "endpoint/model is missing")

	// api_key is optional: an unauthenticated vLLM/Ollama passes both
	// Validate and ValidateForServe.
	h.EscalationTargets["default"] = &CloudConfig{Provider: "openai-compatible", Endpoint: "http://vllm:8000", Model: "m"}
	if err := h.ValidateForServe(); err != nil {
		t.Errorf("openai-compatible without api_key: ValidateForServe = %v, want nil", err)
	}

	f := validConfig()
	f.Cloud = &CloudConfig{Provider: "gemini", APIKey: "k"}
	f.CloudFallbacks = []*CloudConfig{{Provider: "openai-compatible", Endpoint: "https://openrouter.ai/api", Model: "m", APIKey: "k"}}
	if err := f.ValidateForServe(); err != nil {
		t.Errorf("openai-compatible cloud_fallbacks: ValidateForServe = %v, want nil", err)
	}
	f.CloudFallbacks[0].Model = ""
	wantErrContaining(t, f.Validate(), "cloud_fallbacks[0].provider is \"openai-compatible\"")
}

func TestValidate_NegativeCloudTimeout(t *testing.T) {
	c := validConfig()
	c.Cloud = &CloudConfig{Provider: "openai-compatible", Endpoint: "http://x", Model: "m", TimeoutSec: -1}
	wantErrContaining(t, c.Validate(), "cloud.timeout_sec must be >= 0")

	h := validHybridConfig()
	h.EscalationTargets["default"] = &CloudConfig{Provider: "openai-compatible", Endpoint: "http://x", Model: "m", TimeoutSec: -5}
	wantErrContaining(t, h.Validate(), "escalation_targets.default.timeout_sec must be >= 0")
}

func TestValidate_VertexAI(t *testing.T) {
	h := validHybridConfig()
	h.EscalationTargets["default"] = &CloudConfig{Provider: "vertex-ai", Model: "gemini-2.5-flash"}
	wantErrContaining(t, h.Validate(), `escalation_targets.default.provider is "vertex-ai" but escalation_targets.default.project/model is missing`)

	h.EscalationTargets["default"] = &CloudConfig{Provider: "vertex-ai", Project: "my-proj", Model: "gemini-2.5-flash"}
	if err := h.ValidateForServe(); err != nil {
		t.Errorf("vertex-ai without api_key: ValidateForServe = %v, want nil", err)
	}

	h.EscalationTargets["default"].Location = "us-central1.evil.com/"
	wantErrContaining(t, h.Validate(), `escalation_targets.default.location "us-central1.evil.com/" is not a valid location`)
	h.EscalationTargets["default"].Location = "global"
	h.EscalationTargets["default"].Project = "p/../x"
	wantErrContaining(t, h.Validate(), `escalation_targets.default.project "p/../x" is not a valid GCP project ID`)

	// The legacy cloud block and cloud_fallbacks get the same format checks.
	c := validConfig()
	c.Cloud = &CloudConfig{Provider: "vertex-ai", Project: "my-proj", Model: "m", Location: "bad host"}
	wantErrContaining(t, c.Validate(), `cloud.location "bad host"`)
	c.Cloud = &CloudConfig{Provider: "gemini", APIKey: "k"}
	c.CloudFallbacks = []*CloudConfig{{Provider: "vertex-ai", Model: "m"}}
	wantErrContaining(t, c.Validate(), `cloud_fallbacks[0].provider is "vertex-ai" but cloud_fallbacks[0].project/model is missing`)
	c.CloudFallbacks[0].Project = "my-proj"
	if err := c.ValidateForServe(); err != nil {
		t.Errorf("vertex-ai cloud_fallbacks: ValidateForServe = %v, want nil", err)
	}
}

// vertex-ai never sends an api_key, so ValidateForServe rejects one on
// every kind of block — but plain Validate (note/sync) doesn't look at
// keys at all, same as for the gemini/anthropic rule.
func TestValidateForServe_ADCProviderRejectsAPIKey(t *testing.T) {
	c := validConfig()
	c.Cloud = &CloudConfig{Provider: "vertex-ai", Project: "p", Model: "m", APIKey: "leftover"}
	wantErrContaining(t, c.ValidateForServe(), `cloud.provider is "vertex-ai" but cloud.api_key is set`)
	if err := c.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil (api_key rules are serve-only)", err)
	}

	h := validHybridConfig()
	h.EscalationTargets["default"] = &CloudConfig{Provider: "vertex-ai", Project: "p", Model: "m", APIKey: "leftover"}
	wantErrContaining(t, h.ValidateForServe(), `escalation_targets.default.provider is "vertex-ai" but escalation_targets.default.api_key is set`)

	f := validConfig()
	f.Cloud = &CloudConfig{Provider: "gemini", APIKey: "k"}
	f.CloudFallbacks = []*CloudConfig{{Provider: "vertex-ai", Project: "p", Model: "m", APIKey: "leftover"}}
	wantErrContaining(t, f.ValidateForServe(), `cloud_fallbacks[0].provider is "vertex-ai" but cloud_fallbacks[0].api_key is set`)
}

// An env var left over from a gemini setup lands in api_key via Load and
// must be caught, not silently ignored.
func TestLoad_VertexAIWithLeftoverEnvKeyRejectedForServe(t *testing.T) {
	t.Setenv("VICTORIA_GATEWAY_ESCALATION_GCP_API_KEY", "leftover")
	c := loadYAML(t, `
loki: {endpoint: "http://loki:3100"}
summarizer: {endpoint: "http://llm:1234", model: m}
log_sources: {onprem: {type: loki}}
escalation_targets:
  gcp: {provider: vertex-ai, project: my-proj, location: us-central1, model: gemini-2.5-flash, timeout_sec: 90}
hybrid_routes:
  - default: true
    log_source: onprem
    escalation: gcp
`)
	if got := c.EscalationTargets["gcp"]; got.Project != "my-proj" || got.Location != "us-central1" || got.TimeoutSec != 90 {
		t.Errorf("parsed target = %+v", got)
	}
	wantErrContaining(t, c.ValidateForServe(), "escalation_targets.gcp.api_key is set")
}

func TestValidate_GCPCloudAssist(t *testing.T) {
	h := validHybridConfig()
	h.EscalationTargets["aws"] = &CloudConfig{Provider: "gcp-cloud-assist"}
	wantErrContaining(t, h.Validate(), `escalation_targets.aws.provider is "gcp-cloud-assist" but escalation_targets.aws.project is missing`)

	h.EscalationTargets["aws"] = &CloudConfig{Provider: "gcp-cloud-assist", Project: "my-proj", PollIntervalSec: 20, PollTimeoutSec: 900}
	if err := h.ValidateForServe(); err != nil {
		t.Errorf("gcp-cloud-assist: ValidateForServe = %v, want nil", err)
	}
	h.EscalationTargets["aws"].Location = "global"
	if err := h.Validate(); err != nil {
		t.Errorf("location global: Validate = %v, want nil", err)
	}
	h.EscalationTargets["aws"].Location = "us-central1"
	wantErrContaining(t, h.Validate(), `escalation_targets.aws.location is "us-central1", but the Gemini Cloud Assist investigations API only serves "global"`)
	h.EscalationTargets["aws"].Location = ""
	h.EscalationTargets["aws"].PollTimeoutSec = -1
	wantErrContaining(t, h.Validate(), "escalation_targets.aws.poll_interval_sec/poll_timeout_sec must be >= 0")
	h.EscalationTargets["aws"].PollTimeoutSec = 0

	h.EscalationTargets["aws"].APIKey = "leftover"
	wantErrContaining(t, h.ValidateForServe(), `escalation_targets.aws.provider is "gcp-cloud-assist" but escalation_targets.aws.api_key is set`)

	// Legacy cloud and cloud_fallbacks accept it too.
	c := validConfig()
	c.Cloud = &CloudConfig{Provider: "gcp-cloud-assist", Project: "my-proj"}
	c.CloudFallbacks = []*CloudConfig{{Provider: "vertex-ai", Project: "my-proj", Model: "gemini-2.5-flash"}}
	if err := c.ValidateForServe(); err != nil {
		t.Errorf("legacy gcp-cloud-assist + vertex-ai fallback: ValidateForServe = %v, want nil", err)
	}
	c.CloudFallbacks = []*CloudConfig{{Provider: "gcp-cloud-assist"}}
	wantErrContaining(t, c.Validate(), `cloud_fallbacks[0].provider is "gcp-cloud-assist" but cloud_fallbacks[0].project is missing`)
}

// The vertex-ai model is a URL path segment, so anything that could
// reshape the path is rejected on every kind of block, and the other
// providers' model names (which aren't path segments) aren't held to it.
func TestValidate_VertexAIModelID(t *testing.T) {
	for _, ok := range []string{"gemini-2.5-flash", "gemini-3.5-flash", "claude-opus-4@20250514", "text_embedding.v2"} {
		c := validConfig()
		c.Cloud = &CloudConfig{Provider: "vertex-ai", Project: "p", Model: ok}
		if err := c.Validate(); err != nil {
			t.Errorf("model %q: Validate = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"../x", "gemini/2", "m?alt=1", "m#frag", "m m", "m%2F"} {
		c := validConfig()
		c.Cloud = &CloudConfig{Provider: "vertex-ai", Project: "p", Model: bad}
		wantErrContaining(t, c.Validate(), "cloud.model "+strconv.Quote(bad)+" is not a valid Vertex AI model ID")

		h := validHybridConfig()
		h.EscalationTargets["default"] = &CloudConfig{Provider: "vertex-ai", Project: "p", Model: bad}
		wantErrContaining(t, h.Validate(), "escalation_targets.default.model")

		f := validConfig()
		f.Cloud = &CloudConfig{Provider: "gemini", APIKey: "k"}
		f.CloudFallbacks = []*CloudConfig{{Provider: "vertex-ai", Project: "p", Model: bad}}
		wantErrContaining(t, f.Validate(), "cloud_fallbacks[0].model")
	}
	o := validConfig()
	o.Cloud = &CloudConfig{Provider: "openai-compatible", Endpoint: "http://x", Model: "org/model:tag"}
	if err := o.Validate(); err != nil {
		t.Errorf("openai-compatible with a slash in model: Validate = %v, want nil", err)
	}
}
