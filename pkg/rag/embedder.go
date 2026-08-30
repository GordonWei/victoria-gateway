// Package rag retrieves past incidents similar to a new alert (and lets an
// operator record what a past alert actually turned out to be) so the
// summarizer prompt can be grounded in real prior experience instead of
// starting from nothing every time. It's entirely optional — see
// config.RAGConfig.Enabled — and victoria-gateway runs exactly as it did
// before this package existed when it's off.
package rag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Embedder calls an OpenAI-compatible /v1/embeddings endpoint (LM Studio,
// Ollama, vLLM) to turn text into a vector. It's the same shape of client
// as pkg/model.OpenAIClient but embeddings are a different API surface
// (no chat/completions semantics), so it isn't reused directly.
type Embedder struct {
	endpoint string
	model    string
	apiKey   string // empty for unauthenticated local servers
	client   *http.Client
}

// NewEmbedder builds an Embedder. apiKey is optional — leave it empty for
// an unauthenticated local server (LM Studio, Ollama); set it if
// embedding_endpoint points at a real cloud endpoint or a proxy (e.g. a
// LiteLLM gateway shared with summarizer) that requires one.
func NewEmbedder(endpoint, model, apiKey string) *Embedder {
	return &Embedder{
		endpoint: endpoint,
		model:    model,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// retrySleep is swapped out by tests so retry paths don't wait real
// backoffs.
var retrySleep = time.Sleep

// Embed returns the embedding vector for one piece of text. A transient
// failure (transport error or 5xx) is retried once after a short pause —
// an embed failure either drops past-incident context from the prompt or
// (worse) silently skips capturing an incident into RAG, and a single
// network blip shouldn't be what decides that.
func (e *Embedder) Embed(text string) ([]float32, error) {
	vec, retryable, err := e.embedOnce(text)
	if err != nil && retryable {
		retrySleep(500 * time.Millisecond)
		vec, _, err = e.embedOnce(text)
	}
	return vec, err
}

func (e *Embedder) embedOnce(text string) (vec []float32, retryable bool, err error) {
	reqBody, err := json.Marshal(embeddingRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, false, fmt.Errorf("marshal embedding request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, e.endpoint+"/v1/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, false, fmt.Errorf("build embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("embedding request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode >= 500, fmt.Errorf("embedding endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, false, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, false, fmt.Errorf("embedding endpoint returned no vector")
	}
	return result.Data[0].Embedding, false, nil
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}
