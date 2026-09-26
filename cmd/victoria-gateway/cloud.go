package main

import (
	"fmt"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/model"
)

// buildCloud constructs the escalation client one cloud block describes —
// the legacy top-level cloud (label "cloud") or one named entry of
// escalation_targets (label "escalation_targets.<name>"). Shared by both
// paths so a provider added later only has to be wired in once.
func buildCloud(label string, c *config.CloudConfig) (model.LLM, error) {
	switch c.Provider {
	case "", "gemini":
		return model.NewGeminiClient(model.GeminiClientConfig{
			Endpoint: c.Endpoint,
			APIKey:   c.APIKey,
			Model:    c.Model,
		}), nil
	case "anthropic":
		return model.NewAnthropicClient(model.AnthropicClientConfig{
			Endpoint: c.Endpoint,
			APIKey:   c.APIKey,
			Model:    c.Model,
		}), nil
	case "bedrock":
		if c.Region == "" {
			return nil, fmt.Errorf("%s.provider is \"bedrock\" but %s.region is not set", label, label)
		}
		if c.Model == "" {
			return nil, fmt.Errorf("%s.provider is \"bedrock\" but %s.model is not set", label, label)
		}
		return model.NewBedrockClient(model.BedrockClientConfig{
			Region:   c.Region,
			Model:    c.Model,
			Endpoint: c.Endpoint,
		}), nil
	case "azure-openai":
		if c.Endpoint == "" {
			return nil, fmt.Errorf("%s.provider is \"azure-openai\" but %s.endpoint is not set", label, label)
		}
		if c.Deployment == "" {
			return nil, fmt.Errorf("%s.provider is \"azure-openai\" but %s.deployment is not set", label, label)
		}
		return model.NewAzureOpenAIClient(model.AzureOpenAIClientConfig{
			Endpoint:   c.Endpoint,
			Deployment: c.Deployment,
			APIKey:     c.APIKey,
			APIVersion: c.APIVersion,
		}), nil
	case "aws-devops-agent":
		da := c.DevOpsAgent
		if da == nil {
			return nil, fmt.Errorf("%s.provider is \"aws-devops-agent\" but %s.aws_devops_agent is not set", label, label)
		}
		return model.NewDevOpsAgentClient(model.DevOpsAgentClientConfig{
			BinaryPath: da.BinaryPath,
			UserID:     da.UserID,
			Region:     da.Region,
			SpaceID:    da.SpaceID,
			Priority:   da.Priority,
		}), nil
	default:
		return nil, fmt.Errorf("unknown %s.provider %q (must be \"gemini\", \"anthropic\", \"bedrock\", \"azure-openai\", or \"aws-devops-agent\")", label, c.Provider)
	}
}
