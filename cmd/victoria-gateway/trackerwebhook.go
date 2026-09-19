// trackerwebhook.go implements POST /webhook/gitea-issues and POST
// /webhook/github-issues: an "issue closed" event from either tracker
// triggers an immediate resync of that one issue, instead of waiting for
// the next `victoria-gateway sync` cron tick (see sync.go's doc comment
// — cron sync remains a fallback for missed/failed deliveries, this is
// just the fast path). Both handlers share resyncIssue below; only
// signature verification and payload shape differ between the two
// trackers.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// resyncIssue is the shared core both webhook handlers and (in spirit)
// `sync`'s per-record loop implement: given a tracker issue number that's
// just been closed, look up the Pending record it's linked to, fetch the
// issue's last comment as the resolution, and confirm. Every outcome
// short of an actual error is reported, not treated as failure — "no
// linked Pending record" and "closed with no comment" are both normal,
// expected states a webhook can arrive in.
func (h *handler) resyncIssue(ctx context.Context, issueNumber int64) (result string, err error) {
	if h.tracker == nil {
		return "", errors.New("no issue tracker configured")
	}
	rec, err := h.rag.GetPendingByGiteaIssue(ctx, issueNumber)
	if err != nil {
		if errors.Is(err, rag.ErrNotFound) {
			// Not every closed issue is one Victoria Gateway filed (or
			// it's already Confirmed — e.g. this is the webhook firing
			// for the very close a web-form confirm just performed via
			// CloseWithComment, chasing its own tail). Not an error.
			return "no pending record linked to this issue (already confirmed, or not ours)", nil
		}
		return "", fmt.Errorf("look up pending record: %w", err)
	}

	resolution, err := h.tracker.LastComment(ctx, issueNumber)
	if err != nil {
		return "", fmt.Errorf("fetch last comment: %w", err)
	}
	if resolution == "" {
		return "closed with no comment, left pending", nil
	}

	switch err := h.rag.ConfirmPending(ctx, rec.ID, resolution); {
	case err == nil:
		return fmt.Sprintf("confirmed id=%d from issue #%d", rec.ID, issueNumber), nil
	case errors.Is(err, rag.ErrAlreadyConfirmed):
		return "already confirmed (likely by the web form, which is what closed this issue)", nil
	default:
		return "", fmt.Errorf("confirm: %w", err)
	}
}

// giteaIssueWebhookPayload is the handful of fields this handler needs
// from Gitea's "Issues" webhook event. Gitea's actual payload has many
// more; everything else is ignored.
type giteaIssueWebhookPayload struct {
	Action string `json:"action"` // "opened", "closed", "edited", "reopened", ...
	Issue  struct {
		Number int64 `json:"number"`
	} `json:"issue"`
}

// handleGiteaIssueWebhook serves POST /webhook/gitea-issues. Refuses
// every request when GiteaWebhookSecret isn't configured — this endpoint
// never runs unauthenticated. See config.GiteaConfig.WebhookSecret's doc
// comment for the Gitea-side webhook setup this expects.
func (h *handler) handleGiteaIssueWebhook(w http.ResponseWriter, r *http.Request) {
	if h.giteaWebhookSecret == "" {
		http.NotFound(w, r) // indistinguishable from a route that doesn't exist, on purpose
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MiB: generous for an issue-event payload, bounded against abuse
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !verifyGiteaSignature(h.giteaWebhookSecret, body, r.Header.Get("X-Gitea-Signature")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	var payload giteaIssueWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if payload.Action != "closed" {
		w.WriteHeader(http.StatusOK) // opened/edited/reopened/etc — nothing to do, not an error
		return
	}

	result, err := h.resyncIssue(r.Context(), payload.Issue.Number)
	if err != nil {
		log.Printf("gitea webhook: resync issue #%d failed: %v", payload.Issue.Number, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("gitea webhook: issue #%d: %s", payload.Issue.Number, result)
	w.WriteHeader(http.StatusOK)
}

// verifyGiteaSignature checks X-Gitea-Signature: hex(HMAC-SHA256(secret,
// body)). Gitea documents this exact scheme (unlike GitHub's
// X-Hub-Signature-256, Gitea's header carries the hex digest with no
// "sha256=" prefix).
func verifyGiteaSignature(secret string, body []byte, signatureHeader string) bool {
	if signatureHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}

// githubIssueWebhookPayload is the handful of fields this handler needs
// from GitHub's "issues" webhook event.
type githubIssueWebhookPayload struct {
	Action string `json:"action"` // "opened", "closed", "edited", "reopened", ...
	Issue  struct {
		Number int64 `json:"number"`
	} `json:"issue"`
}

// handleGitHubIssueWebhook serves POST /webhook/github-issues — the
// GitHub-tracker counterpart to handleGiteaIssueWebhook, same idea, only
// the signature scheme (X-Hub-Signature-256, "sha256=" prefixed) and
// header name differ.
func (h *handler) handleGitHubIssueWebhook(w http.ResponseWriter, r *http.Request) {
	if h.githubWebhookSecret == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !verifyGitHubSignature(h.githubWebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	var payload githubIssueWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if payload.Action != "closed" {
		w.WriteHeader(http.StatusOK)
		return
	}

	result, err := h.resyncIssue(r.Context(), payload.Issue.Number)
	if err != nil {
		log.Printf("github webhook: resync issue #%d failed: %v", payload.Issue.Number, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("github webhook: issue #%d: %s", payload.Issue.Number, result)
	w.WriteHeader(http.StatusOK)
}

// verifyGitHubSignature checks X-Hub-Signature-256:
// "sha256="+hex(HMAC-SHA256(secret, body)), GitHub's documented scheme.
func verifyGitHubSignature(secret string, body []byte, signatureHeader string) bool {
	const prefix = "sha256="
	if len(signatureHeader) <= len(prefix) || signatureHeader[:len(prefix)] != prefix {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := prefix + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signatureHeader))
}
