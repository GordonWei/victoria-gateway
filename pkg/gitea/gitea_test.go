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
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Issue{Number: 42, State: "open", Title: "test"})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "admin", Repo: "victoria-gateway-incidents"})
	n, err := c.CreateIssue(context.Background(), "test title", "test body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if n != 42 {
		t.Errorf("issue number = %d, want 42", n)
	}
	if receivedPath != "/api/v1/repos/admin/victoria-gateway-incidents/issues" {
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
		_, _ = w.Write([]byte(`{"message":"token does not have required scope"}`))
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "bad", Owner: "admin", Repo: "r"})
	if _, err := c.CreateIssue(context.Background(), "t", "b"); err == nil {
		t.Error("expected error on 403 response")
	}
}

func TestGetIssue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/admin/victoria-gateway-incidents/issues/7" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(Issue{Number: 7, State: "closed", Title: "InstanceDown"})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "admin", Repo: "victoria-gateway-incidents"})
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
		_ = json.NewEncoder(w).Encode([]comment{
			{Body: "investigating"},
			{Body: "舊測試機殘留 target，已下線"},
		})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "admin", Repo: "r"})
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
		_ = json.NewEncoder(w).Encode([]comment{})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "admin", Repo: "r"})
	last, err := c.LastComment(context.Background(), 7)
	if err != nil {
		t.Fatalf("LastComment: %v", err)
	}
	if last != "" {
		t.Errorf("expected empty string when there are no comments, got %q", last)
	}
}

func TestCloseWithComment(t *testing.T) {
	var calls []struct {
		Method string
		Path   string
		Body   map[string]string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, struct {
			Method string
			Path   string
			Body   map[string]string
		}{r.Method, r.URL.Path, body})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "admin", Repo: "victoria-gateway-incidents"})
	if err := c.CloseWithComment(context.Background(), 9, "root cause: disk full, cleared logs"); err != nil {
		t.Fatalf("CloseWithComment: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 requests (post comment, then close), got %d: %+v", len(calls), calls)
	}
	if calls[0].Method != http.MethodPost || calls[0].Path != "/api/v1/repos/admin/victoria-gateway-incidents/issues/9/comments" {
		t.Errorf("first call = %+v, want POST to the comments endpoint", calls[0])
	}
	if calls[0].Body["body"] != "root cause: disk full, cleared logs" {
		t.Errorf("comment body = %q", calls[0].Body["body"])
	}
	if calls[1].Method != http.MethodPatch || calls[1].Path != "/api/v1/repos/admin/victoria-gateway-incidents/issues/9" {
		t.Errorf("second call = %+v, want PATCH to the issue endpoint", calls[1])
	}
	if calls[1].Body["state"] != "closed" {
		t.Errorf("expected the patch to set state=closed, got %+v", calls[1].Body)
	}
}

func TestCloseWithComment_CommentPostFails_NeverCallsClose(t *testing.T) {
	var closeCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			closeCalled = true
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"token does not have required scope"}`))
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "bad", Owner: "admin", Repo: "r"})
	if err := c.CloseWithComment(context.Background(), 9, "fixed"); err == nil {
		t.Error("expected an error when posting the comment fails")
	}
	if closeCalled {
		t.Error("close must not be attempted when the comment post itself failed")
	}
}

func TestCloseWithComment_ClosePatchFails_CommentAlreadyPosted(t *testing.T) {
	var postedComment bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postedComment = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient(ClientConfig{Endpoint: server.URL, Token: "tok", Owner: "admin", Repo: "r"})
	err := c.CloseWithComment(context.Background(), 9, "fixed")
	if err == nil {
		t.Fatal("expected an error when the close patch fails")
	}
	if !postedComment {
		t.Error("the comment should have been posted before the close attempt")
	}
	if !strings.Contains(err.Error(), "comment already posted") {
		t.Errorf("error should note the comment already landed so a retry doesn't need to re-post it, got: %v", err)
	}
}
