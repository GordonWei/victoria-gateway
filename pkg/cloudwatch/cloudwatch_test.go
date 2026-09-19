package cloudwatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
)

// newTestClient points a Client at a local httptest server with static
// fake credentials, mirroring pkg/model.newTestBedrockClient — tests
// never touch the real AWS credential chain or make a network call
// outside the test process.
func newTestClient(t *testing.T, endpoint string, opts ...func(*ClientConfig)) *Client {
	t.Helper()
	cfg := ClientConfig{
		Region:        "us-east-1",
		LogGroupNames: []string{"/test/log-group"},
		Endpoint:      endpoint,
		credentialsProvider: credentials.NewStaticCredentialsProvider(
			"AKIAFAKEFAKEFAKEFAKE", "fakesecretfakesecretfakesecretfakesecret", "",
		),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	c := NewClient(cfg)
	c.pollInterval = time.Millisecond // don't actually wait real seconds between polls in tests
	return c
}

// awsTarget extracts the "Logs_20140328.<Action>" suffix from the
// X-Amz-Target header the SDK sets on every request, so a fake server can
// tell StartQuery and GetQueryResults requests apart.
func awsTarget(r *http.Request) string {
	target := r.Header.Get("X-Amz-Target")
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

func TestQueryRange_PollsUntilCompleteAndParsesResults(t *testing.T) {
	var startQueryCalls, getResultsCalls int
	var receivedQueryString string
	var receivedStart, receivedEnd int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch awsTarget(r) {
		case "StartQuery":
			startQueryCalls++
			var body struct {
				QueryString string `json:"queryString"`
				StartTime   int64  `json:"startTime"`
				EndTime     int64  `json:"endTime"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			receivedQueryString = body.QueryString
			receivedStart, receivedEnd = body.StartTime, body.EndTime
			_ = json.NewEncoder(w).Encode(map[string]string{"queryId": "test-query-id"})
		case "GetQueryResults":
			getResultsCalls++
			// First poll: still running. Second poll: complete with results —
			// exercises the poll loop actually looping, not just handling an
			// immediate Complete.
			if getResultsCalls == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "Running", "results": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "Complete",
				"results": [][]map[string]string{
					{
						{"field": "@timestamp", "value": "2024-03-26 06:17:25.758"},
						{"field": "@message", "value": "disk at 97%"},
					},
					{
						// A row missing @message (e.g. a query that only
						// selected @timestamp) must be skipped, not turned
						// into an empty-line entry.
						{"field": "@timestamp", "value": "2024-03-26 06:17:26.000"},
					},
				},
			})
		default:
			t.Errorf("unexpected AWS action: %q", r.Header.Get("X-Amz-Target"))
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	start := time.Date(2024, 3, 26, 6, 0, 0, 0, time.UTC)
	end := time.Date(2024, 3, 26, 7, 0, 0, 0, time.UTC)

	entries, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "172.16.100.6"}, start, end, 100)
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}

	if startQueryCalls != 1 {
		t.Errorf("StartQuery called %d times, want 1", startQueryCalls)
	}
	if getResultsCalls != 2 {
		t.Errorf("GetQueryResults called %d times, want 2 (poll loop should have looped once)", getResultsCalls)
	}
	if !strings.Contains(receivedQueryString, "172.16.100.6") {
		t.Errorf("query string = %q, want it to contain the identity term", receivedQueryString)
	}
	if receivedStart != start.Unix() || receivedEnd != end.Unix() {
		t.Errorf("start/end = %d/%d, want %d/%d", receivedStart, receivedEnd, start.Unix(), end.Unix())
	}

	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1 (the row with no @message must be skipped)", entries)
	}
	if entries[0].Line != "disk at 97%" {
		t.Errorf("entries[0].Line = %q, want %q", entries[0].Line, "disk at 97%")
	}
	wantTime := time.Date(2024, 3, 26, 6, 17, 25, 758000000, time.UTC)
	if !entries[0].Timestamp.Equal(wantTime) {
		t.Errorf("entries[0].Timestamp = %v, want %v", entries[0].Timestamp, wantTime)
	}
}

func TestQueryRange_TerminalFailedStatusIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch awsTarget(r) {
		case "StartQuery":
			_ = json.NewEncoder(w).Encode(map[string]string{"queryId": "test-query-id"})
		case "GetQueryResults":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "Failed"})
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "h"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil {
		t.Fatal("expected an error when the query ends with status Failed")
	}
}

func TestQueryRange_StartQueryErrorPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"__type":  "ResourceNotFoundException",
			"message": "Log group '/test/log-group' does not exist",
		})
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "h"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil {
		t.Fatal("expected an error when StartQuery itself fails")
	}
}

func TestQueryRange_TimeoutWhileRunning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch awsTarget(r) {
		case "StartQuery":
			_ = json.NewEncoder(w).Encode(map[string]string{"queryId": "test-query-id"})
		case "GetQueryResults":
			// Never completes — exercises the ctx-deadline branch of the
			// poll loop rather than an infinite test hang.
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "Running", "results": []any{}})
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, func(c *ClientConfig) { c.Timeout = 10 * time.Millisecond })
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "h"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil {
		t.Fatal("expected a timeout error when the query never leaves Running")
	}
}

func TestQueryRange_CustomQueryTemplate(t *testing.T) {
	var receivedQueryString string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch awsTarget(r) {
		case "StartQuery":
			var body struct {
				QueryString string `json:"queryString"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			receivedQueryString = body.QueryString
			_ = json.NewEncoder(w).Encode(map[string]string{"queryId": "q"})
		case "GetQueryResults":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "Complete", "results": [][]map[string]string{}})
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, func(c *ClientConfig) {
		c.QueryTemplate = `fields @timestamp, @message | filter host = "{{TERM}}"`
	})
	_, err := client.QueryRange(context.Background(), aiops.LogIdentity{Host: "web-01"}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatalf("QueryRange failed: %v", err)
	}
	if want := `filter host = "web-01"`; !strings.Contains(receivedQueryString, want) {
		t.Errorf("query string = %q, want it to contain the custom template with the term substituted (%q)", receivedQueryString, want)
	}
}
