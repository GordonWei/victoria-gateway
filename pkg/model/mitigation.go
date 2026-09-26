package model

// Outcomes of asking a backend for a mitigation plan, used as
// Mitigation.Result and as the result label on
// victoria_gateway_mitigation_plan_total.
const (
	MitigationOK    = "ok"    // a plan came back
	MitigationNone  = "none"  // the backend ran but produced no plan
	MitigationError = "error" // asking for or reading the plan failed (including a timeout)
)

// Mitigation is a remediation plan a backend produced for what it just
// analyzed, next to the analysis itself. It's the backend's proposal, not
// a verified fix — the tracker issue shows it for a human to review.
type Mitigation struct {
	// Source names where the plan came from, for the tracker issue's
	// heading, e.g. "AWS DevOps Agent mitigation plan".
	Source string
	// Plan is the plan itself (Markdown), empty unless Result is
	// MitigationOK.
	Plan string
	// Result is MitigationOK, MitigationNone or MitigationError.
	Result string
	// Err says what went wrong when Result is MitigationError, for the
	// caller's log line (this package doesn't log).
	Err string
}

// ChatResult is a reply plus whatever else a backend produced alongside
// it. Mitigation is nil when the backend wasn't asked for a plan.
type ChatResult struct {
	Reply      string
	Mitigation *Mitigation
}

// DetailedLLM is implemented by backends whose answer can carry more than
// the reply text — today only DevOpsAgentClient, whose investigation can
// be followed by a mitigation plan. Callers that care (pkg/aiops) check
// for it with a type assertion and fall back to Chat otherwise, so the
// LLM interface itself stays unchanged for every other backend.
type DetailedLLM interface {
	LLM
	ChatDetailed(messages []Message, opts *ChatOptions) (ChatResult, error)
}
