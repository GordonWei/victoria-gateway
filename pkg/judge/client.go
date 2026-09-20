// Package judge is a client for TypeSafe AI's System One API
// (https://docs.typesafe.ai) — a structured-decision model that answers
// narrow questions with calibrated probabilities instead of generating
// text. It's used here as a second, independent signal on top of the
// local summarizer's own self-reported escalate/confidence fields (see
// pkg/aiops.SummarizeResult), which are known to be poorly calibrated for
// small local models.
//
// Chinese-language calibration for this API is undocumented upstream (see
// docs/topics/jev-typesafe/note.md in the Study project for the full
// research writeup) — this package is wired in dry-run mode only until
// that's verified against real incident text; see cmd/judge-eval.
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultEndpoint is TypeSafe AI's System One API endpoint.
const DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"

// DefaultModel is the only model TypeSafe currently serves; "jev-latest"
// and "jev-preview" are aliases for the same version as of the research
// in docs/topics/jev-typesafe/note.md (2026-09-20) — not usable for
// staged rollout / canarying between versions.
const DefaultModel = "jev-latest"

// QuestionType selects which System One primitive a Question uses.
type QuestionType string

const (
	// TypeScore asks the model to place the state on an ordered scale
	// (2-10 levels). Response carries a weighted score plus the full
	// probability distribution across levels.
	TypeScore QuestionType = "score"

	// TypeNoul asks a yes/no probability question. Response carries a
	// single 0-1 probability that the answer is "true" — there is no
	// confidence field for this primitive (see note.md's "坑" section);
	// callers that want a confidence-style signal have to derive one from
	// how far the probability sits from 0.5.
	TypeNoul QuestionType = "noul"

	// TypeChoice asks the model to pick one of several named options.
	// Not used by this package yet (JudgeEscalation only needs Score +
	// Noul) but included so Ask isn't Score/Noul-specific if a future
	// caller needs it.
	TypeChoice QuestionType = "choice"
)

// Question is one question in a System One request. Criteria's shape
// depends on Type: a []string of level descriptions for Score (ordered
// low to high), a map[string]string of {"true": ..., "false": ...}
// descriptions for Noul (optional — omit for the default framing), or a
// map[string]string of {option: description} for Choice.
type Question struct {
	Type         QuestionType `json:"type"`
	Instructions string       `json:"instructions"`
	Criteria     any          `json:"criteria,omitempty"`
}

// Answer is one question's result. Which fields are populated depends on
// the corresponding Question's Type — Score populates Score/Probabilities
// /Confidence/Legend, Noul populates only Noul, Choice populates
// Choice/Probabilities/Confidence. Unused fields are left at their zero
// value rather than using separate response types per primitive, since a
// single request commonly mixes types and the API's own response shape is
// a flat per-question object with type-dependent keys.
type Answer struct {
	// Score is Score's weighted position on the scale (e.g. 1.30 for a
	// mostly-level-1-with-some-level-2 distribution).
	Score float64 `json:"score"`
	// Noul is the 0-1 probability the answer is "true".
	Noul float64 `json:"noul"`
	// Choice is the highest-probability option name.
	Choice string `json:"choice"`
	// Probabilities is the full distribution (Score: per level index as
	// string key; Choice: per option name). Not populated for Noul.
	Probabilities map[string]float64 `json:"probabilities"`
	// Confidence is derived from Probabilities' concentration. Not
	// populated for Noul — see TypeNoul's doc comment.
	Confidence float64 `json:"confidence"`
	// Legend maps Score's level indices to their description, echoed back
	// from the request's Criteria for convenience when logging/storing a
	// score without the original question alongside it.
	Legend map[string]string `json:"legend"`
}

// Client talks to the TypeSafe AI System One API.
type Client struct {
	endpoint   string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient builds a Client against the real TypeSafe AI endpoint. apiKey
// is required — the API rejects unauthenticated requests with 401.
func NewClient(apiKey string) *Client {
	return &Client{
		endpoint:   DefaultEndpoint,
		apiKey:     apiKey,
		model:      DefaultModel,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// NewClientWithEndpoint is NewClient with an overridable endpoint, for
// pointing at an httptest.Server in tests. Production callers want
// NewClient.
func NewClientWithEndpoint(apiKey, endpoint string) *Client {
	c := NewClient(apiKey)
	c.endpoint = endpoint
	return c
}

type systemOneRequest struct {
	State     string              `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// systemOneResponse is the actual wire shape returned by
// api.typesafe.ai/v1/systemone — confirmed against the live API
// 2026-09-21: {"model": "...", "answers": {<id>: {...}}, "usage": {...}}.
// note.md's per-primitive field docs describe each Answer's own shape
// correctly; what it doesn't call out is that the top-level response
// wraps them under "answers" rather than being that map directly.
type systemOneResponse struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
}

// Ask sends one request bundling every question in questions (System One
// pricing/latency is per-request, not per-question — see note.md's
// "Speculative Fan-Out" pattern) and returns each question's Answer keyed
// by the same id. A non-2xx response is returned as an error carrying the
// status code and body so a caller can distinguish 401 (bad key) from 429
// (rate limited) from 5xx without re-parsing anything.
func (c *Client) Ask(ctx context.Context, state string, questions map[string]Question) (map[string]Answer, error) {
	if len(questions) == 0 {
		return nil, fmt.Errorf("judge: Ask called with no questions")
	}

	body, err := json.Marshal(systemOneRequest{State: state, Model: c.model, Questions: questions})
	if err != nil {
		return nil, fmt.Errorf("judge: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("judge: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("judge: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("judge: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("judge: %s returned %d: %s", c.endpoint, resp.StatusCode, string(respBody))
	}

	var parsed systemOneResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("judge: parse response: %w (body: %s)", err, string(respBody))
	}
	return parsed.Answers, nil
}

// severityLevels mirrors TypeSafe's own llm_guardrails cookbook (see
// note.md §"Pattern 與 Cookbook"): a 0-3 harm/severity scale from "none"
// to "extreme". Reused here for infrastructure-incident severity rather
// than content harm — the shape (an ordered 4-level scale with a Score
// question) is what's being borrowed, not the wording.
var severityLevels = []string{
	"無明顯影響：告警可能是雜訊、已知良性狀況，或影響範圍極小",
	"輕微：局部或非關鍵服務受影響，有明確線索指向原因",
	"嚴重：使用者可見的服務中斷或效能劣化，原因不明或牽涉多個元件",
	"極嚴重：大範圍服務中斷、資料風險，或看起來需要立即人工介入",
}

const escalationQuestionID = "needs_escalation"
const severityQuestionID = "severity"

// EscalationJudgment is Jev's independent read on one already-analyzed
// alert: how severe it looks, and whether it should go to a stronger
// model. It is NOT used to make the actual escalation decision yet — see
// this package's doc comment — callers should log/record it
// side-by-side with the existing aiops.ShouldEscalate result, not branch
// on it.
type EscalationJudgment struct {
	// Severity is the weighted 0-3 score (see severityLevels).
	Severity float64
	// SeverityConfidence is how concentrated the severity distribution
	// was (Score primitive; higher = more decisive).
	SeverityConfidence float64
	// EscalateProbability is Noul's 0-1 probability that this incident
	// needs a stronger model's attention.
	EscalateProbability float64
}

// JudgeEscalation asks Jev to independently assess one alert's already-
// produced local summary: how severe it looks, and whether it warrants
// escalation to a stronger model. alertName and summary should be the
// same text a human would see in the notification — this is meant to run
// after the local summarizer, evaluating its output, not instead of it.
func (c *Client) JudgeEscalation(ctx context.Context, alertName, summary string) (EscalationJudgment, error) {
	state := fmt.Sprintf("告警名稱：%s\n\n分析摘要：%s", alertName, summary)

	questions := map[string]Question{
		severityQuestionID: {
			Type:         TypeScore,
			Instructions: "根據這份地端基礎設施告警的分析摘要，評估這次事件的嚴重程度。",
			Criteria:     severityLevels,
		},
		escalationQuestionID: {
			Type:         TypeNoul,
			Instructions: "根據這份分析摘要，這個問題是否複雜到需要交給更強的模型重新分析（例如：摘要顯示資訊不足以判斷根因、牽涉多個服務或不熟悉的關聯、或摘要本身顯得不確定）？",
		},
	}

	answers, err := c.Ask(ctx, state, questions)
	if err != nil {
		return EscalationJudgment{}, err
	}

	severity, ok := answers[severityQuestionID]
	if !ok {
		return EscalationJudgment{}, fmt.Errorf("judge: response missing %q question", severityQuestionID)
	}
	escalation, ok := answers[escalationQuestionID]
	if !ok {
		return EscalationJudgment{}, fmt.Errorf("judge: response missing %q question", escalationQuestionID)
	}

	return EscalationJudgment{
		Severity:            severity.Score,
		SeverityConfidence:  severity.Confidence,
		EscalateProbability: escalation.Noul,
	}, nil
}
