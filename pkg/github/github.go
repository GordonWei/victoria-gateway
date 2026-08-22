// Package github is a minimal client for the GitHub REST API calls
// Victoria Gateway's incident-capture flow needs: filing an issue when
// an alert is analyzed, and checking whether it's since been closed with
// a resolution. It implements pkg/tracker.Tracker the same way
// pkg/gitea does — this is the alternative for anyone using GitHub
// Issues instead of a self-hosted Gitea instance.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	endpoint string // defaults to "https://api.github.com"
	token    string
	owner    string
	repo     string
	client   *http.Client
}

type ClientConfig struct {
	Endpoint string // defaults to "https://api.github.com"; override for GitHub Enterprise
	Token    string // a personal access token with Issues read/write on the target repo
	Owner    string
	Repo     string
	Timeout  time.Duration
}

func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return &Client{
		endpoint: endpoint,
		token:    cfg.Token,
		owner:    cfg.Owner,
		repo:     cfg.Repo,
		client:   &http.Client{Timeout: timeout},
	}
}

type issue struct {
	Number int64  `json:"number"`
	State  string `json:"state"` // "open" | "closed"
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// GitHub accepts both "token" and "Bearer" for the Authorization
	// header on classic and fine-grained PATs alike; Bearer is what
	// GitHub's current API docs lead with.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("github request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github returned %d: %s", resp.StatusCode, string(respBody))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode github response: %w", err)
	}
	return nil
}

// CreateIssue files a new issue and returns its issue number.
func (c *Client) CreateIssue(ctx context.Context, title, body string) (int64, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo)
	var iss issue
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"title": title, "body": body}, &iss); err != nil {
		return 0, fmt.Errorf("create issue: %w", err)
	}
	return iss.Number, nil
}

// IssueState returns "open" or "closed" for the given issue number.
func (c *Client) IssueState(ctx context.Context, number int64) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", c.owner, c.repo, number)
	var iss issue
	if err := c.do(ctx, http.MethodGet, path, nil, &iss); err != nil {
		return "", fmt.Errorf("get issue: %w", err)
	}
	return iss.State, nil
}

type comment struct {
	Body string `json:"body"`
}

// LastComment returns the body of the most recently posted comment on an
// issue, or "" if there are none. Asks the API to sort newest-first and
// return only one result (rather than fetching the whole list and taking
// the last element) — GitHub's comments endpoint defaults to 30 per page,
// and an issue with more comments than that would otherwise silently
// return the last comment of page one, not the actual most recent one.
func (c *Client) LastComment(ctx context.Context, number int64) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?sort=created&direction=desc&per_page=1", c.owner, c.repo, number)
	var comments []comment
	if err := c.do(ctx, http.MethodGet, path, nil, &comments); err != nil {
		return "", fmt.Errorf("list comments: %w", err)
	}
	if len(comments) == 0 {
		return "", nil
	}
	return comments[0].Body, nil
}
