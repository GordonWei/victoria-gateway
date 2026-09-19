package gcplogging

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
)

// fakeTokenSource is an oauth2.TokenSource that never touches the real ADC
// chain (env var, ~/.config/gcloud, GCE/GKE metadata server) — tests
// construct a Client directly (bypassing NewClient's real
// google.FindDefaultCredentials call) with this instead.
type fakeTokenSource struct{ token string }

func (f fakeTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{AccessToken: f.token}, nil
}

func newTestClient(endpoint string, opts ...func(*Client)) *Client {
	c := &Client{
		projectID:      "test-project",
		filterTemplate: defaultFilterTemplate,
		endpoint:       endpoint,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		timeout:        5 * time.Second,
		tokenSource:    fakeTokenSource{token: "fake-access-token"},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func TestQueryRange_ParsesEntriesAndSendsFilter(t *testing.T) {
	var receivedAuth string
	var receivedBody entriesListRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entriesListResponse{
			Entries: []logEntry{
				{Timestamp: "2026-09-19T18:30:00.123456789Z", TextPayload: "disk at 97%"},
				{Timestamp: "2026-09-19T18:29:00Z", JSONPayload: json.RawMessage(`{"msg":"cpu high"}`)},
				{Timestamp: "2026-09-19T18:28:00Z"}, // no payload at all — must be skipped
			},
		})
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	start := time.Date(2026, 9, 19, 18, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 19, 19, 0, 0, 0, time.UTC)

	entries, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "172.16.100.7"}, start, end, 50)
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}

	if receivedAuth != "Bearer fake-access-token" {
		t.Errorf("Authorization header = %q, want Bearer fake-access-token", receivedAuth)
	}
	if len(receivedBody.ResourceNames) != 1 || receivedBody.ResourceNames[0] != "projects/test-project" {
		t.Errorf("resourceNames = %v, want [projects/test-project]", receivedBody.ResourceNames)
	}
	if !strings.Contains(receivedBody.Filter, "172.16.100.7") {
		t.Errorf("filter = %q, want it to contain the identity term", receivedBody.Filter)
	}
	if !strings.Contains(receivedBody.Filter, start.Format(time.RFC3339)) || !strings.Contains(receivedBody.Filter, end.Format(time.RFC3339)) {
		t.Errorf("filter = %q, want it to contain both the start and end timestamps", receivedBody.Filter)
	}
	if receivedBody.PageSize != 50 {
		t.Errorf("pageSize = %d, want 50", receivedBody.PageSize)
	}

	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want exactly 2 (the payload-less row must be skipped)", entries)
	}
	if entries[0].Line != "disk at 97%" {
		t.Errorf("entries[0].Line = %q, want %q", entries[0].Line, "disk at 97%")
	}
	if entries[1].Line != `{"msg":"cpu high"}` {
		t.Errorf("entries[1].Line = %q, want the raw jsonPayload text", entries[1].Line)
	}
}

func TestQueryRange_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"permission denied"}}`))
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "h"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil {
		t.Fatal("expected an error on a 403 response")
	}
}

func TestQueryRange_TokenSourceErrorPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not make an HTTP request when the token source itself fails")
	}))
	defer server.Close()

	client := newTestClient(server.URL, func(c *Client) {
		c.tokenSource = erroringTokenSource{}
	})
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "h"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil {
		t.Fatal("expected an error when the token source fails")
	}
}

type erroringTokenSource struct{}

func (erroringTokenSource) Token() (*oauth2.Token, error) {
	return nil, errTokenFailed
}

var errTokenFailed = &tokenError{"token refresh failed"}

type tokenError struct{ msg string }

func (e *tokenError) Error() string { return e.msg }

func TestQueryRange_LoadErrPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not make an HTTP request when credential loading itself failed")
	}))
	defer server.Close()

	client := newTestClient(server.URL, func(c *Client) {
		c.loadErr = errTokenFailed
	})
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "h"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil {
		t.Fatal("expected an error when loadErr is set")
	}
}

func TestQueryRange_CustomFilterTemplate(t *testing.T) {
	var receivedBody entriesListRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entriesListResponse{})
	}))
	defer server.Close()

	client := newTestClient(server.URL, func(c *Client) {
		c.filterTemplate = `jsonPayload.host="{{TERM}}"`
	})
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "web-01"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if want := `jsonPayload.host="web-01"`; !strings.Contains(receivedBody.Filter, want) {
		t.Errorf("filter = %q, want it to contain the custom template with the term substituted (%q)", receivedBody.Filter, want)
	}
}
