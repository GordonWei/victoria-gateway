package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadYAML(t *testing.T, yaml string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestLoad_EscalationStringOrList(t *testing.T) {
	c := loadYAML(t, `
summarizer: {endpoint: "http://llm"}
loki: {endpoint: "http://loki:3100"}
log_sources:
  onprem: {type: loki}
escalation_targets:
  aws: {provider: bedrock, region: us-east-1, model: m}
  default: {provider: gemini, api_key: k}
hybrid_routes:
  - matchers: {cloud: aws}
    log_source: onprem
    escalation: [aws, default]
  - matchers: {site: b}
    log_source: onprem
    escalation: default
  - matchers: {site: c}
    log_source: onprem
    escalation: ""
  - default: true
    log_source: onprem
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []EscalationList{{"aws", "default"}, {"default"}, nil, nil}
	for i, w := range want {
		got := c.HybridRoutes[i].Escalation
		if len(got) != len(w) {
			t.Fatalf("route %d escalation = %v, want %v", i, got, w)
		}
		for j := range w {
			if got[j] != w[j] {
				t.Errorf("route %d escalation = %v, want %v", i, got, w)
			}
		}
	}
}

func TestLoad_EscalationRejectsMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(path, []byte("hybrid_routes:\n  - default: true\n    escalation: {a: b}\n"), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("Load = nil error, want a parse error for a mapping escalation")
	}
}

func TestValidateHybrid_EscalationListDuplicate(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[1].Escalation = EscalationList{"default", "default"}
	wantErrContaining(t, c.Validate(), `lists "default" more than once`)
}

func TestValidateHybrid_EscalationListUndefinedSecond(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[1].Escalation = EscalationList{"default", "nope"}
	wantErrContaining(t, c.Validate(), `escalation "nope" is not defined`)
}

func TestValidate_SummarizerFallbacks(t *testing.T) {
	c := validConfig()
	c.Summarizer.Fallbacks = []LLMConfig{{Endpoint: "http://b", Model: "m"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid fallback: %v", err)
	}
	c.Summarizer.Fallbacks = []LLMConfig{{Model: "m"}}
	wantErrContaining(t, c.Validate(), "summarizer.fallbacks[0].endpoint is not set")
	c.Summarizer.Fallbacks = []LLMConfig{{Endpoint: "http://b", TimeoutSec: -1}}
	wantErrContaining(t, c.Validate(), "summarizer.fallbacks[0].timeout_sec")
	c.Summarizer.Fallbacks = []LLMConfig{{Endpoint: "http://b", Fallbacks: []LLMConfig{{Endpoint: "http://c"}}}}
	wantErrContaining(t, c.Validate(), "has fallbacks of its own")
}

func TestValidate_CloudFallbacks(t *testing.T) {
	c := validConfig()
	c.CloudFallbacks = []*CloudConfig{{Provider: "bedrock", Region: "us-east-1", Model: "m"}}
	wantErrContaining(t, c.Validate(), "cloud_fallbacks is set but cloud is not")

	c.Cloud = &CloudConfig{Provider: "bedrock", Region: "us-east-1", Model: "m"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid cloud_fallbacks: %v", err)
	}
	c.CloudFallbacks = []*CloudConfig{{Provider: "anthropic"}}
	wantErrContaining(t, c.Validate(), "cloud_fallbacks[0].api_key is empty")
	c.CloudFallbacks = []*CloudConfig{{Provider: "bedrock"}}
	wantErrContaining(t, c.Validate(), "cloud_fallbacks[0].provider is \"bedrock\"")
	c.CloudFallbacks = []*CloudConfig{nil}
	wantErrContaining(t, c.Validate(), "cloud_fallbacks[0] is empty")

	h := validHybridConfig()
	h.CloudFallbacks = []*CloudConfig{{Provider: "bedrock", Region: "r", Model: "m"}}
	wantErrContaining(t, h.Validate(), "cloud_fallbacks is set together with hybrid_routes")
}

func TestLoad_FallbackYAMLAndEnv(t *testing.T) {
	t.Setenv("VICTORIA_GATEWAY_SUMMARIZER_FALLBACK_0_API_KEY", "sum-env")
	t.Setenv("VICTORIA_GATEWAY_CLOUD_FALLBACK_1_API_KEY", "cloud-env")
	c := loadYAML(t, `
summarizer:
  endpoint: "http://mlx:8091"
  model: primary
  fallbacks:
    - {endpoint: "http://lmstudio:1234", model: backup, timeout_sec: 90}
loki: {endpoint: "http://loki:3100"}
cloud: {provider: bedrock, region: us-east-1, model: haiku}
cloud_fallbacks:
  - {provider: bedrock, region: us-west-2, model: haiku}
  - {provider: anthropic, model: claude}
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	fb := c.Summarizer.Fallbacks
	if len(fb) != 1 || fb[0].Endpoint != "http://lmstudio:1234" || fb[0].TimeoutSec != 90 || fb[0].APIKey != "sum-env" {
		t.Errorf("summarizer.fallbacks = %+v", fb)
	}
	if len(c.CloudFallbacks) != 2 || c.CloudFallbacks[0].Region != "us-west-2" || c.CloudFallbacks[1].APIKey != "cloud-env" {
		t.Errorf("cloud_fallbacks = %+v", c.CloudFallbacks)
	}
}
