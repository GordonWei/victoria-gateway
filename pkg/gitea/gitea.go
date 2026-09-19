// Package gitea is a minimal client for the two Gitea API calls Victoria
// Gateway's incident-capture flow needs: filing an issue when an alert is
// analyzed, and checking whether that issue has since been closed with a
// resolution. It's deliberately not a general-purpose Gitea SDK — just
// enough surface for pkg/rag's pending/confirmed flow.
package gitea

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
	endpoint string
	token    string
	owner    string
	repo     string
	client   *http.Client
}

type ClientConfig struct {
	Endpoint string // e.g. "https://gitea.ngu.tw"
	Token    string
	Owner    string // repo owner, e.g. "admin"
	Repo     string // e.g. "victoria-gateway-incidents"
	Timeout  time.Duration
}

func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		endpoint: cfg.Endpoint,
		token:    cfg.Token,
		owner:    cfg.Owner,
		repo:     cfg.Repo,
		client:   &http.Client{Timeout: timeout},
	}
}

// Issue is the subset of Gitea's issue object this package cares about.
type Issue struct {
	Number int64  `json:"number"`
	State  string `json:"state"` // "open" | "closed"
	Title  string `json:"title"`
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
	req.Header.Set("Authorization", "token "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("gitea request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gitea returned %d: %s", resp.StatusCode, string(respBody))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode gitea response: %w", err)
	}
	return nil
}

// CreateIssue files a new issue in the configured owner/repo and returns
// its issue number (the per-repo number shown in the UI/URL, not a
// global database ID).
func (c *Client) CreateIssue(ctx context.Context, title, body string) (int64, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues", c.owner, c.repo)
	var issue Issue
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"title": title, "body": body}, &issue); err != nil {
		return 0, fmt.Errorf("create issue: %w", err)
	}
	return issue.Number, nil
}

// GetIssue fetches one issue's current state.
func (c *Client) GetIssue(ctx context.Context, number int64) (Issue, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", c.owner, c.repo, number)
	var issue Issue
	if err := c.do(ctx, http.MethodGet, path, nil, &issue); err != nil {
		return Issue{}, fmt.Errorf("get issue: %w", err)
	}
	return issue, nil
}

// IssueState implements pkg/tracker.Tracker — a thin wrapper over
// GetIssue for callers that only need the state, not the full Issue.
func (c *Client) IssueState(ctx context.Context, number int64) (string, error) {
	issue, err := c.GetIssue(ctx, number)
	if err != nil {
		return "", err
	}
	return issue.State, nil
}

type comment struct {
	Body string `json:"body"`
}

// LastComment returns the body of the most recently posted comment on an
// issue, or "" if there are none. Victoria Gateway's sync flow treats the
// last comment before an issue is closed as the resolution — the normal
// workflow is investigate, then leave one final comment explaining what
// happened, then close.
//
// Gitea returns comments oldest-first and paginates at a server-default
// page size (commonly 30) if `limit` isn't set — an issue with more
// comments than that would otherwise silently return the last comment of
// page one, not the actual most recent one. Unlike GitHub's API, Gitea
// doesn't offer a sort-direction parameter here, so the fix is a
// generously high limit rather than a single "give me the last one"
// request; a resolution thread realistically never approaches this.
func (c *Client) LastComment(ctx context.Context, number int64) (string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments?limit=200", c.owner, c.repo, number)
	var comments []comment
	if err := c.do(ctx, http.MethodGet, path, nil, &comments); err != nil {
		return "", fmt.Errorf("list comments: %w", err)
	}
	if len(comments) == 0 {
		return "", nil
	}
	return comments[len(comments)-1].Body, nil
}

// CloseWithComment posts comment on the issue, then sets its state to
// closed. Two requests, not one — Gitea's issue-edit endpoint has no way
// to attach a comment atomically with a state change. Posting the
// comment first means that if the close call fails, the comment (the
// resolution itself) is still saved rather than silently lost; a retry
// then only needs to close, not re-post.
func (c *Client) CloseWithComment(ctx context.Context, number int64, body string) error {
	commentPath := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", c.owner, c.repo, number)
	if err := c.do(ctx, http.MethodPost, commentPath, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("post closing comment: %w", err)
	}
	closePath := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", c.owner, c.repo, number)
	if err := c.do(ctx, http.MethodPatch, closePath, map[string]string{"state": "closed"}, nil); err != nil {
		return fmt.Errorf("close issue (comment already posted): %w", err)
	}
	return nil
}
