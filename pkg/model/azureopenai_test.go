package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAzureOpenAIClient_Chat(t *testing.T) {
	var receivedBody openAIRequest
	var receivedAPIKey, receivedPath, receivedQuery string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedQuery = r.URL.RawQuery
		receivedAPIKey = r.Header.Get("api-key")
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "azure reply"}}},
		})
	}))
	defer server.Close()

	client := NewAzureOpenAIClient(AzureOpenAIClientConfig{
		Endpoint:   server.URL,
		Deployment: "prod-gpt4o",
		APIKey:     "test-key",
	})

	reply, err := client.Chat([]Message{
		{Role: "user", Content: "hello"},
	}, &ChatOptions{MaxTokens: 512, Temperature: 0.3})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if reply != "azure reply" {
		t.Errorf("reply = %q, want %q", reply, "azure reply")
	}

	if receivedPath != "/openai/v1/chat/completions" {
		t.Errorf("path = %q, want the v1 chat completions endpoint", receivedPath)
	}
	if receivedQuery != "api-version="+azureOpenAIDefaultAPIVersion {
		t.Errorf("query = %q, want api-version=%s", receivedQuery, azureOpenAIDefaultAPIVersion)
	}
	if receivedAPIKey != "test-key" {
		t.Errorf("api-key header = %q, want %q", receivedAPIKey, "test-key")
	}
	// The deployment name goes in the body's "model" field, not the URL
	// path — this is the v1 data-plane API's addressing scheme.
	if receivedBody.Model != "prod-gpt4o" {
		t.Errorf("model field = %q, want the configured deployment name", receivedBody.Model)
	}
	if receivedBody.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512", receivedBody.MaxTokens)
	}

	if client.ModelName() != "prod-gpt4o" {
		t.Errorf("ModelName() = %q, want the configured deployment name", client.ModelName())
	}
	if client.Backend() != "azure-openai" {
		t.Errorf("Backend() = %q, want %q", client.Backend(), "azure-openai")
	}
}

func TestAzureOpenAIClient_CustomAPIVersion(t *testing.T) {
	var receivedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}))
	defer server.Close()

	client := NewAzureOpenAIClient(AzureOpenAIClientConfig{
		Endpoint:   server.URL,
		Deployment: "d",
		APIKey:     "k",
		APIVersion: "2024-06-01",
	})
	if _, err := client.Chat([]Message{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if receivedQuery != "api-version=2024-06-01" {
		t.Errorf("query = %q, want the explicitly configured api-version", receivedQuery)
	}
}

func TestAzureOpenAIClient_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api-key"}}`))
	}))
	defer server.Close()

	client := NewAzureOpenAIClient(AzureOpenAIClientConfig{Endpoint: server.URL, Deployment: "d", APIKey: "bad"})
	_, err := client.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("expected error on 401 response")
	}
}

func TestAzureOpenAIClient_EmptyChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	client := NewAzureOpenAIClient(AzureOpenAIClientConfig{Endpoint: server.URL, Deployment: "d", APIKey: "k"})
	_, err := client.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("expected error when choices is empty")
	}
}

func TestAzureOpenAIClient_Unavailable(t *testing.T) {
	client := NewAzureOpenAIClient(AzureOpenAIClientConfig{
		Endpoint:   "http://127.0.0.1:19999",
		Deployment: "d",
		APIKey:     "k",
	})
	if client.Available() {
		t.Error("expected client to be unavailable when nothing is listening")
	}
}

func TestAzureOpenAIClient_Available_CountsErrorResponseAsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api-key"}}`))
	}))
	defer server.Close()

	client := NewAzureOpenAIClient(AzureOpenAIClientConfig{Endpoint: server.URL, Deployment: "d", APIKey: "bad"})
	if !client.Available() {
		t.Error("expected an HTTP-level error response to still count as available")
	}
}
