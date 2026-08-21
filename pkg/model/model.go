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
	client   *http.Client
}

type OpenAIClientConfig struct {
	Endpoint string // e.g., "http://localhost:8080"
	Model    string // e.g., "mlx-community/Qwen2.5-3B-Instruct-4bit"
	Backend  string // e.g., "mlx", "ollama", "lm-studio"
	Timeout  time.Duration
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
		client:   &http.Client{Timeout: timeout},
	}
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

	resp, err := c.client.Post(c.endpoint+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request to %s failed: %w", c.backend, err)
	}
	defer resp.Body.Close()

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
	resp, err := c.client.Get(c.endpoint + "/v1/models")
	if err != nil {
		return false
	}
	resp.Body.Close()
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
