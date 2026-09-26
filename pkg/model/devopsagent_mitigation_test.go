package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMitigation configures newFakeMitigationServer. The investigation
// always completes on the first poll with execution "exe-inv"; what
// happens after that is up to these fields.
type fakeMitigation struct {
	inlinePlan   string // mitigation_summary_md already on the investigation's execution
	plan         string // mitigation_summary_md written to "exe-mit" once the mitigation run completes
	noTool       bool   // server predates create_mitigation_plan (sample < v1.1.0)
	triggerError string // create_mitigation_plan answers {"error": triggerError}
	endStatus    string // status the mitigation run ends in; "" means COMPLETED
	never        bool   // the mitigation run never finishes
}

type fakeMitigationCalls struct {
	mu          sync.Mutex
	triggered   int
	journalExec []string // execution_id of every mitigation_summary_md read
}

func (c *fakeMitigationCalls) snapshot() (int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.triggered, append([]string(nil), c.journalExec...)
}

func textResult(v any) *mcp.CallToolResult {
	body, _ := json.Marshal(v)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}
}

// newFakeMitigationServer mirrors the sample server's tools closely
// enough to drive the investigation and then the mitigation phase:
// create_mitigation_plan flips the task back to PENDING_START, get_task
// reports IN_PROGRESS once and then the end status, list_executions
// lists the investigation, one of its sub-agents and the mitigation run.
func newFakeMitigationServer(t *testing.T, f fakeMitigation, calls *fakeMitigationCalls) *mcp.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-aws-devops-agent", Version: "test"}, nil)

	var mu sync.Mutex
	mitigating := false
	pollsSinceTrigger := 0

	mcp.AddTool(server, &mcp.Tool{Name: "create_investigation"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return textResult(map[string]any{"task": map[string]any{"taskId": "task-1", "executionId": "exe-inv", "status": "PENDING_START"}}), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_task"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		mu.Lock()
		defer mu.Unlock()
		status := "COMPLETED"
		if mitigating {
			pollsSinceTrigger++
			switch {
			case f.never || pollsSinceTrigger == 1:
				status = "IN_PROGRESS"
			case f.endStatus != "":
				status = f.endStatus
			}
		}
		return textResult(map[string]any{"task": map[string]any{"taskId": "task-1", "executionId": "exe-inv", "status": status}}), nil, nil
	})
	if !f.noTool {
		mcp.AddTool(server, &mcp.Tool{Name: "create_mitigation_plan"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			calls.mu.Lock()
			calls.triggered++
			calls.mu.Unlock()
			if args["task_id"] != "task-1" {
				t.Errorf("create_mitigation_plan task_id = %v, want task-1", args["task_id"])
			}
			if f.triggerError != "" {
				return textResult(map[string]any{"error": f.triggerError, "message": "denied"}), nil, nil
			}
			mu.Lock()
			mitigating = true
			mu.Unlock()
			return textResult(map[string]any{"task": map[string]any{"taskId": "task-1", "status": "PENDING_START"}}), nil, nil
		})
	}
	mcp.AddTool(server, &mcp.Tool{Name: "list_executions"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return textResult(map[string]any{"executions": []map[string]any{
			{"executionId": "exe-inv", "createdAt": "2026-09-27T10:00:00+00:00", "agentSubTask": "x"},
			{"executionId": "exe-sub", "parentExecutionId": "exe-inv", "createdAt": "2026-09-27T10:09:00+00:00"},
			{"executionId": "exe-mit", "createdAt": "2026-09-27T10:08:00+00:00"},
		}}), nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "list_journal_records"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		exec, _ := args["execution_id"].(string)
		if args["record_type"] != mitigationSummaryRecordType {
			// The investigation's own summary read.
			return textResult(map[string]any{"records": []map[string]any{
				{"recordType": "investigation_summary_md", "content": "root cause: bad deploy"},
			}}), nil, nil
		}
		calls.mu.Lock()
		calls.journalExec = append(calls.journalExec, exec)
		calls.mu.Unlock()
		var records []map[string]any
		switch {
		case exec == "exe-inv" && f.inlinePlan != "":
			records = append(records, map[string]any{"recordType": mitigationSummaryRecordType, "content": f.inlinePlan})
		case exec == "exe-mit" && f.plan != "":
			records = append(records, map[string]any{"recordType": mitigationSummaryRecordType, "content": f.plan})
		}
		return textResult(map[string]any{"records": records}), nil, nil
	})
	return server
}

func mitigationClient(t *testing.T, server *mcp.Server, enabled bool, timeout time.Duration) *DevOpsAgentClient {
	t.Helper()
	c := NewDevOpsAgentClient(DevOpsAgentClientConfig{
		PollInterval:      5 * time.Millisecond,
		PollTimeout:       5 * time.Second,
		MitigationPlan:    enabled,
		MitigationTimeout: timeout,
	})
	c.transport = func() mcp.Transport {
		st, ct := mcp.NewInMemoryTransports()
		go func() {
			if _, err := server.Connect(context.Background(), st, nil); err != nil {
				t.Logf("fake server connect: %v", err)
			}
		}()
		return ct
	}
	return c
}

func chatDetailed(t *testing.T, c *DevOpsAgentClient) ChatResult {
	t.Helper()
	res, err := c.ChatDetailed([]Message{{Role: "user", Content: "Lambda checkout errors"}}, nil)
	if err != nil {
		t.Fatalf("ChatDetailed: %v", err)
	}
	if res.Reply != "root cause: bad deploy" {
		t.Fatalf("Reply = %q, want the investigation summary", res.Reply)
	}
	return res
}

func TestDevOpsAgentMitigation_Disabled_NotRequested(t *testing.T) {
	calls := &fakeMitigationCalls{}
	c := mitigationClient(t, newFakeMitigationServer(t, fakeMitigation{plan: "p"}, calls), false, time.Second)
	res := chatDetailed(t, c)
	if res.Mitigation != nil {
		t.Errorf("Mitigation = %+v, want nil when mitigation_plan is off", res.Mitigation)
	}
	if n, reads := calls.snapshot(); n != 0 || len(reads) != 0 {
		t.Errorf("mitigation was touched while disabled: triggered=%d reads=%v", n, reads)
	}
}

func TestDevOpsAgentMitigation_TriggersAndReadsPlan(t *testing.T) {
	calls := &fakeMitigationCalls{}
	c := mitigationClient(t, newFakeMitigationServer(t, fakeMitigation{plan: "## Prepare\n1. roll back to v41"}, calls), true, 5*time.Second)
	res := chatDetailed(t, c)
	m := res.Mitigation
	if m == nil || m.Result != MitigationOK {
		t.Fatalf("Mitigation = %+v, want ok", m)
	}
	if m.Plan != "## Prepare\n1. roll back to v41" || m.Source != "AWS DevOps Agent mitigation plan" {
		t.Errorf("Mitigation = %+v", m)
	}
	n, reads := calls.snapshot()
	if n != 1 {
		t.Errorf("create_mitigation_plan called %d times, want 1", n)
	}
	for _, id := range reads {
		if id == "exe-sub" {
			t.Errorf("read a sub-agent execution's journal: %v", reads)
		}
	}
}

func TestDevOpsAgentMitigation_InlinePlan_NotTriggered(t *testing.T) {
	calls := &fakeMitigationCalls{}
	c := mitigationClient(t, newFakeMitigationServer(t, fakeMitigation{inlinePlan: "inline proposal"}, calls), true, 5*time.Second)
	res := chatDetailed(t, c)
	if res.Mitigation == nil || res.Mitigation.Result != MitigationOK || res.Mitigation.Plan != "inline proposal" {
		t.Fatalf("Mitigation = %+v, want the inline plan", res.Mitigation)
	}
	if n, _ := calls.snapshot(); n != 0 {
		t.Errorf("create_mitigation_plan called %d times; a plan already on the investigation shouldn't start (and bill) another run", n)
	}
}

func TestDevOpsAgentMitigation_NoPlan(t *testing.T) {
	calls := &fakeMitigationCalls{}
	c := mitigationClient(t, newFakeMitigationServer(t, fakeMitigation{}, calls), true, 5*time.Second)
	res := chatDetailed(t, c)
	if res.Mitigation == nil || res.Mitigation.Result != MitigationNone || res.Mitigation.Plan != "" {
		t.Fatalf("Mitigation = %+v, want none", res.Mitigation)
	}
}

func TestDevOpsAgentMitigation_Errors(t *testing.T) {
	cases := []struct {
		name    string
		f       fakeMitigation
		timeout time.Duration
		want    string
	}{
		{"api error", fakeMitigation{triggerError: "AccessDeniedException"}, 5 * time.Second, "AccessDeniedException"},
		{"old sample server", fakeMitigation{noTool: true}, 5 * time.Second, "v1.1.0"},
		{"run failed", fakeMitigation{endStatus: "FAILED", plan: "p"}, 5 * time.Second, "ended FAILED"},
		{"timeout", fakeMitigation{never: true, plan: "p"}, 50 * time.Millisecond, "no plan within 50ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := &fakeMitigationCalls{}
			c := mitigationClient(t, newFakeMitigationServer(t, tc.f, calls), true, tc.timeout)
			res := chatDetailed(t, c) // the investigation still succeeds
			m := res.Mitigation
			if m == nil || m.Result != MitigationError {
				t.Fatalf("Mitigation = %+v, want error", m)
			}
			if !strings.Contains(m.Err, tc.want) {
				t.Errorf("Err = %q, want it to mention %q", m.Err, tc.want)
			}
			if m.Plan != "" {
				t.Errorf("Plan = %q on error", m.Plan)
			}
		})
	}
}

// Chat (the plain LLM interface) must keep returning just the summary.
func TestDevOpsAgentMitigation_ChatReturnsReplyOnly(t *testing.T) {
	calls := &fakeMitigationCalls{}
	c := mitigationClient(t, newFakeMitigationServer(t, fakeMitigation{plan: "p"}, calls), true, 5*time.Second)
	reply, err := c.Chat([]Message{{Role: "user", Content: "x"}}, nil)
	if err != nil || reply != "root cause: bad deploy" {
		t.Fatalf("Chat = %q, %v", reply, err)
	}
}

func TestJournalContentText(t *testing.T) {
	for raw, want := range map[string]string{
		`"# plan"`:        "# plan",
		`null`:            "",
		``:                "",
		`{"steps":["a"]}`: `{"steps":["a"]}`,
	} {
		if got := journalContentText(json.RawMessage(raw)); got != want {
			t.Errorf("journalContentText(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestAPIError(t *testing.T) {
	if err := apiError("t", `{"task":{"taskId":"x"}}`); err != nil {
		t.Errorf("normal response read as error: %v", err)
	}
	if err := apiError("t", `not json`); err != nil {
		t.Errorf("non-JSON read as error: %v", err)
	}
	err := apiError("get_task", `{"error":"ThrottlingException","message":"slow down"}`)
	if err == nil || !strings.Contains(err.Error(), "get_task: ThrottlingException: slow down") {
		t.Errorf("apiError = %v", err)
	}
}

func TestNewDevOpsAgentClient_MitigationDefaults(t *testing.T) {
	c := NewDevOpsAgentClient(DevOpsAgentClientConfig{})
	if c.mitigationPlan {
		t.Error("mitigation plan should default to off")
	}
	if c.mitigationTimeout != DevOpsAgentDefaultMitigationTimeout {
		t.Errorf("mitigationTimeout = %s", c.mitigationTimeout)
	}
}
