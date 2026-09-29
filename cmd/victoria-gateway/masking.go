package main

import (
	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/mask"
)

// This file holds the rag.mask_log_excerpt call sites' shared plumbing.
// The switch is applied at the boundaries text crosses, not once at the
// end, because the same log line leaves the process in several directions
// (summarizer prompt, cloud escalation prompt, embedding input, Jev, the
// tracker issue, Postgres, /incidents, the notification) and a single
// late mask only ever protected the last of them. Two points cover all of
// them: everything that enters the pipeline (maskInputs, right after the
// log query) and everything a model hands back (maskResult, right after
// each Summarize / runEscalation call). With the switch off every helper
// here returns its argument untouched.

// maskText redacts credential-shaped substrings when rag.mask_log_excerpt
// is on, and is the identity otherwise.
func (h *handler) maskText(s string) string {
	if !h.maskLogExcerpt || s == "" {
		return s
	}
	return mask.RedactLikelyCredentials(s)
}

// maskInputs returns copies of alert and logs safe to hand to a model and
// to the embedder: log lines and the free-text annotations (summary,
// description) are redacted; labels are left alone since they identify
// the alert and the host. The caller's originals are never modified.
func (h *handler) maskInputs(alert aiops.Alert, logs []aiops.LogEntry) (aiops.Alert, []aiops.LogEntry) {
	if !h.maskLogExcerpt {
		return alert, logs
	}
	masked := make([]aiops.LogEntry, len(logs))
	for i, l := range logs {
		masked[i] = aiops.LogEntry{Timestamp: l.Timestamp, Line: mask.RedactLikelyCredentials(l.Line)}
	}
	if len(alert.Annotations) > 0 {
		ann := make(map[string]string, len(alert.Annotations))
		for k, v := range alert.Annotations {
			ann[k] = mask.RedactLikelyCredentials(v)
		}
		alert.Annotations = ann
	}
	return alert, masked
}

// maskResult redacts what a model wrote back — a summary or a mitigation
// plan can quote a credential it was shown in a source that masking
// doesn't reach (an older stored record, a tool the agent ran itself).
// The Mitigation struct is copied, not edited in place.
func (h *handler) maskResult(r aiops.SummarizeResult) aiops.SummarizeResult {
	if !h.maskLogExcerpt {
		return r
	}
	r.Summary = mask.RedactLikelyCredentials(r.Summary)
	r.Reason = mask.RedactLikelyCredentials(r.Reason)
	if r.Mitigation != nil {
		m := *r.Mitigation
		m.Plan = mask.RedactLikelyCredentials(m.Plan)
		r.Mitigation = &m
	}
	return r
}
