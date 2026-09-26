package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// devOpsAgentMitigationSource heads the tracker issue section a plan from
// this client lands in.
const devOpsAgentMitigationSource = "AWS DevOps Agent mitigation plan"

// mitigationSummaryRecordType is the journal record type AWS DevOps Agent
// stores a mitigation plan under — the counterpart of the
// investigation_summary_md record finalSummary reads. Documented in the
// EventBridge integration guide ("Retrieving an investigation or
// mitigation summary"): ListJournalRecords with recordType
// mitigation_summary_md on the mitigation's execution.
const mitigationSummaryRecordType = "mitigation_summary_md"

// fetchMitigation runs the mitigation phase under its own timeout and
// turns the outcome into a Mitigation. It never returns an error: a
// missing or failed plan leaves the investigation's answer untouched.
func (c *DevOpsAgentClient) fetchMitigation(ctx context.Context, session *mcp.ClientSession, taskID, investigationExecutionID string) *Mitigation {
	ctx, cancel := context.WithTimeout(ctx, c.mitigationTimeout)
	defer cancel()

	plan, err := c.mitigation(ctx, session, taskID, investigationExecutionID)
	switch {
	case err != nil:
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("no plan within %s: %w", c.mitigationTimeout, err)
		}
		return &Mitigation{Source: devOpsAgentMitigationSource, Result: MitigationError, Err: err.Error()}
	case plan == "":
		return &Mitigation{Source: devOpsAgentMitigationSource, Result: MitigationNone}
	default:
		return &Mitigation{Source: devOpsAgentMitigationSource, Result: MitigationOK, Plan: plan}
	}
}

// mitigation gets the mitigation plan for a completed investigation,
// returning "" if the agent produced none.
//
// How a plan comes about, per AWS's documentation and its official sample
// MCP server (aws-samples/sample-aws-devops-agent-acp-mcp, v1.1.0+):
//
//   - The agent only offers a plan once the investigation has a root
//     cause; in the web app that's the "Generate mitigation plan" button.
//     Alarm-triggered investigations may already carry inline mitigation
//     proposals, so the investigation's own execution is checked first —
//     if a plan is already there, nothing more is started (or billed).
//   - Otherwise the sample server's create_mitigation_plan tool starts
//     one: UpdateBacklogTask with taskStatus PENDING_START on the
//     completed task, which runs the Mitigation Agent as a new execution
//     of the same task. That's the documented use of UpdateBacklogTask
//     ("approve a mitigation plan") in the IAM reference, and the
//     mitigation's lifecycle shows up as its own EventBridge events.
//   - The task is polled until it completes again, then the task's
//     executions are searched for a mitigation_summary_md journal record.
//
// Only the plan is read. Nothing here approves or applies it: the plan
// is a proposal for a human, and acting on it stays a human decision.
func (c *DevOpsAgentClient) mitigation(ctx context.Context, session *mcp.ClientSession, taskID, investigationExecutionID string) (string, error) {
	if investigationExecutionID != "" {
		plan, err := c.readMitigationSummary(ctx, session, investigationExecutionID)
		if err != nil {
			return "", err
		}
		if plan != "" {
			return plan, nil
		}
	}

	text, err := callTool(ctx, session, "create_mitigation_plan", map[string]any{"task_id": taskID})
	if err != nil {
		return "", fmt.Errorf("start mitigation plan (needs sample-aws-devops-agent-acp-mcp v1.1.0+): %w", err)
	}
	if err := apiError("create_mitigation_plan", text); err != nil {
		return "", err
	}

	latestExecutionID, err := c.pollMitigation(ctx, session, taskID)
	if err != nil {
		return "", err
	}

	candidates, err := c.mitigationExecutions(ctx, session, taskID, investigationExecutionID)
	if err != nil {
		return "", err
	}
	// The task's own current executionId first (in case it now points at
	// the mitigation run), then the task's other top-level executions,
	// newest first.
	if latestExecutionID != "" && latestExecutionID != investigationExecutionID {
		candidates = append([]string{latestExecutionID}, candidates...)
	}
	seen := map[string]bool{}
	for _, id := range candidates {
		if seen[id] {
			continue
		}
		seen[id] = true
		plan, err := c.readMitigationSummary(ctx, session, id)
		if err != nil {
			return "", err
		}
		if plan != "" {
			return plan, nil
		}
	}
	return "", nil
}

// pollMitigation waits for the task to finish its mitigation run and
// returns the task's executionId at that point. It sleeps before the
// first poll so a get_task that still reflects the investigation's
// COMPLETED status isn't mistaken for the mitigation's.
//
// PENDING_CUSTOMER_APPROVAL is treated as done: it's one of
// UpdateBacklogTask's documented statuses, and UpdateBacklogTask is how a
// plan gets approved, so a task waiting there has a plan to read. It is
// never approved from here.
func (c *DevOpsAgentClient) pollMitigation(ctx context.Context, session *mcp.ClientSession, taskID string) (string, error) {
	for {
		if err := sleepCtx(ctx, c.pollInterval); err != nil {
			return "", err
		}
		text, err := callTool(ctx, session, "get_task", map[string]any{"task_id": taskID})
		if err != nil {
			return "", fmt.Errorf("poll mitigation: %w", err)
		}
		if err := apiError("get_task", text); err != nil {
			return "", err
		}
		var status taskEnvelope
		if err := json.Unmarshal([]byte(text), &status); err != nil {
			return "", fmt.Errorf("parse task status: %w", err)
		}
		switch status.Task.Status {
		case "COMPLETED", "PENDING_CUSTOMER_APPROVAL":
			return status.Task.ExecutionID, nil
		case "FAILED", "TIMED_OUT", "CANCELED", "CANCELLED":
			return "", fmt.Errorf("mitigation run ended %s", status.Task.Status)
		}
	}
}

type executionsResponse struct {
	Executions []struct {
		ExecutionID       string          `json:"executionId"`
		ParentExecutionID string          `json:"parentExecutionId"`
		CreatedAt         json.RawMessage `json:"createdAt"`
	} `json:"executions"`
}

// mitigationExecutions lists the task's top-level executions other than
// the investigation's own, newest first. Child executions
// (parentExecutionId set) are the investigation's parallel sub-agents,
// not a mitigation run, so they're skipped.
func (c *DevOpsAgentClient) mitigationExecutions(ctx context.Context, session *mcp.ClientSession, taskID, investigationExecutionID string) ([]string, error) {
	text, err := callTool(ctx, session, "list_executions", map[string]any{"task_id": taskID})
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	if err := apiError("list_executions", text); err != nil {
		return nil, err
	}
	var resp executionsResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		return nil, fmt.Errorf("parse list_executions response: %w", err)
	}
	execs := resp.Executions[:0:0]
	for _, e := range resp.Executions {
		if e.ExecutionID == "" || e.ParentExecutionID != "" || e.ExecutionID == investigationExecutionID {
			continue
		}
		execs = append(execs, e)
	}
	// createdAt arrives as an ISO-8601 string from the sample server (it
	// serializes boto3 datetimes with isoformat), which sorts correctly
	// as text; the raw JSON is compared so an epoch number would too.
	sort.SliceStable(execs, func(i, j int) bool { return string(execs[i].CreatedAt) > string(execs[j].CreatedAt) })
	ids := make([]string, len(execs))
	for i, e := range execs {
		ids[i] = e.ExecutionID
	}
	return ids, nil
}

type mitigationRecordsResponse struct {
	Records []struct {
		RecordType string          `json:"recordType"`
		Content    json.RawMessage `json:"content"`
	} `json:"records"`
}

// readMitigationSummary returns the newest mitigation_summary_md record's
// content for one execution, or "" if it has none.
func (c *DevOpsAgentClient) readMitigationSummary(ctx context.Context, session *mcp.ClientSession, executionID string) (string, error) {
	text, err := callTool(ctx, session, "list_journal_records", map[string]any{
		"execution_id": executionID,
		"record_type":  mitigationSummaryRecordType,
		"order":        "DESC",
		"limit":        10,
	})
	if err != nil {
		return "", fmt.Errorf("read mitigation journal: %w", err)
	}
	if err := apiError("list_journal_records", text); err != nil {
		return "", err
	}
	var resp mitigationRecordsResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		return "", fmt.Errorf("parse mitigation journal: %w", err)
	}
	for _, rec := range resp.Records {
		if rec.RecordType != "" && rec.RecordType != mitigationSummaryRecordType {
			continue
		}
		if plan := strings.TrimSpace(journalContentText(rec.Content)); plan != "" {
			return plan, nil
		}
	}
	return "", nil
}

// journalContentText renders a journal record's content as text. The API
// types content as a JSON value; a summary record's is a Markdown string,
// but anything else is kept as its raw JSON rather than dropped.
func journalContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// apiError reports the {"error": code, "message": msg} shape the sample
// server returns (as ordinary text, not an MCP error result) when the
// underlying AWS call fails — AccessDenied, a ValidationError for a task
// that isn't COMPLETED, throttling.
func apiError(tool, text string) error {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(text), &e) != nil || e.Error == "" {
		return nil
	}
	return fmt.Errorf("%s: %s: %s", tool, e.Error, e.Message)
}
