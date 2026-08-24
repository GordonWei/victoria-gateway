package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newFakeDevOpsAgentServer registers handlers matching the real
// aws-devops-agent MCP tool schemas (investigate/get_task/
// list_journal_records/list_agent_spaces — see mcp_server.py in
// aws-samples/sample-aws-devops-agent-acp-mcp) closely enough to drive
// DevOpsAgentClient's polling and summary-extraction logic the same way
// the real subprocess would, without shelling out to Python in tests.
// capturedCreateInvestigationArgs, if non-nil, records the arguments the
// fake server's create_investigation handler was called with — used to
// assert that Chat() actually sends RAG/log context as the investigation
// description, not just a bare title.
func newFakeDevOpsAgentServer(t *testing.T, statusesBeforeComplete int, capturedCreateInvestigationArgs *map[string]any) *mcp.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-aws-devops-agent", Version: "test"}, nil)

	getTaskCalls := 0

	// create_investigation and get_task both proxy the backlog-task API
	// directly and return everything nested under "task" — see the
	// taskEnvelope doc comment in devopsagent.go for why the client
	// deliberately does NOT use the sibling "investigate" convenience
	// tool, which flattens this shape incorrectly in the real server.
	mcp.AddTool(server, &mcp.Tool{Name: "create_investigation"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		if capturedCreateInvestigationArgs != nil {
			*capturedCreateInvestigationArgs = args
		}
		body, _ := json.Marshal(map[string]any{
			"task": map[string]any{
				"taskId":      "task-123",
				"executionId": "exe-456",
				"status":      "PENDING_START",
			},
		})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "get_task"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		getTaskCalls++
		status := "IN_PROGRESS"
		if getTaskCalls > statusesBeforeComplete {
			status = "COMPLETED"
		}
		body, _ := json.Marshal(map[string]any{
			"task": map[string]any{
				"status":      status,
				"executionId": "exe-456",
			},
		})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "list_journal_records"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		// Mirrors the real shape: a "message" record whose content field is
		// itself a JSON-encoded string containing role + content blocks.
		msg, _ := json.Marshal(map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "root cause: intentionally broken handler.py"},
			},
		})
		records := []map[string]any{
			{"recordType": "message", "content": string(msg)},
		}
		body, _ := json.Marshal(map[string]any{"records": records})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "list_agent_spaces"}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		body, _ := json.Marshal(map[string]any{"agentSpaces": []any{}})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
	})

	return server
}

// clientWithFakeServer wires a DevOpsAgentClient's transport to an
// in-memory MCP session backed by the given server, so Chat()/Available()
// exercise the real client code (marshaling, polling loop, journal
// parsing) end-to-end without a subprocess.
func clientWithFakeServer(t *testing.T, server *mcp.Server, pollInterval time.Duration) *DevOpsAgentClient {
	t.Helper()
	client := NewDevOpsAgentClient(DevOpsAgentClientConfig{
		UserID:       "test-user",
		Region:       "us-east-1",
		PollInterval: pollInterval,
		PollTimeout:  5 * time.Second,
	})
	client.transport = func() mcp.Transport {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		ctx := context.Background()
		go func() {
			if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
				t.Logf("fake server connect: %v", err)
			}
		}()
		return clientTransport
	}
	return client
}

func TestDevOpsAgentClient_Chat_PollsUntilCompleteAndReturnsSummary(t *testing.T) {
	server := newFakeDevOpsAgentServer(t, 2, nil) // IN_PROGRESS twice, then COMPLETED
	client := clientWithFakeServer(t, server, 10*time.Millisecond)

	reply, err := client.Chat([]Message{
		{Role: "system", Content: "you are a triage assistant"},
		{Role: "user", Content: "Investigate elevated error rate on victoria-gateway-demo-checkout-service"},
	}, nil)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if reply != "root cause: intentionally broken handler.py" {
		t.Fatalf("unexpected reply: %q", reply)
	}
}

// TestDevOpsAgentClient_Chat_SendsFullContextAsDescription locks in the
// hybrid-cloud integration point: RAG-retrieved history of past,
// human-confirmed incidents (baked into the built prompt by
// pkg/aiops.buildPrompt before it ever reaches this package) must reach
// AWS DevOps Agent as investigation context, not get discarded down to a
// bare title. See the DevOpsAgentClient doc comment.
func TestDevOpsAgentClient_Chat_SendsFullContextAsDescription(t *testing.T) {
	var captured map[string]any
	server := newFakeDevOpsAgentServer(t, 0, &captured)
	client := clientWithFakeServer(t, server, 10*time.Millisecond)

	fullContext := "告警名稱：VictoriaGatewayDemoCheckoutServiceErrors\n" +
		"主機：victoria-gateway-demo-checkout-service\n" +
		"狀態：firing\n\n" +
		"過去類似事件（供參考，不代表這次一定是同樣原因）：\n" +
		"- 2026-08-10 同一支 Lambda 曾經因為 IAM role 少了一個 policy 導致 AccessDenied，後來補上 policy 解決\n\n" +
		"相關 log：\n[13:00:00] [ERROR] DependencyTimeoutError: payment-processor did not respond within 3000ms\n"

	_, err := client.Chat([]Message{
		{Role: "system", Content: "you are a triage assistant"},
		{Role: "user", Content: fullContext},
	}, nil)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}

	if captured == nil {
		t.Fatal("create_investigation was never called")
	}
	description, _ := captured["description"].(string)
	if description == "" {
		t.Fatal("create_investigation was called with an empty description")
	}
	if !strings.Contains(description, "過去類似事件") {
		t.Errorf("description does not include the RAG-retrieved history section:\n%s", description)
	}
	if !strings.Contains(description, "IAM role 少了一個 policy") {
		t.Errorf("description does not include the specific past-incident content:\n%s", description)
	}

	title, _ := captured["title"].(string)
	if title == "" {
		t.Error("create_investigation was called with an empty title")
	}
}

func TestDevOpsAgentClient_Chat_NoUserMessage(t *testing.T) {
	client := NewDevOpsAgentClient(DevOpsAgentClientConfig{})
	_, err := client.Chat([]Message{{Role: "system", Content: "no user turn here"}}, nil)
	if err == nil {
		t.Fatal("expected error for missing user message, got nil")
	}
}

func TestDevOpsAgentClient_Available(t *testing.T) {
	server := newFakeDevOpsAgentServer(t, 0, nil)
	client := clientWithFakeServer(t, server, 10*time.Millisecond)

	if !client.Available() {
		t.Fatal("expected Available() to return true against a working fake server")
	}
}

func TestDevOpsAgentClient_Available_ConnectFailure(t *testing.T) {
	client := NewDevOpsAgentClient(DevOpsAgentClientConfig{BinaryPath: "this-binary-does-not-exist-anywhere"})
	if client.Available() {
		t.Fatal("expected Available() to return false when the binary can't be found")
	}
}

func TestDevOpsAgentClient_ModelNameAndBackend(t *testing.T) {
	client := NewDevOpsAgentClient(DevOpsAgentClientConfig{})
	if client.ModelName() != "aws-devops-agent" {
		t.Errorf("ModelName() = %q", client.ModelName())
	}
	if client.Backend() != "aws-devops-agent" {
		t.Errorf("Backend() = %q", client.Backend())
	}
}

func TestLastUserMessage(t *testing.T) {
	got := lastUserMessage([]Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	})
	if got != "second" {
		t.Errorf("lastUserMessage() = %q, want %q", got, "second")
	}

	if got := lastUserMessage([]Message{{Role: "system", Content: "only system"}}); got != "" {
		t.Errorf("lastUserMessage() with only a system message = %q, want empty", got)
	}
}
