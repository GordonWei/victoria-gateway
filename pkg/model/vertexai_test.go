package model

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// fakeTokenSource stands in for ADC so no test touches the real
// credential chain.
type fakeTokenSource struct {
	token string
	err   error
}

func (f fakeTokenSource) Token() (*oauth2.Token, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &oauth2.Token{AccessToken: f.token, TokenType: "Bearer"}, nil
}

func newTestVertexAIClient(url string) *VertexAIClient {
	c := newVertexAIClient(VertexAIClientConfig{Project: "p1", Location: "us-central1", Model: "gemini-2.5-flash", Endpoint: url})
	c.tokenSource = fakeTokenSource{token: "fake-token"}
	return c
}

func TestVertexAIBaseURL(t *testing.T) {
	cases := map[string]string{
		"global":      "https://aiplatform.googleapis.com",
		"us-central1": "https://us-central1-aiplatform.googleapis.com",
		"asia-east1":  "https://asia-east1-aiplatform.googleapis.com",
		"us":          "https://aiplatform.us.rep.googleapis.com",
		"eu":          "https://aiplatform.eu.rep.googleapis.com",
	}
	for loc, want := range cases {
		if got := vertexAIBaseURL(loc); got != want {
			t.Errorf("vertexAIBaseURL(%q) = %q, want %q", loc, got, want)
		}
	}
	c := newVertexAIClient(VertexAIClientConfig{Project: "p", Model: "m"})
	if c.location != "global" || c.baseURL != "https://aiplatform.googleapis.com" {
		t.Errorf("default location/baseURL = %q/%q, want global/https://aiplatform.googleapis.com", c.location, c.baseURL)
	}
}

func TestVertexAIClient_Chat(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody geminiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[
			{"text":"thinking about it","thought":true},
			{"text":"disk "},{"text":"full"}]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()

	reply, err := newTestVertexAIClient(srv.URL).Chat([]Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "why?"},
	}, &ChatOptions{MaxTokens: 256, Temperature: 0.2})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply != "disk full" {
		t.Errorf("reply = %q, want thought parts skipped and the rest joined", reply)
	}
	if want := "/v1/projects/p1/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer fake-token" {
		t.Errorf("Authorization = %q, want the ADC token", gotAuth)
	}
	if gotBody.SystemInstruction == nil || gotBody.SystemInstruction.Parts[0].Text != "be brief" {
		t.Errorf("systemInstruction = %+v, want the system message", gotBody.SystemInstruction)
	}
	if len(gotBody.Contents) != 1 || gotBody.Contents[0].Role != "user" {
		t.Errorf("contents = %+v, want one user turn", gotBody.Contents)
	}
	if gotBody.GenerationConfig.MaxOutputTokens != 256 {
		t.Errorf("maxOutputTokens = %d, want 256", gotBody.GenerationConfig.MaxOutputTokens)
	}
}

// 5xx and 429 must come back as *HTTPStatusError so the escalation path
// can tell "Vertex is down / out of quota" from "our request is wrong".
func TestVertexAIClient_ChatHTTPStatusError(t *testing.T) {
	for _, code := range []int{429, 500, 503, 403, 404} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"status":"X"}}`, code)
		}))
		_, err := newTestVertexAIClient(srv.URL).Chat([]Message{{Role: "user", Content: "hi"}}, nil)
		srv.Close()
		var se *HTTPStatusError
		if !errors.As(err, &se) || se.StatusCode != code || se.Backend != "vertex-ai" {
			t.Errorf("status %d: err = %v, want *HTTPStatusError with that code", code, err)
		}
	}
}

func TestVertexAIClient_ChatBlockedAndEmpty(t *testing.T) {
	cases := []struct {
		body    string
		wantErr string
		want    string
	}{
		{`{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"nope"}}`, "blocked the prompt: SAFETY", ""},
		{`{"candidates":[]}`, "no candidates", ""},
		{`{"candidates":[{"content":{"parts":[]},"finishReason":"RECITATION"}]}`, "finishReason RECITATION", ""},
		// Thinking used the whole budget: empty, no error, so the
		// summarizer's empty-reply retry gets a chance.
		{`{"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}]}`, "", ""},
		{`not json`, "decode response", ""},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(tc.body))
		}))
		got, err := newTestVertexAIClient(srv.URL).Chat([]Message{{Role: "user", Content: "hi"}}, nil)
		srv.Close()
		if tc.wantErr == "" {
			if err != nil || got != tc.want {
				t.Errorf("body %s: got (%q, %v), want (%q, nil)", tc.body, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("body %s: err = %v, want containing %q", tc.body, err, tc.wantErr)
		}
	}
}

func TestVertexAIClient_CredentialErrorsSkipTheRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()

	c := newTestVertexAIClient(srv.URL)
	c.tokenSource = fakeTokenSource{err: errors.New("refresh failed")}
	if _, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil); err == nil || !strings.Contains(err.Error(), "get access token") {
		t.Errorf("token error: err = %v", err)
	}
	c = newTestVertexAIClient(srv.URL)
	c.loadErr = errors.New("could not find default credentials")
	if _, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil); err == nil || !strings.Contains(err.Error(), "Application Default Credentials") {
		t.Errorf("loadErr: err = %v", err)
	}
	if c.Available() {
		t.Error("Available() = true with no credentials")
	}
	if calls != 0 {
		t.Errorf("server called %d times, want 0", calls)
	}
}

func TestVertexAIClient_Available(t *testing.T) {
	var gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := newTestVertexAIClient(srv.URL)
	if !c.Available() {
		t.Error("Available() = false on 200")
	}
	if gotPath != "/v1/publishers/google/models/gemini-2.5-flash" {
		t.Errorf("path = %q", gotPath)
	}
	status = http.StatusForbidden
	if c.Available() {
		t.Error("Available() = true on 403")
	}
	if c.Backend() != "vertex-ai" || c.ModelName() != "gemini-2.5-flash" {
		t.Errorf("Backend/ModelName = %q/%q", c.Backend(), c.ModelName())
	}
}

// A model name that would reshape the URL stays one escaped path
// segment, both for generateContent and for Available's metadata GET.
func TestVertexAIClient_ModelPathIsEscaped(t *testing.T) {
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.RequestURI)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newTestVertexAIClient(srv.URL)
	c.model = "../../other/models/x?alt=1"
	_, _ = c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	_ = c.Available()
	want := []string{
		"/v1/projects/p1/locations/us-central1/publishers/google/models/..%2F..%2Fother%2Fmodels%2Fx%3Falt=1:generateContent",
		"/v1/publishers/google/models/..%2F..%2Fother%2Fmodels%2Fx%3Falt=1",
	}
	if strings.Join(uris, "\n") != strings.Join(want, "\n") {
		t.Errorf("request URIs =\n%s\nwant\n%s", strings.Join(uris, "\n"), strings.Join(want, "\n"))
	}
	// The characters a real model ID uses pass through unchanged.
	c.model = "claude-opus-4@20250514"
	if got := c.modelPath(); !strings.HasSuffix(got, "/models/claude-opus-4@20250514") {
		t.Errorf("modelPath = %q", got)
	}
}
