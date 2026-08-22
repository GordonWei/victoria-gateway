package gitea

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateIssue(t *testing.T) {
	var receivedPath, receivedAuth string
	var receivedBody map[string]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Issue{Number: 42, State: "open", Title: "test"})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "ops-team", Repo: "victoria-gateway-incidents"})
	n, err := c.CreateIssue(context.Background(), "test title", "test body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if n != 42 {
		t.Errorf("issue number = %d, want 42", n)
	}
	if receivedPath != "/api/v1/repos/ops-team/victoria-gateway-incidents/issues" {
		t.Errorf("path = %q", receivedPath)
	}
	if receivedAuth != "token tok" {
		t.Errorf("Authorization = %q, want %q", receivedAuth, "token tok")
	}
	if receivedBody["title"] != "test title" || receivedBody["body"] != "test body" {
		t.Errorf("request body = %+v", receivedBody)
	}
}

func TestCreateIssue_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"token does not have required scope"}`))
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "bad", Owner: "ops-team", Repo: "r"})
	if _, err := c.CreateIssue(context.Background(), "t", "b"); err == nil {
		t.Error("expected error on 403 response")
	}
}

func TestGetIssue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/ops-team/victoria-gateway-incidents/issues/7" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(Issue{Number: 7, State: "closed", Title: "InstanceDown"})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "ops-team", Repo: "victoria-gateway-incidents"})
	issue, err := c.GetIssue(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.State != "closed" {
		t.Errorf("state = %q, want %q", issue.State, "closed")
	}
}

func TestLastComment(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode([]comment{
			{Body: "investigating"},
			{Body: "舊測試機殘留 target，已下線"},
		})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "ops-team", Repo: "r"})
	last, err := c.LastComment(context.Background(), 7)
	if err != nil {
		t.Fatalf("LastComment: %v", err)
	}
	if last != "舊測試機殘留 target，已下線" {
		t.Errorf("last comment = %q", last)
	}
	if !strings.Contains(gotQuery, "limit=200") {
		t.Errorf("query = %q, want it to contain \"limit=200\" (an issue with more comments than Gitea's default page size would otherwise return the wrong one)", gotQuery)
	}
}

func TestLastComment_None(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]comment{})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "ops-team", Repo: "r"})
	last, err := c.LastComment(context.Background(), 7)
	if err != nil {
		t.Fatalf("LastComment: %v", err)
	}
	if last != "" {
		t.Errorf("expected empty string when there are no comments, got %q", last)
	}
}
