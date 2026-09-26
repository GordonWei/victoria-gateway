package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/gordonwei/victoria-gateway/pkg/gcpauth"
)

// VertexAIClient calls Gemini through Vertex AI's generateContent API
// instead of the Gemini Developer API GeminiClient uses. The request and
// response bodies are the same shape; what differs is who is billed and
// how the call is authorized. Vertex AI bills the GCP project and checks
// IAM (roles/aiplatform.user on the project), so there is no API key:
// credentials come from Application Default Credentials — GOOGLE_
// APPLICATION_CREDENTIALS, `gcloud auth application-default login`, or
// the GCE/GKE/Cloud Run metadata server — the same chain pkg/gcplogging
// uses for Cloud Logging.
//
// Endpoint shape, from the Vertex AI "Locations" page
// (docs.cloud.google.com/vertex-ai/generative-ai/docs/learn/locations,
// checked 2026-09-27):
//
//	regional:     https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent
//	global:       https://aiplatform.googleapis.com/v1/projects/{project}/locations/global/publishers/google/models/{model}:generateContent
//	multi-region: https://aiplatform.{us|eu}.rep.googleapis.com/v1/projects/{project}/locations/{us|eu}/publishers/google/models/{model}:generateContent
//
// The default location is "global": the same page says it "can improve
// overall availability while reducing resource exhausted (429) errors",
// and newer Gemini models tend to reach it first. The trade-off it spells
// out is that you "can't control or know which region your ML processing
// requests are sent to" — set Location to a region (or "us"/"eu") when
// that matters.
type VertexAIClient struct {
	project  string
	location string
	model    string
	baseURL  string
	client   *http.Client
	// tokenSource is ADC's token source in production. Tests set it
	// directly so they never touch the real credential chain.
	tokenSource oauth2.TokenSource
	// loadErr carries a FindDefaultCredentials failure through to Chat,
	// the same construction-never-fails shape as BedrockClient and
	// gcplogging.Client.
	loadErr error
}

type VertexAIClientConfig struct {
	Project  string // GCP project ID billed for the call
	Location string // "global" (default), a region such as "us-central1", or "us"/"eu"
	Model    string // e.g. "gemini-2.5-flash"
	Timeout  time.Duration
	// Endpoint overrides the base URL derived from Location (scheme and
	// host only, no path). Leave empty for real use; it exists for tests
	// and for a Private Service Connect endpoint.
	Endpoint string
}

// VertexAIDefaultLocation is VertexAIClientConfig.Location's default.
const VertexAIDefaultLocation = "global"

// cloudPlatformScope is the OAuth2 scope both Vertex AI generateContent
// and the Gemini Cloud Assist API list in their discovery documents.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// vertexAIBaseURL returns the service endpoint for a location, per the
// three shapes in VertexAIClient's doc comment.
func vertexAIBaseURL(location string) string {
	switch location {
	case "global":
		return "https://aiplatform.googleapis.com"
	case "us", "eu":
		return "https://aiplatform." + location + ".rep.googleapis.com"
	default:
		return "https://" + location + "-aiplatform.googleapis.com"
	}
}

func NewVertexAIClient(cfg VertexAIClientConfig) *VertexAIClient {
	c := newVertexAIClient(cfg)
	creds, err := gcpauth.FindDefaultCredentials(cloudPlatformScope)
	if err != nil {
		c.loadErr = err
	} else {
		c.tokenSource = creds.TokenSource
	}
	return c
}

// newVertexAIClient builds everything but the credentials, so tests can
// plug in a fake token source.
func newVertexAIClient(cfg VertexAIClientConfig) *VertexAIClient {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	location := cfg.Location
	if location == "" {
		location = VertexAIDefaultLocation
	}
	baseURL := strings.TrimRight(cfg.Endpoint, "/")
	if baseURL == "" {
		baseURL = vertexAIBaseURL(location)
	}
	return &VertexAIClient{
		project:  cfg.Project,
		location: location,
		model:    cfg.Model,
		baseURL:  baseURL,
		client:   &http.Client{Timeout: timeout},
	}
}

func (c *VertexAIClient) modelPath() string {
	return fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models/%s", c.project, c.location, c.model)
}

// authorize adds the ADC bearer token to req.
func (c *VertexAIClient) authorize(req *http.Request) error {
	if c.loadErr != nil {
		return fmt.Errorf("vertex-ai: load Application Default Credentials: %w", c.loadErr)
	}
	tok, err := c.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("vertex-ai: get access token: %w", err)
	}
	tok.SetAuthHeader(req)
	return nil
}

// vertexAIResponse is generateContent's reply. Parts can carry
// thought: true (a thinking model's reasoning summary, only sent when
// asked for); those are skipped so only the answer is returned.
type vertexAIResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason        string `json:"blockReason"`
		BlockReasonMessage string `json:"blockReasonMessage"`
	} `json:"promptFeedback"`
}

// Chat sends one generateContent request. A non-200 answer comes back as
// *HTTPStatusError, so a 5xx or 429 (quota) moves an escalation on to
// the next target and shows up as "unavailable" the same way it does for
// the OpenAI-compatible clients.
func (c *VertexAIClient) Chat(messages []Message, opts *ChatOptions) (string, error) {
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
	system, contents := toGeminiContents(messages)
	body, err := json.Marshal(geminiRequest{
		Contents:          contents,
		SystemInstruction: system,
		GenerationConfig:  geminiGenerationConfig{MaxOutputTokens: maxTokens, Temperature: temperature},
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+c.modelPath()+":generateContent", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authorize(req); err != nil {
		return "", err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to vertex-ai failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
		return "", &HTTPStatusError{Backend: "vertex-ai", StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(respBody))}
	}

	var result vertexAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(result.Candidates) == 0 {
		if pf := result.PromptFeedback; pf != nil && pf.BlockReason != "" {
			return "", fmt.Errorf("vertex-ai blocked the prompt: %s %s", pf.BlockReason, pf.BlockReasonMessage)
		}
		return "", fmt.Errorf("vertex-ai returned no candidates")
	}
	var text strings.Builder
	for _, p := range result.Candidates[0].Content.Parts {
		if !p.Thought {
			text.WriteString(p.Text)
		}
	}
	if text.Len() == 0 {
		// An empty reply with finishReason MAX_TOKENS means the thinking
		// budget used up maxOutputTokens; returning "" (not an error)
		// lets the summarizer's empty-reply retry handle it like it does
		// for a local reasoning model.
		if fr := result.Candidates[0].FinishReason; fr != "" && fr != "STOP" && fr != "MAX_TOKENS" {
			return "", fmt.Errorf("vertex-ai returned no text (finishReason %s)", fr)
		}
	}
	return text.String(), nil
}

// Available fetches the publisher model's metadata
// (GET v1/publishers/google/models/{model}): no tokens are spent, and a
// 200 confirms the credentials work and the model name exists.
func (c *VertexAIClient) Available() bool {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/v1/publishers/google/models/"+c.model, nil)
	if err != nil {
		return false
	}
	if err := c.authorize(req); err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *VertexAIClient) ModelName() string {
	return c.model
}

func (c *VertexAIClient) Backend() string {
	return "vertex-ai"
}
