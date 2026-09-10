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
// base URL (e.g. "http://172.16.100.6:3100") — no auth, because the
// on-prem Loki in this environment sits inside the private network.
type Client struct {
	endpoint   string
	httpClient *http.Client
}

// NewClient creates a Loki Client. endpoint is the base URL without
// trailing slash (e.g. "http://172.16.100.6:3100").
func NewClient(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// retrySleep is swapped out by tests so retry paths don't wait real
// backoffs.
var retrySleep = time.Sleep

// QueryRange fetches log entries from Loki matching selector (a complete
// LogQL stream selector, e.g. `{host="web-01"}` or
// `{namespace="gitea",pod="gitea-585b7c9565-r2lc7"}`) within the given
// time window. Building the right selector for a given alert is the
// caller's job — see Alert.AffectedIdentity — since which label
// vocabulary applies depends on where the alert came from, not on
// anything this client can infer.
//
// A transient failure (transport error or 5xx) is retried once after a
// short pause: a Loki failure fails the whole alert analysis, and on a
// home-lab network a single blip is common enough — and a duplicate
// query cheap enough — that one retry meaningfully cuts the "analysis
// died for nothing" rate. Anything 4xx fails immediately; the query
// won't get better by asking again.
func (c *Client) QueryRange(selector string, start, end time.Time, limit int) ([]LogEntry, error) {
	params := url.Values{}
	params.Set("query", selector)
	params.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}

	reqURL := fmt.Sprintf("%s/loki/api/v1/query_range?%s", c.endpoint, params.Encode())

	body, err := c.getWithOneRetry(reqURL)
	if err != nil {
		return nil, err
	}
	return parseLokiResponse(body)
}

func (c *Client) getWithOneRetry(reqURL string) ([]byte, error) {
	body, retryable, err := c.getOnce(reqURL)
	if err != nil && retryable {
		retrySleep(500 * time.Millisecond)
		body, _, err = c.getOnce(reqURL)
	}
	return body, err
}

func (c *Client) getOnce(reqURL string) (body []byte, retryable bool, err error) {
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, true, fmt.Errorf("loki: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("loki: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode >= 500, fmt.Errorf("loki: HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return body, false, nil
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
