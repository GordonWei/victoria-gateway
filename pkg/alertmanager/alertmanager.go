// Package alertmanager is a minimal client for Alertmanager's v2 silence
// API (POST/GET /api/v2/silences) — used by `victoria-gateway
// suppression-candidates --apply-silences` (see cmd/victoria-gateway's
// suppression.go) to create a real, time-bounded silence directly,
// instead of only printing one for a human to run through amtool.
//
// This is deliberately narrow: it does exactly what --apply-silences
// needs and nothing else — no route/receiver management, no alert
// querying, no general-purpose Alertmanager API coverage. In particular
// it never edits Alertmanager's static config.yml or triggers a config
// reload. A silence is a live, API-created, self-expiring object — that's
// what makes creating one automatable at all: get it wrong and it expires
// on its own, unlike a permanent route.routes edit, which is why
// pkg/suppress's RouteYAML output stays print-only (see that package's
// doc comment for the full reasoning). Applying it as a real route change
// still requires a human to read Alertmanager's current config, paste the
// entry in by hand, and reload — this package doesn't change that.
package alertmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config points at the Alertmanager instance to talk to.
type Config struct {
	Endpoint string // e.g. "http://172.16.100.6:9093"
	Username string // optional HTTP Basic Auth
	Password string
}

// Client is a minimal Alertmanager v2 silence API client.
type Client struct {
	endpoint   string
	username   string
	password   string
	httpClient *http.Client
}

// NewClient builds a Client. It performs no I/O — a bad endpoint only
// surfaces on the first actual call.
func NewClient(cfg Config) *Client {
	return &Client{
		endpoint:   strings.TrimRight(cfg.Endpoint, "/"),
		username:   cfg.Username,
		password:   cfg.Password,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Matcher is one label match, in Alertmanager's v2 silence API shape.
// IsEqual true + IsRegex false (what NewExactMatcher builds) is a plain
// exact-value match — the only kind this package's caller needs.
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

// NewExactMatcher builds a Matcher requiring label name to equal value
// exactly — no regex, no negation.
func NewExactMatcher(name, value string) Matcher {
	return Matcher{Name: name, Value: value, IsRegex: false, IsEqual: true}
}

// silence is the subset of Alertmanager's v2 silence object this package
// reads/writes on create — real silences carry more fields (id, status,
// etc.) but the create request only needs these.
type silence struct {
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

// silenceStatus is the subset of GET /api/v2/silences' response elements
// this package reads, to find whether a matching active silence already
// exists.
type silenceStatus struct {
	Matchers []Matcher `json:"matchers"`
	Status   struct {
		State string `json:"state"` // "active", "pending", or "expired"
	} `json:"status"`
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reader)
	if err != nil {
		return nil, fmt.Errorf("alertmanager: build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alertmanager: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// CreateSilence creates a new silence matching all of matchers, active
// from now until duration later, and returns its id. createdBy/comment
// are stored on the silence itself, visible in Alertmanager's own UI —
// useful for an operator later asking "why is this suppressed."
func (c *Client) CreateSilence(ctx context.Context, matchers []Matcher, duration time.Duration, createdBy, comment string) (id string, err error) {
	now := time.Now().UTC()
	body, err := json.Marshal(silence{
		Matchers:  matchers,
		StartsAt:  now,
		EndsAt:    now.Add(duration),
		CreatedBy: createdBy,
		Comment:   comment,
	})
	if err != nil {
		return "", fmt.Errorf("alertmanager: marshal silence: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/v2/silences", body)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("alertmanager: read create response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("alertmanager: create silence: status %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("alertmanager: parse create response: %w", err)
	}
	return out.SilenceID, nil
}

// ActiveSilenceExists reports whether an active (not expired, not merely
// pending-for-the-future) silence already matches exactly this set of
// matchers — used to skip re-creating a silence --apply-silences already
// applied on a previous run, so repeated runs are idempotent rather than
// piling up duplicate silences for the same candidate.
func (c *Client) ActiveSilenceExists(ctx context.Context, matchers []Matcher) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v2/silences", nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("alertmanager: list silences: status %d: %s", resp.StatusCode, string(body))
	}
	var existing []silenceStatus
	if err := json.NewDecoder(resp.Body).Decode(&existing); err != nil {
		return false, fmt.Errorf("alertmanager: parse list response: %w", err)
	}
	for _, s := range existing {
		if s.Status.State == "active" && matchersEqual(s.Matchers, matchers) {
			return true, nil
		}
	}
	return false, nil
}

// matchersEqual compares two matcher sets order-independently —
// Alertmanager doesn't guarantee it echoes matchers back in the order
// they were submitted.
func matchersEqual(a, b []Matcher) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[Matcher]int, len(a))
	for _, m := range a {
		count[m]++
	}
	for _, m := range b {
		count[m]--
	}
	for _, n := range count {
		if n != 0 {
			return false
		}
	}
	return true
}
