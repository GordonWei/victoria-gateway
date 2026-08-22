package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LLM defines the interface for any local language model backend.
// Implementations can be MLX, Ollama, or any OpenAI-compatible API.
type LLM interface {
	// Chat sends a conversation and returns the assistant's reply.
	Chat(messages []Message, opts *ChatOptions) (string, error)

	// Available checks if the backend is reachable and ready.
	Available() bool

	// ModelName returns the current model identifier.
	ModelName() string

	// Backend returns the backend type (e.g., "mlx", "ollama", "openai-compatible").
	Backend() string
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatOptions struct {
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

// --- OpenAI-Compatible Client (works with MLX, Ollama, LM Studio, vLLM, etc.) ---

type OpenAIClient struct {
	endpoint string
	model    string
	backend  string
	apiKey   string // empty for unauthenticated local servers (LM Studio, Ollama, ...)
	client   *http.Client
}

type OpenAIClientConfig struct {
	Endpoint string // e.g., "http://localhost:8080"
	Model    string // e.g., "mlx-community/Qwen2.5-3B-Instruct-4bit"
	Backend  string // e.g., "mlx", "ollama", "lm-studio"
	// APIKey is optional and sent as "Authorization: Bearer <key>" when
	// set. Local backends (LM Studio, Ollama, vLLM on a private network)
	// typically don't need one; a real cloud OpenAI-compatible endpoint
	// (OpenAI itself, a LiteLLM proxy, Gemini's OpenAI-compatibility
	// layer at generativelanguage.googleapis.com/v1beta/openai) does.
	APIKey  string
	Timeout time.Duration
}

func NewOpenAIClient(cfg OpenAIClientConfig) *OpenAIClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	backend := cfg.Backend
	if backend == "" {
		backend = "openai-compatible"
	}

	return &OpenAIClient{
		endpoint: cfg.Endpoint,
		model:    cfg.Model,
		backend:  backend,
		apiKey:   cfg.APIKey,
		client:   &http.Client{Timeout: timeout},
	}
}

// newRequest builds an HTTP request against this client's endpoint,
// attaching the Authorization header when apiKey is set. Both Chat and
// Available need this, so it's shared rather than duplicated.
func (c *OpenAIClient) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.endpoint+path, body)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *OpenAIClient) Chat(messages []Message, opts *ChatOptions) (string, error) {
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
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: temperature,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := c.newRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request to %s: %w", c.backend, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to %s failed: %w", c.backend, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s returned %d: %s", c.backend, resp.StatusCode, string(respBody))
	}

	var result openAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("%s returned no choices", c.backend)
	}

	return result.Choices[0].Message.Content, nil
}

func (c *OpenAIClient) Available() bool {
	req, err := c.newRequest(http.MethodGet, "/v1/models", nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == 200
}

func (c *OpenAIClient) ModelName() string {
	return c.model
}

func (c *OpenAIClient) Backend() string {
	return c.backend
}

// --- Request/Response types ---

type openAIRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// --- Anthropic Client (cloud escalation target) ---

// AnthropicClient speaks the Anthropic Messages API. It exists so the
// triage path (see pkg/aiops) can escalate a hard-to-diagnose alert from
// the local model to a stronger cloud model without the caller needing to
// know which one it's talking to — both satisfy the same LLM interface.
type AnthropicClient struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

type AnthropicClientConfig struct {
	Endpoint string // defaults to "https://api.anthropic.com" if empty
	APIKey   string
	Model    string // e.g. "claude-haiku-4-5"
	Timeout  time.Duration
}

const anthropicAPIVersion = "2023-06-01"

func NewAnthropicClient(cfg AnthropicClientConfig) *AnthropicClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	return &AnthropicClient{
		endpoint: endpoint,
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		client:   &http.Client{Timeout: timeout},
	}
}

// Chat maps the shared Message/ChatOptions shape onto Anthropic's Messages
// API. Anthropic takes the system prompt as a separate top-level field
// rather than a message with role "system", so any such messages are
// pulled out of the list rather than sent through as-is.
func (c *AnthropicClient) Chat(messages []Message, opts *ChatOptions) (string, error) {
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

	var system string
	chatMessages := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			// Anthropic only accepts one system prompt; later ones (there
			// shouldn't be more than one in practice) get appended rather
			// than silently dropped.
			if system == "" {
				system = m.Content
			} else {
				system += "\n\n" + m.Content
			}
			continue
		}
		chatMessages = append(chatMessages, m)
	}

	reqBody := anthropicRequest{
		Model:       c.model,
		System:      system,
		Messages:    chatMessages,
		MaxTokens:   maxTokens,
		Temperature: temperature,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.endpoint+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to anthropic failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	for _, block := range result.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("anthropic returned no text content block")
}

// Available does a lightweight reachability check. Anthropic has no
// unauthenticated health endpoint, so this sends a minimal (1-token)
// real request — any response that isn't a network-level failure counts
// as "available", including an auth error, since that still confirms the
// endpoint itself is reachable and it's the caller's config that's wrong.
func (c *AnthropicClient) Available() bool {
	req, err := http.NewRequest(http.MethodPost, c.endpoint+"/v1/messages", bytes.NewReader([]byte(
		`{"model":"`+c.model+`","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`,
	)))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

func (c *AnthropicClient) ModelName() string {
	return c.model
}

func (c *AnthropicClient) Backend() string {
	return "anthropic"
}

type anthropicRequest struct {
	Model       string    `json:"model"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// --- Gemini Client (default cloud escalation target) ---

// GeminiClient speaks the Google Gemini generateContent API. It's the
// default cloud escalation target — chosen over Anthropic because this
// deployment already has other Gemini API usage (see Cowork's
// call-gemini skill) and reusing that reduces how many separate API
// keys/billing accounts an operator has to manage for one home-lab
// service.
type GeminiClient struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

type GeminiClientConfig struct {
	Endpoint string // defaults to "https://generativelanguage.googleapis.com" if empty
	APIKey   string
	Model    string // e.g. "gemini-2.5-flash"
	Timeout  time.Duration
}

func NewGeminiClient(cfg GeminiClientConfig) *GeminiClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://generativelanguage.googleapis.com"
	}
	return &GeminiClient{
		endpoint: endpoint,
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		client:   &http.Client{Timeout: timeout},
	}
}

// Chat maps the shared Message/ChatOptions shape onto Gemini's
// generateContent request. Like Anthropic, Gemini takes the system
// prompt as a separate top-level field (systemInstruction) rather than a
// message in the conversation list, and uses "model" rather than
// "assistant" as the role for the model's own turns — this codebase
// never sends an assistant-role message back in, but the mapping is
// still correct if that ever changes.
func (c *GeminiClient) Chat(messages []Message, opts *ChatOptions) (string, error) {
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

	var system *geminiContent
	contents := make([]geminiContent, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if system == nil {
				system = &geminiContent{Parts: []geminiPart{{Text: m.Content}}}
			} else {
				system.Parts = append(system.Parts, geminiPart{Text: "\n\n" + m.Content})
			}
			continue
		}
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, geminiContent{Role: role, Parts: []geminiPart{{Text: m.Content}}})
	}

	reqBody := geminiRequest{
		Contents:          contents,
		SystemInstruction: system,
		GenerationConfig: geminiGenerationConfig{
			MaxOutputTokens: maxTokens,
			Temperature:     temperature,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", c.endpoint, c.model)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to gemini failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("gemini returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini returned no content")
	}
	var text string
	for _, p := range result.Candidates[0].Content.Parts {
		text += p.Text
	}
	if text == "" {
		return "", fmt.Errorf("gemini returned no text content")
	}
	return text, nil
}

// Available lists models with the configured key — a cheap, free call
// (unlike a real generateContent request) that still confirms both the
// endpoint and the key are working.
func (c *GeminiClient) Available() bool {
	req, err := http.NewRequest(http.MethodGet, c.endpoint+"/v1beta/models", nil)
	if err != nil {
		return false
	}
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == 200
}

func (c *GeminiClient) ModelName() string {
	return c.model
}

func (c *GeminiClient) Backend() string {
	return "gemini"
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
	Temperature     float64 `json:"temperature,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
}
