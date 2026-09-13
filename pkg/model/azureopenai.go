package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AzureOpenAIClient speaks the Azure OpenAI (Microsoft Foundry) chat
// completions API — a general-purpose cloud escalation target for
// operators whose org already standardizes on Azure, or who need
// inference to stay inside an Azure tenant for compliance reasons. Like
// AnthropicClient and GeminiClient, this is plain net/http against a JSON
// REST API with no extra SDK dependency; unlike Bedrock, Azure OpenAI
// authenticates with a static header value (api-key) rather than a
// signed request, so there's nothing here an SDK would meaningfully do
// better.
//
// Request/response bodies are the OpenAI Chat Completions shape, so this
// reuses openAIRequest/openAIResponse from model.go rather than
// redefining an identical pair of structs.
type AzureOpenAIClient struct {
	endpoint   string
	deployment string
	apiKey     string
	apiVersion string
	client     *http.Client
}

type AzureOpenAIClientConfig struct {
	// Endpoint is the resource's base URL, e.g.
	// "https://myresource.openai.azure.com" — no path suffix.
	Endpoint string

	// Deployment is the Azure *deployment name* the request is sent to —
	// not the underlying model name. In Azure OpenAI, a customer creates
	// a named deployment of a given base model (e.g. a deployment named
	// "prod-gpt4o" backed by "gpt-4o"), and requests address the
	// deployment, not the model, directly. Getting this confused with
	// Model in the other clients' configs is the single most common
	// Azure OpenAI setup mistake, so it has its own field name here
	// instead of overloading Model to mean something different than it
	// does for Bedrock/Anthropic/Gemini.
	Deployment string

	APIKey string

	// APIVersion is the api-version query parameter. Deliberately not
	// hard-coded to a value baked in at some point in the past — Azure's
	// API versioning has changed shape over time (dated versions like
	// "2024-06-01" for the classic deployments-path API, versus the
	// newer unified "v1" data-plane API this client targets), and a
	// stale hard-coded default is exactly the kind of thing that quietly
	// breaks months later. Empty defaults to
	// azureOpenAIDefaultAPIVersion (see its doc comment for the source).
	APIVersion string

	Timeout time.Duration
}

// azureOpenAIDefaultAPIVersion is Microsoft's current documented default
// for the unified Azure OpenAI / Microsoft Foundry data-plane inference
// API (server base "{endpoint}/openai/v1"): "the explicit Azure AI
// Foundry Models API version to use for this request... v1 if not
// otherwise specified." Verified against
// learn.microsoft.com/en-us/rest/api/microsoft-foundry/azureopenai/chat
// (v1 GA reference, last updated 2026-07-09) on 2026-09-13 rather than
// assumed from older training data — Azure had previously used
// YYYY-MM-DD dated versions (e.g. "2024-06-01") against a different URL
// shape ("{endpoint}/openai/deployments/{deployment}/chat/completions"),
// which is still supported but is the *older* scheme, not this one. If
// this default ever looks wrong, re-check that reference page rather
// than assuming it's stale from memory.
const azureOpenAIDefaultAPIVersion = "v1"

func NewAzureOpenAIClient(cfg AzureOpenAIClientConfig) *AzureOpenAIClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	apiVersion := cfg.APIVersion
	if apiVersion == "" {
		apiVersion = azureOpenAIDefaultAPIVersion
	}
	return &AzureOpenAIClient{
		endpoint:   cfg.Endpoint,
		deployment: cfg.Deployment,
		apiKey:     cfg.APIKey,
		apiVersion: apiVersion,
		client:     &http.Client{Timeout: timeout},
	}
}

func (c *AzureOpenAIClient) url() string {
	return fmt.Sprintf("%s/openai/v1/chat/completions?api-version=%s", c.endpoint, c.apiVersion)
}

// Chat sends an OpenAI-shaped chat completion request with the
// deployment name in the body's "model" field — the v1 data-plane API
// selects the deployment that way rather than via a path segment (the
// older dated-version API put the deployment name in the URL path
// instead; this client only implements the current scheme, see
// azureOpenAIDefaultAPIVersion's doc comment).
func (c *AzureOpenAIClient) Chat(messages []Message, opts *ChatOptions) (string, error) {
	maxTokens := 1024
	temperature := 0.1
	if opts != nil {
		if opts.MaxTokens > 0 {
			maxTokens = opts.MaxTokens
		}
		if opts.Temperature > 0 {
			temperature = opts.Temperature
		}
	}

	reqBody := openAIRequest{
		Model:       c.deployment,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: temperature,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.url(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to azure-openai failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("azure-openai returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("azure-openai returned no choices")
	}
	return result.Choices[0].Message.Content, nil
}

// Available does a lightweight reachability check the same way
// AnthropicClient.Available does: Azure OpenAI has no unauthenticated
// health endpoint equivalent to /v1/models on a plain OpenAI-compatible
// server, so this sends a minimal (1-token) real request instead. Any
// response that isn't a network-level failure counts as available,
// including an auth or deployment-not-found error, since that still
// confirms the resource endpoint itself is reachable.
func (c *AzureOpenAIClient) Available() bool {
	body, err := json.Marshal(openAIRequest{
		Model:     c.deployment,
		Messages:  []Message{{Role: "user", Content: "ping"}},
		MaxTokens: 1,
	})
	if err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodPost, c.url(), bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func (c *AzureOpenAIClient) ModelName() string {
	return c.deployment
}

func (c *AzureOpenAIClient) Backend() string {
	return "azure-openai"
}
