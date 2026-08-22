package rag

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbedder_Embed(t *testing.T) {
	var receivedModel, receivedInput, receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		receivedAuth = r.Header.Get("Authorization")
		var req embeddingRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		receivedModel = req.Model
		receivedInput = req.Input

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse{
			Data: []struct {
				Embedding []float32 `json:"embedding"`
			}{{Embedding: []float32{0.1, 0.2, 0.3}}},
		})
	}))
	defer server.Close()

	e := NewEmbedder(server.URL, "bge-m3", "")
	vec, err := e.Embed("告警：InstanceDown 主機：192.0.2.7")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Errorf("vec = %v, want [0.1 0.2 0.3]", vec)
	}
	if receivedAuth != "" {
		t.Errorf("Authorization header = %q, want empty when no apiKey is configured", receivedAuth)
	}
	if receivedModel != "bge-m3" {
		t.Errorf("model sent = %q, want %q", receivedModel, "bge-m3")
	}
	if receivedInput != "告警：InstanceDown 主機：192.0.2.7" {
		t.Errorf("input sent = %q", receivedInput)
	}
}

func TestEmbedder_Embed_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse{})
	}))
	defer server.Close()

	e := NewEmbedder(server.URL, "bge-m3", "")
	if _, err := e.Embed("x"); err == nil {
		t.Error("expected error when the endpoint returns no embedding data")
	}
}

func TestEmbedder_Embed_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer server.Close()

	e := NewEmbedder(server.URL, "bge-m3", "")
	if _, err := e.Embed("x"); err == nil {
		t.Error("expected error on 500 response")
	}
}

func TestEmbedder_Embed_Unreachable(t *testing.T) {
	e := NewEmbedder("http://127.0.0.1:19999", "bge-m3", "")
	if _, err := e.Embed("x"); err == nil {
		t.Error("expected error when nothing is listening")
	}
}

func TestEmbedder_Embed_SendsAPIKey(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embeddingResponse{
			Data: []struct {
				Embedding []float32 `json:"embedding"`
			}{{Embedding: []float32{0.1}}},
		})
	}))
	defer server.Close()

	e := NewEmbedder(server.URL, "bge-m3", "sk-real-secret")
	if _, err := e.Embed("x"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if want := "Bearer sk-real-secret"; receivedAuth != want {
		t.Errorf("Authorization header = %q, want %q", receivedAuth, want)
	}
}
