package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DevOpsAgentClient escalates to AWS DevOps Agent (docs.aws.amazon.com/devopsagent)
// over MCP instead of a plain chat completion. Unlike the other cloud
// clients in this file, escalating here doesn't just re-ask an LLM the same
// question with a bigger model — it hands the alert to an agent that can
// actually read the target AWS account (CloudWatch metrics/alarms,
// CloudTrail, Lambda/EC2/etc. config) and come back with an evidence-backed
// root cause, not just a guess grounded in the log excerpt it was given.
//
// This only makes sense as an escalation target when the thing being
// investigated is itself AWS infrastructure — the agent has no visibility
// into on-prem/home-lab hosts. See CloudConfig.Provider = "aws-devops-agent".
//
// This is deliberately more than a one-way handoff, though. RAG-retrieved
// history of past, human-confirmed incidents — whatever's already in this
// deployment's RAG store, homelab or AWS — is passed into the
// investigation as context (see Chat), and the investigation's own
// conclusion flows back into the same RAG store afterward exactly like
// any other provider's result does (pkg/aiops.captureIncident doesn't
// distinguish which cloud provider produced a result). So over time the
// two sides aren't separate: AWS-side investigations get to draw on what
// was learned on-prem and vice versa, one incident memory instead of two
// unrelated ones that happen to share a Go interface.
//
// The official sample MCP server (aws-samples/sample-aws-devops-agent-acp-mcp)
// is a Python process; this client shells out to it as a subprocess and
// talks standard MCP over stdio, so victoria-gateway's core stays a single
// Go binary and only deployments that enable this provider need a Python
// 3.10+ runtime alongside it.
type DevOpsAgentClient struct {
	binaryPath   string // path to the "aws-devops-agent" executable, e.g. from `pip install -e '.[mcp]'`
	userID       string // DEVOPS_AGENT_USER_ID
	region       string // DEVOPS_AGENT_REGION
	spaceID      string // DEVOPS_AGENT_SPACE_ID; created out-of-band, see ONBOARDING.md
	priority     string // CRITICAL, HIGH, MEDIUM, LOW, or MINIMAL — defaults to HIGH
	pollInterval time.Duration
	pollTimeout  time.Duration

	// transport builds the MCP transport to connect over. Defaults to a
	// real "aws-devops-agent mcp" subprocess (see newCommandTransport);
	// tests swap this for an in-memory transport pair so the polling/
	// summary-extraction logic can be exercised without shelling out to
	// the real Python package.
	transport func() mcp.Transport
}

// Two real bugs in aws-samples/sample-aws-devops-agent-acp-mcp were found
// getting this client working end-to-end against the live service
// (2026-08-23, account 120340392319, region us-east-1) and are worked
// around deliberately rather than silently:
//
//  1. The package's `mcp` dependency is pinned as `mcp[cli]>=1.2.0` with no
//     upper bound. `mcp` shipped a 2.0.0 that removed `mcp.server.fastmcp`,
//     which the sample server imports at module load — `pip install -e
//     '.[mcp]'` on or after that release fails at import time with
//     `ModuleNotFoundError: No module named 'mcp.server.fastmcp'`. Pin
//     `mcp==1.29.0` (latest 1.x) when installing that package.
//  2. The convenience `investigate` MCP tool flattens the backlog-task
//     response incorrectly (`parsed.get("taskId")` when the real field is
//     `parsed["task"]["taskId"]`), so its taskId/executionId always come
//     back null. This client calls the sibling `create_investigation` tool
//     instead, which returns the same nested shape unflattened and
//     correctly — see the taskEnvelope doc comment below.

type DevOpsAgentClientConfig struct {
	BinaryPath   string // defaults to "aws-devops-agent" (resolved via PATH)
	UserID       string
	Region       string // defaults to "us-east-1"
	SpaceID      string
	Priority     string        // defaults to "HIGH"
	PollInterval time.Duration // defaults to 30s, per the tool's own documented cadence
	PollTimeout  time.Duration // defaults to 10 minutes; investigations are documented as taking 5-8 min
}

func NewDevOpsAgentClient(cfg DevOpsAgentClientConfig) *DevOpsAgentClient {
	binaryPath := cfg.BinaryPath
	if binaryPath == "" {
		binaryPath = "aws-devops-agent"
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	priority := cfg.Priority
	if priority == "" {
		priority = "HIGH"
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	pollTimeout := cfg.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = 10 * time.Minute
	}
	c := &DevOpsAgentClient{
		binaryPath:   binaryPath,
		userID:       cfg.UserID,
		region:       region,
		spaceID:      cfg.SpaceID,
		priority:     priority,
		pollInterval: pollInterval,
		pollTimeout:  pollTimeout,
	}
	c.transport = c.newCommandTransport
	return c
}

// newCommandTransport is the default transport: a fresh "aws-devops-agent
// mcp" subprocess per call. Escalations are rare (this is the expensive,
// deliberate tier of a triage pipeline that's free by default — see
// pkg/aiops), so the process-per-call cost is irrelevant next to the 5-8
// minute investigation it's about to kick off.
func (c *DevOpsAgentClient) newCommandTransport() mcp.Transport {
	cmd := exec.Command(c.binaryPath, "mcp")
	cmd.Env = append(cmd.Environ(),
		"DEVOPS_AGENT_USER_ID="+c.userID,
		"DEVOPS_AGENT_REGION="+c.region,
	)
	if c.spaceID != "" {
		cmd.Env = append(cmd.Env, "DEVOPS_AGENT_SPACE_ID="+c.spaceID)
	}
	return &mcp.CommandTransport{Command: cmd}
}

// connect opens an MCP session over c.transport.
func (c *DevOpsAgentClient) connect(ctx context.Context) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "victoria-gateway", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, c.transport(), nil)
	if err != nil {
		return nil, fmt.Errorf("connect to aws-devops-agent mcp: %w", err)
	}
	return session, nil
}

// callTool invokes one MCP tool and returns its text content. The sample
// server's tools all return a single text block containing a JSON string
// (see mcp_server.py's call_api helper), so every caller in this file
// unmarshals that text as JSON rather than inspecting structured content.
func callTool(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any) (string, error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("call %s: %w", name, err)
	}
	if res.IsError {
		return "", fmt.Errorf("%s returned an error result", name)
	}
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok && tc.Text != "" {
			return tc.Text, nil
		}
	}
	return "", fmt.Errorf("%s returned no text content", name)
}

// taskEnvelope matches the shape every backlog-task operation
// (create_backlog_task / get_backlog_task, wrapped here as
// create_investigation / get_task) actually returns: everything nested
// under a top-level "task" object.
//
// The convenience "investigate" tool in the official sample server tries
// to flatten this itself (`parsed.get("taskId")`) and gets it wrong — the
// field is at `parsed["task"]["taskId"]`, so its "started" response comes
// back with taskId/executionId always null. Confirmed 2026-08-23 against
// the real service: calling create_investigation directly and reading
// task.taskId here works; investigate does not. Filed as a known bug
// rather than worked around upstream, since this client only needs the
// tool that already works correctly.
type taskEnvelope struct {
	Task struct {
		TaskID      string `json:"taskId"`
		ExecutionID string `json:"executionId"`
		Status      string `json:"status"`
	} `json:"task"`
}

// Chat starts an AWS DevOps Agent investigation and blocks until it
// completes, returning the agent's own synthesized root-cause summary.
// The interface is the same Chat(messages, opts) every other backend in
// this package implements, so the escalation call site (pkg/aiops) doesn't
// need to know or care that this "chat" is actually a multi-minute,
// evidence-gathering investigation rather than a single completion.
//
// The technical evidence AWS DevOps Agent uses to build its answer, it
// pulls itself from the target AWS account (CloudWatch, CloudTrail,
// etc.) — that part of the package doc comment's caveat still holds. But
// the full alert context (including any RAG-retrieved history of past,
// human-confirmed incidents — see pkg/aiops.retrieveRAGContext and
// buildPrompt) is passed as the investigation's description, not
// discarded. That's deliberate: it's what makes this a hybrid-cloud
// integration rather than two unrelated systems that happen to share an
// interface — an AWS-side investigation can be grounded in what this
// deployment has already learned, homelab or AWS, not just what AWS
// DevOps Agent can see on its own. It's supplementary context, not a
// source of truth the agent is asked to trust blindly — same as every
// other RAG context use in this codebase.
func (c *DevOpsAgentClient) Chat(messages []Message, opts *ChatOptions) (string, error) {
	fullContext := lastUserMessage(messages)
	if fullContext == "" {
		return "", fmt.Errorf("aws-devops-agent: no user message to investigate")
	}
	title := fullContext
	if len(title) > 200 {
		title = title[:200]
	}
	description := fullContext
	if len(description) > 2000 {
		description = description[:2000]
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.pollTimeout+time.Minute)
	defer cancel()

	session, err := c.connect(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = session.Close() }()

	startText, err := callTool(ctx, session, "create_investigation", map[string]any{
		"title":       title,
		"priority":    c.priority,
		"description": description,
	})
	if err != nil {
		return "", fmt.Errorf("aws-devops-agent: %w", err)
	}
	var started taskEnvelope
	if err := json.Unmarshal([]byte(startText), &started); err != nil {
		return "", fmt.Errorf("aws-devops-agent: parse create_investigation response: %w", err)
	}
	if started.Task.TaskID == "" {
		return "", fmt.Errorf("aws-devops-agent: create_investigation response had no taskId: %s", startText)
	}

	executionID, err := c.pollUntilComplete(ctx, session, started.Task.TaskID, started.Task.ExecutionID)
	if err != nil {
		return "", err
	}

	return c.finalSummary(ctx, session, executionID)
}

// pollUntilComplete polls get_task every pollInterval — the cadence the
// tool's own docstring recommends — until status leaves the in-progress
// states or pollTimeout is hit.
func (c *DevOpsAgentClient) pollUntilComplete(ctx context.Context, session *mcp.ClientSession, taskID, executionID string) (string, error) {
	deadline := time.Now().Add(c.pollTimeout)
	for {
		text, err := callTool(ctx, session, "get_task", map[string]any{"task_id": taskID})
		if err != nil {
			return "", fmt.Errorf("aws-devops-agent: poll task: %w", err)
		}
		var status taskEnvelope
		if err := json.Unmarshal([]byte(text), &status); err != nil {
			return "", fmt.Errorf("aws-devops-agent: parse task status: %w", err)
		}
		if status.Task.ExecutionID != "" {
			executionID = status.Task.ExecutionID
		}
		switch status.Task.Status {
		case "COMPLETED":
			return executionID, nil
		case "FAILED":
			return "", fmt.Errorf("aws-devops-agent: investigation %s failed", taskID)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("aws-devops-agent: investigation %s did not complete within %s", taskID, c.pollTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(c.pollInterval):
		}
	}
}

type journalRecord struct {
	RecordType string `json:"recordType"`
	CreatedAt  string `json:"createdAt"`
	Content    string `json:"content"` // JSON-encoded string; shape depends on RecordType
}

type journalRecordsResponse struct {
	Records []journalRecord `json:"records"`
}

type journalMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// finalSummary reads the completed investigation's journal and returns its
// concluding summary — the agent's own account of root cause and evidence,
// after it has finished investigating and ruling out alternatives.
// Everything upstream of that (tool calls, intermediate reasoning) is left
// out; the same "don't blindly trust it" discipline this deployment
// already applies to the local triage model's output applies here too
// (see pkg/aiops and the RAG/human-confirmation loop) — this method
// returns the agent's claim, not a verified fact.
//
// A completed investigation writes a dedicated "investigation_summary_md"
// record (a ready-to-read Markdown report) as its last journal entry, so
// that's tried first. Not every execution produces one — confirmed
// 2026-08-23 against the real service, where one investigation run had it
// and an earlier one didn't — so this falls back to the last assistant
// text block from the raw message trace, which is always present.
func (c *DevOpsAgentClient) finalSummary(ctx context.Context, session *mcp.ClientSession, executionID string) (string, error) {
	text, err := callTool(ctx, session, "list_journal_records", map[string]any{
		"execution_id": executionID,
		"order":        "DESC",
		"limit":        50,
	})
	if err != nil {
		return "", fmt.Errorf("aws-devops-agent: read journal: %w", err)
	}
	var journal journalRecordsResponse
	if err := json.Unmarshal([]byte(text), &journal); err != nil {
		return "", fmt.Errorf("aws-devops-agent: parse journal: %w", err)
	}

	for _, rec := range journal.Records {
		if rec.RecordType == "investigation_summary_md" && rec.Content != "" {
			return rec.Content, nil
		}
	}

	for _, rec := range journal.Records {
		if rec.RecordType != "message" {
			continue
		}
		var msg journalMessage
		if err := json.Unmarshal([]byte(rec.Content), &msg); err != nil {
			continue // not every "message" record parses the same way (tool_result vs assistant); skip and keep looking
		}
		if msg.Role != "assistant" {
			continue
		}
		for i := len(msg.Content) - 1; i >= 0; i-- {
			if msg.Content[i].Type == "text" && msg.Content[i].Text != "" {
				return msg.Content[i].Text, nil
			}
		}
	}
	return "", fmt.Errorf("aws-devops-agent: no summary found in journal for execution %s", executionID)
}

// Available connects and calls list_agent_spaces — the tool the sample
// server's own docs describe as "typically the first call to make". It's
// cheap, read-only, and confirms both that the binary/subprocess wiring
// works and that the configured credentials can actually reach the
// service, which a bare "does the binary exist" check wouldn't.
func (c *DevOpsAgentClient) Available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	session, err := c.connect(ctx)
	if err != nil {
		return false
	}
	defer func() { _ = session.Close() }()

	_, err = callTool(ctx, session, "list_agent_spaces", nil)
	return err == nil
}

func (c *DevOpsAgentClient) ModelName() string {
	return "aws-devops-agent"
}

func (c *DevOpsAgentClient) Backend() string {
	return "aws-devops-agent"
}

// lastUserMessage returns the most recent non-system message's content,
// which is what carries the actual alert/question in every call site in
// this codebase (see pkg/aiops) — the system prompt built for a plain
// chat-completion backend doesn't apply here; AWS DevOps Agent forms its
// own investigation plan from the title alone.
func lastUserMessage(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "system" && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}
