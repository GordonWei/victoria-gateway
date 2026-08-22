// Package tracker defines the minimal issue-tracker surface Victoria
// Gateway's pending/confirmed RAG capture flow needs. pkg/gitea and
// pkg/github each implement it against their own API — the rest of the
// codebase (captureIncident, `sync`) talks to whichever one is configured
// through this interface and never imports either package directly.
package tracker

import "context"

// Tracker files an issue when an alert is analyzed, and lets `sync`
// check later whether it's been closed with a resolution.
type Tracker interface {
	// CreateIssue files a new issue and returns its number.
	CreateIssue(ctx context.Context, title, body string) (number int64, err error)
	// IssueState returns "open" or "closed" for the given issue number.
	IssueState(ctx context.Context, number int64) (state string, err error)
	// LastComment returns the body of the most recently posted comment
	// on an issue, or "" if it has none.
	LastComment(ctx context.Context, number int64) (string, error)
}
