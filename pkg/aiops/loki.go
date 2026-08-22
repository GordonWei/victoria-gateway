package aiops

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client talks to a Loki instance's HTTP query API. It only needs the
// base URL (e.g. "http://192.0.2.6:3100") — no auth, because the
// on-prem Loki in this environment sits inside the private network.
type Client struct {
	endpoint   string
	httpClient *http.Client
}

// NewClient creates a Loki Client. endpoint is the base URL without
// trailing slash (e.g. "http://192.0.2.6:3100").
func NewClient(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// QueryRange fetches log entries from Loki for the given host and time
// window. The LogQL query is `{host="<host>"}` — the label name "host"
// is fixed here; if the webhook uses "instance", the caller maps it
// before calling this (see docs/_agent_handoff.md "已知落差" note).
func (c *Client) QueryRange(host string, start, end time.Time, limit int) ([]LogEntry, error) {
	query := fmt.Sprintf(`{host="%s"}`, host)

	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}

	reqURL := fmt.Sprintf("%s/loki/api/v1/query_range?%s", c.endpoint, params.Encode())

	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("loki: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("loki: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loki: HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}

	return parseLokiResponse(body)
}

// lokiResponse is the top-level Loki query_range JSON envelope.
type lokiResponse struct {
	Status string   `json:"status"`
	Data   lokiData `json:"data"`
}

type lokiData struct {
	ResultType string       `json:"resultType"`
	Result     []lokiStream `json:"result"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"` // each entry: [timestamp_ns_string, log_line]
}

func parseLokiResponse(body []byte) ([]LogEntry, error) {
	var lr lokiResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, fmt.Errorf("loki: unmarshal response: %w", err)
	}
	// A 200 HTTP status doesn't guarantee Loki actually answered the
	// query — some versions return status:"error" with 200 on query
	// timeout or an internal error. Without this check that looks
	// identical to "no logs found" and the summarizer proceeds with an
	// empty log window instead of reporting Loki itself is the problem.
	if lr.Status != "success" {
		return nil, fmt.Errorf("loki: response status is %q (expected \"success\")", lr.Status)
	}

	var entries []LogEntry
	for _, stream := range lr.Data.Result {
		for _, pair := range stream.Values {
			if len(pair) != 2 {
				continue
			}
			nsec, err := strconv.ParseInt(pair[0], 10, 64)
			if err != nil {
				continue
			}
			entries = append(entries, LogEntry{
				Timestamp: time.Unix(0, nsec),
				Line:      pair[1],
			})
		}
	}
	return entries, nil
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
