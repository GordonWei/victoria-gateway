package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateIssue(t *testing.T) {
	var receivedPath, receivedAuth, receivedAccept string
	var receivedBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		receivedAccept = r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issue{Number: 42, State: "open"})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "ghp_tok", Owner: "gordon", Repo: "victoria-gateway-incidents"})
	n, err := c.CreateIssue(context.Background(), "test title", "test body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if n != 42 {
		t.Errorf("issue number = %d, want 42", n)
	}
	if receivedPath != "/repos/gordon/victoria-gateway-incidents/issues" {
		t.Errorf("path = %q", receivedPath)
	}
	if receivedAuth != "Bearer ghp_tok" {
		t.Errorf("Authorization = %q, want %q", receivedAuth, "Bearer ghp_tok")
	}
	if receivedAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", receivedAccept)
	}
	if receivedBody["title"] != "test title" || receivedBody["body"] != "test body" {
		t.Errorf("request body = %+v", receivedBody)
	}
}

func TestCreateIssue_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "bad", Owner: "gordon", Repo: "r"})
	if _, err := c.CreateIssue(context.Background(), "t", "b"); err == nil {
		t.Error("expected error on 403 response")
	}
}

func TestIssueState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/gordon/victoria-gateway-incidents/issues/7" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(issue{Number: 7, State: "closed"})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "gordon", Repo: "victoria-gateway-incidents"})
	state, err := c.IssueState(context.Background(), 7)
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if state != "closed" {
		t.Errorf("state = %q, want %q", state, "closed")
	}
}

func TestLastComment(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		// A real GitHub server given sort=created&direction=desc&per_page=1
		// would return exactly one comment, the newest — the mock does the
		// same rather than returning a full list for LastComment to pick
		// from, so this test exercises the actual contract being relied on.
		_ = json.NewEncoder(w).Encode([]comment{
			{Body: "舊測試機殘留 target，已下線"},
		})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "gordon", Repo: "r"})
	last, err := c.LastComment(context.Background(), 7)
	if err != nil {
		t.Fatalf("LastComment: %v", err)
	}
	if last != "舊測試機殘留 target，已下線" {
		t.Errorf("last comment = %q", last)
	}
	for _, want := range []string{"sort=created", "direction=desc", "per_page=1"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query = %q, want it to contain %q (an issue with >30 comments would otherwise return the wrong one)", gotQuery, want)
		}
	}
}

func TestLastComment_None(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]comment{})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "gordon", Repo: "r"})
	last, err := c.LastComment(context.Background(), 7)
	if err != nil {
		t.Fatalf("LastComment: %v", err)
	}
	if last != "" {
		t.Errorf("expected empty string when there are no comments, got %q", last)
	}
}

func TestNewClient_DefaultEndpoint(t *testing.T) {
	c := NewClient(ClientConfig{Token: "t", Owner: "o", Repo: "r"})
	if c.endpoint != "https://api.github.com" {
		t.Errorf("default endpoint = %q, want the public GitHub API", c.endpoint)
	}
}
