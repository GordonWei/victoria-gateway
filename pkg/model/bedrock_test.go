package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// newTestBedrockClient points a BedrockClient at a local httptest server
// with static fake credentials, so tests never touch the real AWS
// credential chain (env vars, ~/.aws/credentials, IMDS, ...) or make a
// network call outside the test process.
func newTestBedrockClient(endpoint string) *BedrockClient {
	return NewBedrockClient(BedrockClientConfig{
		Region:   "us-east-1",
		Model:    "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Endpoint: endpoint,
		credentialsProvider: credentials.NewStaticCredentialsProvider(
			"AKIAFAKEFAKEFAKEFAKE", "fakesecretfakesecretfakesecretfakesecret", "",
		),
	})
}

func TestBedrockClient_Chat(t *testing.T) {
	var receivedBody bedrockAnthropicRequest
	var receivedPath, receivedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "bedrock reply"}},
		})
	}))
	defer server.Close()

	client := newTestBedrockClient(server.URL)

	reply, err := client.Chat([]Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	}, &ChatOptions{MaxTokens: 512, Temperature: 0.3})
	if err != nil {
		t.Fatalf("chat failed: %v", err)
	}
	if reply != "bedrock reply" {
		t.Errorf("reply = %q, want %q", reply, "bedrock reply")
	}

	if receivedContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", receivedContentType)
	}
	// InvokeModel's path embeds the model id — confirms ModelId was sent,
	// not just that some request arrived.
	if want := "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke"; receivedPath != want {
		t.Errorf("path = %q, want %q", receivedPath, want)
	}
	if receivedBody.AnthropicVersion != bedrockAnthropicVersion {
		t.Errorf("anthropic_version = %q, want %q", receivedBody.AnthropicVersion, bedrockAnthropicVersion)
	}
	if receivedBody.System != "you are helpful" {
		t.Errorf("system field = %q, want %q", receivedBody.System, "you are helpful")
	}
	if len(receivedBody.Messages) != 1 || receivedBody.Messages[0].Role != "user" {
		t.Errorf("messages = %+v, want exactly one user message", receivedBody.Messages)
	}
	if receivedBody.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512", receivedBody.MaxTokens)
	}

	if client.ModelName() != "anthropic.claude-3-5-sonnet-20241022-v2:0" {
		t.Errorf("ModelName() = %q, want the configured model id", client.ModelName())
	}
	if client.Backend() != "bedrock" {
		t.Errorf("Backend() = %q, want %q", client.Backend(), "bedrock")
	}
}

func TestBedrockClient_NoTextContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{})
	}))
	defer server.Close()

	client := newTestBedrockClient(server.URL)
	_, err := client.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("expected error when response has no text content block")
	}
}

func TestBedrockClient_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"access denied"}`))
	}))
	defer server.Close()

	client := newTestBedrockClient(server.URL)
	_, err := client.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Error("expected error on 403 response")
	}
}

func TestBedrockClient_Available(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "text", Text: "pong"}},
		})
	}))
	defer server.Close()

	client := newTestBedrockClient(server.URL)
	if !client.Available() {
		t.Error("expected client to be available")
	}
}

func TestBedrockClient_Available_CountsAuthErrorAsReachable(t *testing.T) {
	// Per Available()'s doc comment: an error response that still made a
	// round trip to the service (here, a 403) counts as "available" —
	// the endpoint/region is reachable even though these fake
	// credentials aren't valid for a real call.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"access denied"}`))
	}))
	defer server.Close()

	client := newTestBedrockClient(server.URL)
	if !client.Available() {
		t.Error("expected an HTTP-level error response to still count as available")
	}
}

func TestBedrockClient_Available_NetworkFailureIsUnavailable(t *testing.T) {
	client := NewBedrockClient(BedrockClientConfig{
		Region:   "us-east-1",
		Model:    "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Endpoint: "http://127.0.0.1:19999", // nothing listening here
		credentialsProvider: credentials.NewStaticCredentialsProvider(
			"AKIAFAKEFAKEFAKEFAKE", "fakesecretfakesecretfakesecretfakesecret", "",
		),
	})
	if client.Available() {
		t.Error("expected client to be unavailable when nothing is listening")
	}
}
