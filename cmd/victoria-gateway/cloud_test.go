package main

import (
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/model"
)

func TestBuildCloud_Providers(t *testing.T) {
	cases := []struct {
		cfg  config.CloudConfig
		want string
	}{
		{config.CloudConfig{}, "*model.GeminiClient"},
		{config.CloudConfig{Provider: "gemini"}, "*model.GeminiClient"},
		{config.CloudConfig{Provider: "anthropic"}, "*model.AnthropicClient"},
		{config.CloudConfig{Provider: "bedrock", Region: "us-east-1", Model: "m"}, "*model.BedrockClient"},
		{config.CloudConfig{Provider: "azure-openai", Endpoint: "https://x", Deployment: "d"}, "*model.AzureOpenAIClient"},
		{config.CloudConfig{Provider: "aws-devops-agent", DevOpsAgent: &config.DevOpsAgentConfig{SpaceID: "s"}}, "*model.DevOpsAgentClient"},
		{config.CloudConfig{Provider: "openai-compatible", Endpoint: "http://x", Model: "m"}, "*model.OpenAIClient"},
		{config.CloudConfig{Provider: "vertex-ai", Project: "p", Model: "m"}, "*model.VertexAIClient"},
	}
	noADC(t)
	for _, tc := range cases {
		c := tc.cfg
		got, err := buildCloud("cloud", &c)
		if err != nil {
			t.Errorf("buildCloud(%q): %v", c.Provider, err)
			continue
		}
		if name := typeName(got); name != tc.want {
			t.Errorf("buildCloud(%q) = %s, want %s", c.Provider, name, tc.want)
		}
	}
}

// The legacy top-level cloud block must keep its exact pre-hybrid error
// messages — they're what an operator of an existing deployment sees.
func TestBuildCloud_LegacyErrorMessagesUnchanged(t *testing.T) {
	cases := []struct {
		cfg  config.CloudConfig
		want string
	}{
		{config.CloudConfig{Provider: "bedrock", Model: "m"}, `cloud.provider is "bedrock" but cloud.region is not set`},
		{config.CloudConfig{Provider: "bedrock", Region: "r"}, `cloud.provider is "bedrock" but cloud.model is not set`},
		{config.CloudConfig{Provider: "azure-openai", Deployment: "d"}, `cloud.provider is "azure-openai" but cloud.endpoint is not set`},
		{config.CloudConfig{Provider: "azure-openai", Endpoint: "e"}, `cloud.provider is "azure-openai" but cloud.deployment is not set`},
		{config.CloudConfig{Provider: "aws-devops-agent"}, `cloud.provider is "aws-devops-agent" but cloud.aws_devops_agent is not set`},
		{config.CloudConfig{Provider: "openai-compatible", Model: "m"}, `cloud.provider is "openai-compatible" but cloud.endpoint is not set`},
		{config.CloudConfig{Provider: "openai-compatible", Endpoint: "http://x"}, `cloud.provider is "openai-compatible" but cloud.model is not set`},
		{config.CloudConfig{Provider: "vertex-ai", Model: "m"}, `cloud.provider is "vertex-ai" but cloud.project is not set`},
		{config.CloudConfig{Provider: "vertex-ai", Project: "p"}, `cloud.provider is "vertex-ai" but cloud.model is not set`},
		{config.CloudConfig{Provider: "nope"}, `unknown cloud.provider "nope" (must be "gemini", "anthropic", "bedrock", "azure-openai", "aws-devops-agent", "openai-compatible", or "vertex-ai")`},
	}
	for _, tc := range cases {
		c := tc.cfg
		_, err := buildCloud("cloud", &c)
		if err == nil || err.Error() != tc.want {
			t.Errorf("buildCloud(%+v) err = %v, want %q", c, err, tc.want)
		}
	}
}

func typeName(v model.LLM) string {
	switch v.(type) {
	case *model.GeminiClient:
		return "*model.GeminiClient"
	case *model.AnthropicClient:
		return "*model.AnthropicClient"
	case *model.BedrockClient:
		return "*model.BedrockClient"
	case *model.AzureOpenAIClient:
		return "*model.AzureOpenAIClient"
	case *model.DevOpsAgentClient:
		return "*model.DevOpsAgentClient"
	case *model.OpenAIClient:
		return "*model.OpenAIClient"
	case *model.VertexAIClient:
		return "*model.VertexAIClient"
	}
	return "unknown"
}
