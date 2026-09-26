package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validHybridConfig is a minimal valid hybrid setup: AWS alerts go to
// CloudWatch + AWS DevOps Agent, everything else to on-prem Loki +
// Gemini. Tests mutate a fresh copy to hit one rule at a time.
func validHybridConfig() *Config {
	c := validConfig()
	c.LogSources = map[string]*LogSourceConfig{
		"onprem": {Type: "loki"},
		"aws":    {Type: "cloudwatch", CloudWatch: &CloudWatchConfig{Region: "us-east-1", LogGroupNames: []string{"/aws/lambda/checkout"}}},
	}
	c.EscalationTargets = map[string]*CloudConfig{
		"default": {Provider: "gemini", APIKey: "k"},
		"aws":     {Provider: "aws-devops-agent", DevOpsAgent: &DevOpsAgentConfig{SpaceID: "s"}},
	}
	c.HybridRoutes = []HybridRouteConfig{
		{Matchers: map[string]string{"cloud": "aws"}, LogSource: "aws", Escalation: "aws"},
		{Default: true, LogSource: "onprem", Escalation: "default"},
	}
	return c
}

func wantErrContaining(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Validate() = nil, want an error containing %q", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("Validate() = %q, want it to contain %q", err, substr)
	}
}

func TestValidateHybrid_Valid(t *testing.T) {
	if err := validHybridConfig().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateHybrid_TopLevelLokiNotRequiredWhenNoLokiSource(t *testing.T) {
	c := validHybridConfig()
	c.Loki.Endpoint = ""
	c.LogSources = map[string]*LogSourceConfig{
		"aws": {Type: "cloudwatch", CloudWatch: &CloudWatchConfig{Region: "us-east-1", LogGroupNames: []string{"/g"}}},
		"gcp": {Type: "gcp_logging", GCPLogging: &GCPLoggingConfig{ProjectID: "p"}},
	}
	c.HybridRoutes = []HybridRouteConfig{
		{Matchers: map[string]string{"cloud": "aws"}, LogSource: "aws", Escalation: "aws"},
		{Default: true, LogSource: "gcp", Escalation: "default"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil (no loki source, so loki.endpoint is not needed)", err)
	}
}

func TestValidateHybrid_LokiSourceOwnEndpoint(t *testing.T) {
	c := validHybridConfig()
	c.Loki.Endpoint = ""
	c.LogSources["onprem"] = &LogSourceConfig{Type: "loki", Loki: &LokiEndpointConfig{Endpoint: "http://loki-site-b:3100"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil (the source has its own loki.endpoint)", err)
	}
}

func TestValidateHybrid_LokiSourceWithoutAnyEndpoint(t *testing.T) {
	c := validHybridConfig()
	c.Loki.Endpoint = ""
	wantErrContaining(t, c.Validate(), "log_sources.onprem")
}

func TestValidateHybrid_MissingDefault(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes = c.HybridRoutes[:1]
	delete(c.LogSources, "onprem")
	delete(c.EscalationTargets, "default")
	wantErrContaining(t, c.Validate(), "no default route")
}

func TestValidateHybrid_DefaultNotLast(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes = []HybridRouteConfig{c.HybridRoutes[1], c.HybridRoutes[0]}
	wantErrContaining(t, c.Validate(), "hybrid_routes[0]: the default route must be the last route")
}

func TestValidateHybrid_DefaultWithMatchers(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[1].Matchers = map[string]string{"x": "y"}
	wantErrContaining(t, c.Validate(), "default route must not have matchers")
}

func TestValidateHybrid_NonDefaultWithoutMatchers(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[0].Matchers = nil
	wantErrContaining(t, c.Validate(), "hybrid_routes[0]: matchers must not be empty")
}

func TestValidateHybrid_UndefinedLogSource(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[0].LogSource = "awz"
	wantErrContaining(t, c.Validate(), `hybrid_routes[0]: log_source "awz" is not defined`)
}

func TestValidateHybrid_MissingLogSourceName(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[0].LogSource = ""
	wantErrContaining(t, c.Validate(), "hybrid_routes[0]: log_source is required")
}

func TestValidateHybrid_UndefinedEscalation(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[0].Escalation = "devops"
	wantErrContaining(t, c.Validate(), `hybrid_routes[0]: escalation "devops" is not defined`)
}

func TestValidateHybrid_RouteWithoutEscalationIsAllowed(t *testing.T) {
	c := validHybridConfig()
	c.HybridRoutes[1].Escalation = ""
	delete(c.EscalationTargets, "default")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil (a route may opt out of escalation)", err)
	}
}

func TestValidateHybrid_UnusedLogSource(t *testing.T) {
	c := validHybridConfig()
	c.LogSources["gcp"] = &LogSourceConfig{Type: "gcp_logging", GCPLogging: &GCPLoggingConfig{ProjectID: "p"}}
	wantErrContaining(t, c.Validate(), "log_sources.gcp is defined but no hybrid_routes entry uses it")
}

func TestValidateHybrid_UnusedEscalationTarget(t *testing.T) {
	c := validHybridConfig()
	c.EscalationTargets["bedrock"] = &CloudConfig{Provider: "bedrock", Region: "us-east-1", Model: "m"}
	wantErrContaining(t, c.Validate(), "escalation_targets.bedrock is defined but no hybrid_routes entry uses it")
}

func TestValidateHybrid_InvalidLogSourceEntry(t *testing.T) {
	c := validHybridConfig()
	c.LogSources["aws"].CloudWatch.Region = ""
	wantErrContaining(t, c.Validate(), "log_sources.aws.type is \"cloudwatch\"")
}

func TestValidateHybrid_InvalidEscalationTarget(t *testing.T) {
	c := validHybridConfig()
	c.EscalationTargets["aws"].DevOpsAgent = nil
	wantErrContaining(t, c.Validate(), "escalation_targets.aws.provider is \"aws-devops-agent\"")
}

func TestValidateHybrid_UnknownProvider(t *testing.T) {
	c := validHybridConfig()
	c.EscalationTargets["default"].Provider = "openrouter"
	wantErrContaining(t, c.Validate(), `escalation_targets.default.provider is "openrouter"`)
}

func TestValidateHybrid_WithLegacyLogSourceRejected(t *testing.T) {
	c := validHybridConfig()
	c.LogSource = &LogSourceConfig{Type: "loki"}
	wantErrContaining(t, c.Validate(), "hybrid_routes and log_source are both set")
}

func TestValidateHybrid_WithLegacyCloudRejected(t *testing.T) {
	c := validHybridConfig()
	c.Cloud = &CloudConfig{Provider: "gemini"}
	wantErrContaining(t, c.Validate(), "hybrid_routes and cloud are both set")
}

func TestValidate_LogSourcesWithoutRoutesRejected(t *testing.T) {
	c := validConfig()
	c.LogSources = map[string]*LogSourceConfig{"onprem": {Type: "loki"}}
	wantErrContaining(t, c.Validate(), "hybrid_routes is empty")
}

func TestValidateHybrid_AlwaysCloudSatisfiedByTargets(t *testing.T) {
	c := validHybridConfig()
	c.Escalation.AlwaysCloud = []string{"cpu_high"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil (escalation_targets count as a cloud for always_cloud)", err)
	}
}

func TestLoad_HybridYAML(t *testing.T) {
	yaml := `
summarizer: {endpoint: "http://llm"}
loki: {endpoint: "http://loki:3100"}
log_sources:
  onprem: {type: loki}
  aws:
    type: cloudwatch
    cloudwatch: {region: us-east-1, log_group_names: ["/aws/lambda/checkout"]}
escalation_targets:
  default: {provider: gemini, api_key: from-file}
  aws-prod:
    provider: aws-devops-agent
    aws_devops_agent: {space_id: s}
hybrid_routes:
  - matchers: {cloud: aws}
    log_source: aws
    escalation: aws-prod
  - default: true
    log_source: onprem
    escalation: default
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VICTORIA_GATEWAY_ESCALATION_DEFAULT_API_KEY", "from-env")
	t.Setenv("VICTORIA_GATEWAY_ESCALATION_AWS_PROD_API_KEY", "aws-env")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := c.EscalationTargets["default"].APIKey; got != "from-env" {
		t.Errorf("default api_key = %q, want the env override", got)
	}
	if got := c.EscalationTargets["aws-prod"].APIKey; got != "aws-env" {
		t.Errorf("aws-prod api_key = %q, want the env override (dash mapped to _)", got)
	}
	if len(c.HybridRoutes) != 2 || c.HybridRoutes[0].Matchers["cloud"] != "aws" || !c.HybridRoutes[1].Default {
		t.Errorf("hybrid_routes parsed as %+v", c.HybridRoutes)
	}
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{"default": "DEFAULT", "aws-prod": "AWS_PROD", "gcp.asia": "GCP_ASIA", "x1": "X1"} {
		if got := EnvName(in); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", in, got, want)
		}
	}
}
