package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIClient_Chat(t *testing.T) {
	// Mock OpenAI-compatible server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			resp := openAIResponse{
				Choices: []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				}{
					{Message: struct {
						Content string `json:"content"`
					}{Content: "test reply"}},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(200)
			w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()

	client := NewOpenAIClient(OpenAIClientConfig{
		Endpoint: server.URL,
		Model:    "test-model",
		Backend:  "test",
	})

	// Test Available
	if !client.Available() {
		t.Error("expected client to be available")
	}

	// Test Chat
	reply, err := client.Chat([]Message{
		{Role: "user", Content: "hello"},
	}, nil)
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if reply != "test reply" {
		t.Errorf("expected 'test reply', got %q", reply)
	}

	// Test ModelName
	if client.ModelName() != "test-model" {
		t.Errorf("expected 'test-model', got %q", client.ModelName())
	}

	// Test Backend
	if client.Backend() != "test" {
		t.Errorf("expected 'test', got %q", client.Backend())
	}
}

func TestOpenAIClient_Unavailable(t *testing.T) {
	client := NewOpenAIClient(OpenAIClientConfig{
		Endpoint: "http://localhost:19999", // nothing here
		Model:    "ghost-model",
		Backend:  "mlx",
	})

	if client.Available() {
		t.Error("expected client to be unavailable")
	}

	_, err := client.Chat([]Message{
		{Role: "user", Content: "hello"},
	}, nil)
	if err == nil {
		t.Error("expected error when server is down")
	}
}

func TestOpenAIClient_ChatOptions(t *testing.T) {
	var receivedBody openAIRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			json.NewDecoder(r.Body).Decode(&receivedBody)
			resp := openAIResponse{
				Choices: []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				}{
					{Message: struct {
						Content string `json:"content"`
					}{Content: "ok"}},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
	}))
	defer server.Close()

	client := NewOpenAIClient(OpenAIClientConfig{
		Endpoint: server.URL,
		Model:    "test-model",
	})

	_, err := client.Chat([]Message{
		{Role: "user", Content: "test"},
	}, &ChatOptions{MaxTokens: 2048, Temperature: 0.7})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}

	if receivedBody.MaxTokens != 2048 {
		t.Errorf("expected max_tokens=2048, got %d", receivedBody.MaxTokens)
	}
	if receivedBody.Temperature != 0.7 {
		t.Errorf("expected temperature=0.7, got %f", receivedBody.Temperature)
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// New Coverage Tests
// ══════════════════════════════════════════════════════════════════════════════

// TestOpenAIClient_Unavailable_Explicit verifies Available() returns false for unreachable endpoint.
func TestOpenAIClient_Unavailable_Explicit(t *testing.T) {
	client := NewOpenAIClient(OpenAIClientConfig{
		Endpoint: "http://127.0.0.1:19876", // port nobody listens on
		Model:    "nonexistent-model",
		Backend:  "test",
	})

	if client.Available() {
		t.Error("Available() should return false for unreachable endpoint")
	}
}

// TestOpenAIClient_ChatError verifies Chat() with unreachable server returns error.
func TestOpenAIClient_ChatError(t *testing.T) {
	client := NewOpenAIClient(OpenAIClientConfig{
		Endpoint: "http://127.0.0.1:19876",
		Model:    "nonexistent-model",
		Backend:  "test",
	})

	_, err := client.Chat([]Message{
		{Role: "user", Content: "hello"},
	}, nil)
	if err == nil {
		t.Error("Chat() should return error when server is unreachable")
	}

	// Test with custom options too
	_, err = client.Chat([]Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "test"},
	}, &ChatOptions{MaxTokens: 100, Temperature: 0.5})
	if err == nil {
		t.Error("Chat() with options should still return error when server is unreachable")
	}
}

// TestOpenAIClient_ServerError verifies Chat() handles non-200 responses.
func TestOpenAIClient_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	client := NewOpenAIClient(OpenAIClientConfig{
		Endpoint: server.URL,
		Model:    "test-model",
		Backend:  "test",
	})

	_, err := client.Chat([]Message{
		{Role: "user", Content: "hello"},
	}, nil)
	if err == nil {
		t.Error("Chat() should return error on 500 response")
	}
}

// TestOpenAIClient_EmptyChoices verifies Chat() handles empty choices array.
func TestOpenAIClient_EmptyChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(OpenAIClientConfig{
		Endpoint: server.URL,
		Model:    "test-model",
		Backend:  "test",
	})

	_, err := client.Chat([]Message{
		{Role: "user", Content: "hello"},
	}, nil)
	if err == nil {
		t.Error("Chat() should return error when choices is empty")
	}
}

// ══════════════════════════════════════════════════════════════════════════════
// AnthropicClient
// ══════════════════════════════════════════════════════════════════════════════

func TestAnthropicClient_Chat(t *testing.T) {
	var receivedBody anthropicRequest
	var receivedAPIKey, receivedVersion string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		receivedAPIKey = r.Header.Get("x-api-key")
		receivedVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "cloud reply"}},
		})
	}))
	defer server.Close()

	client := NewAnthropicClient(AnthropicClientConfig{
		Endpoint: server.URL,
		APIKey:   "test-key",
		Model:    "claude-haiku-4-5",
	})

	reply, err := client.Chat([]Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	}, &ChatOptions{MaxTokens: 512, Temperature: 0.3})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if reply != "cloud reply" {
		t.Errorf("reply = %q, want %q", reply, "cloud reply")
	}

	if receivedAPIKey != "test-key" {
		t.Errorf("x-api-key header = %q, want %q", receivedAPIKey, "test-key")
	}
	if receivedVersion != anthropicAPIVersion {
		t.Errorf("anthropic-version header = %q, want %q", receivedVersion, anthropicAPIVersion)
	}
	// The system-role message must be lifted into the top-level `system`
	// field, not sent through in `messages` — Anthropic rejects role
	// "system" inside the messages array.
	if receivedBody.System != "you are helpful" {
		t.Errorf("system field = %q, want %q", receivedBody.System, "you are helpful")
	}
	if len(receivedBody.Messages) != 1 || receivedBody.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want exactly one user message", receivedBody.Messages)
	}
	if receivedBody.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512", receivedBody.MaxTokens)
	}

	if client.ModelName() != "claude-haiku-4-5" {
		t.Errorf("ModelName() = %q, want %q", client.ModelName(), "claude-haiku-4-5")
	}
	if client.Backend() != "anthropic" {
		t.Errorf("Backend() = %q, want %q", client.Backend(), "anthropic")
	}
}

func TestAnthropicClient_DefaultEndpoint(t *testing.T) {
	client := NewAnthropicClient(AnthropicClientConfig{APIKey: "k", Model: "m"})
	if client.endpoint != "https://api.anthropic.com" {
		t.Errorf("default endpoint = %q, want the public Anthropic API", client.endpoint)
	}
}

func TestAnthropicClient_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"invalid x-api-key"}}`))
	}))
	defer server.Close()

	client := NewAnthropicClient(AnthropicClientConfig{Endpoint: server.URL, APIKey: "bad", Model: "m"})
	_, err := client.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("expected error on 401 response")
	}
}

func TestAnthropicClient_NoTextContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(anthropicResponse{})
	}))
	defer server.Close()

	client := NewAnthropicClient(AnthropicClientConfig{Endpoint: server.URL, APIKey: "k", Model: "m"})
	_, err := client.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("expected error when response has no text content block")
	}
}

func TestAnthropicClient_Unavailable(t *testing.T) {
	client := NewAnthropicClient(AnthropicClientConfig{
		Endpoint: "http://127.0.0.1:19999",
		APIKey:   "k",
		Model:    "m",
	})
	if client.Available() {
		t.Error("expected client to be unavailable when nothing is listening")
	}
}
