package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/metrics"
	"github.com/gordonwei/victoria-gateway/pkg/model"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// mitigatingCloud is an escalation target that answers like an
// aws-devops-agent target with mitigation_plan on: a Markdown summary
// plus a Mitigation.
type mitigatingCloud struct {
	reply      string
	mitigation *model.Mitigation
}

func (m *mitigatingCloud) Chat([]model.Message, *model.ChatOptions) (string, error) {
	return m.reply, nil
}
func (m *mitigatingCloud) ChatDetailed([]model.Message, *model.ChatOptions) (model.ChatResult, error) {
	return model.ChatResult{Reply: m.reply, Mitigation: m.mitigation}, nil
}
func (m *mitigatingCloud) Available() bool   { return true }
func (m *mitigatingCloud) ModelName() string { return "aws-devops-agent" }
func (m *mitigatingCloud) Backend() string   { return "aws-devops-agent" }

func planOK(plan string) *model.Mitigation {
	return &model.Mitigation{Source: "AWS DevOps Agent mitigation plan", Plan: plan, Result: model.MitigationOK}
}

// mitigationHandler builds a handler whose alerts always escalate (via
// always_cloud) to cloud. withTracker also turns on RAG capture with a
// fake tracker, which is what files an issue at all.
func mitigationHandler(t *testing.T, cloud model.LLM, withTracker bool) (*handler, *fakeTracker) {
	t.Helper()
	lokiSrv := newFakeLoki(t)
	t.Cleanup(lokiSrv.Close)
	llmSrv := newFakeLLM(t, `{"summary":"local","confidence":"high","escalate":false,"reason":"x"}`)
	t.Cleanup(llmSrv.Close)

	h := newTestHandler(t, lokiSrv.URL, llmSrv.URL)
	h.cloud = cloud
	h.escalation = config.EscalationConfig{AlwaysCloud: []string{"LambdaErrors"}}
	h.metrics = &metrics.Counters{}
	if !withTracker {
		return h, nil
	}
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{0.1}}}})
	}))
	t.Cleanup(embedSrv.Close)
	tr := &fakeTracker{nextIssue: 41}
	h.rag = &fakeRAGStore{}
	h.ragEmbedder = rag.NewEmbedder(embedSrv.URL, "bge-m3", "")
	h.tracker = tr
	h.issueURL = func(n int64) string { return fmt.Sprintf("https://git.example/issues/%d", n) }
	return h, tr
}

func lambdaAlert() aiops.Alert {
	return aiops.Alert{
		Status:   "firing",
		Labels:   map[string]string{"alertname": "LambdaErrors", "host": "checkout"},
		StartsAt: time.Now().Add(-time.Minute).Format(time.RFC3339),
	}
}

func TestSummarizeOne_MitigationPlan_AttachedToIssue(t *testing.T) {
	cloud := &mitigatingCloud{reply: "## Root cause\nbad deploy", mitigation: planOK("### Apply\n1. roll back to v41")}
	h, tr := mitigationHandler(t, cloud, true)

	res := h.summarizeOne(lambdaAlert())

	if res.AnalyzedBy != "cloud" || res.Summary != "## Root cause\nbad deploy" {
		t.Fatalf("res = %+v, want the cloud analysis", res)
	}
	if len(tr.bodies) != 1 {
		t.Fatalf("issues filed = %d, want 1", len(tr.bodies))
	}
	body := tr.bodies[0]
	heading := "## 建議處置（AWS DevOps Agent mitigation plan）"
	for _, want := range []string{"## Root cause\nbad deploy", heading, "### Apply\n1. roll back to v41", "未經驗證", "victoria-gateway sync"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
	// Analysis first, then the plan, then the footer.
	analysis, plan, footer := strings.Index(body, "bad deploy"), strings.Index(body, heading), strings.Index(body, "victoria-gateway sync")
	if analysis > plan || plan > footer {
		t.Errorf("issue body sections out of order:\n%s", body)
	}
	if want := "已附建議處置（AWS DevOps Agent mitigation plan）於 issue #42：https://git.example/issues/42"; res.mitigationNote != want {
		t.Errorf("mitigationNote = %q, want %q", res.mitigationNote, want)
	}
	if strings.Contains(res.Summary, "roll back") {
		t.Error("the plan leaked into the notification summary")
	}
	if got := scrape(t, h.metrics); !strings.Contains(got, `victoria_gateway_mitigation_plan_total{target="aws-devops-agent",result="ok"} 1`) {
		t.Errorf("metric missing:\n%s", got)
	}
}

func TestSummarizeOne_MitigationPlan_NoneOrError_IssueWithoutSection(t *testing.T) {
	for _, m := range []*model.Mitigation{
		{Source: "S", Result: model.MitigationNone},
		{Source: "S", Result: model.MitigationError, Err: "AccessDeniedException"},
		nil,
	} {
		cloud := &mitigatingCloud{reply: "cloud analysis", mitigation: m}
		h, tr := mitigationHandler(t, cloud, true)
		res := h.summarizeOne(lambdaAlert())

		if res.Error != "" || res.AnalyzedBy != "cloud" || res.Summary != "cloud analysis" {
			t.Fatalf("mitigation %+v changed the analysis: %+v", m, res)
		}
		if len(tr.bodies) != 1 || strings.Contains(tr.bodies[0], "建議處置") {
			t.Errorf("mitigation %+v: issue bodies = %q, want one without a plan section", m, tr.bodies)
		}
		if res.mitigationNote != "" {
			t.Errorf("mitigation %+v: note = %q, want none", m, res.mitigationNote)
		}
		got := scrape(t, h.metrics)
		want := "victoria_gateway_mitigation_plan_total 0\n" // an unrequested plan isn't counted
		if m != nil {
			want = fmt.Sprintf(`victoria_gateway_mitigation_plan_total{target="aws-devops-agent",result="%s"} 1`, m.Result)
		}
		if !strings.Contains(got, want) {
			t.Errorf("mitigation %+v: metric %q missing:\n%s", m, want, got)
		}
	}
}

func TestSummarizeOne_MitigationPlan_NoTracker(t *testing.T) {
	cloud := &mitigatingCloud{reply: "cloud analysis", mitigation: planOK("roll back")}
	h, _ := mitigationHandler(t, cloud, false)

	res := h.summarizeOne(lambdaAlert())

	if res.Error != "" || res.Summary != "cloud analysis" {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.mitigationNote, "沒有建立 tracker issue") {
		t.Errorf("mitigationNote = %q, want it to say where the plan went", res.mitigationNote)
	}
}

// The local-summarizer-down path hands the alert straight to the
// escalation target; the plan must come along there too.
func TestSummarizeOne_MitigationPlan_LocalUnavailablePath(t *testing.T) {
	cloud := &mitigatingCloud{reply: "cloud analysis", mitigation: planOK("roll back")}
	h, tr := mitigationHandler(t, cloud, true)
	h.summarizer = aiops.NewSummarizer(model.NewOpenAIClient(model.OpenAIClientConfig{Endpoint: "http://127.0.0.1:1", Model: "m", Backend: "test"}))

	res := h.summarizeOne(lambdaAlert())

	if res.AnalyzedBy != "cloud" {
		t.Fatalf("res = %+v, want the escalation to answer", res)
	}
	if len(tr.bodies) != 1 || !strings.Contains(tr.bodies[0], "roll back") {
		t.Errorf("issue bodies = %q, want the plan attached", tr.bodies)
	}
	if !strings.Contains(res.mitigationNote, "issue #42") {
		t.Errorf("mitigationNote = %q", res.mitigationNote)
	}
}

func TestIssueBody_TruncatesLongPlan(t *testing.T) {
	plan := strings.Repeat("處", maxMitigationRunes+10)
	body := issueBody("cloud → aws", aiops.SummarizeResult{Summary: "s", Mitigation: planOK(plan)})
	if strings.Contains(body, plan) {
		t.Error("an over-long plan was not truncated")
	}
	if !strings.Contains(body, strings.Repeat("處", maxMitigationRunes)) || !strings.Contains(body, "內容過長已截斷") {
		t.Error("truncated plan should keep maxMitigationRunes runes and say it was cut")
	}
}

func TestIssueBody_WithoutMitigation_Unchanged(t *testing.T) {
	got := issueBody("local", aiops.SummarizeResult{Summary: "CPU 過載"})
	want := "**分析結果（local）：**\n\nCPU 過載\n\n---\n此 Issue 由 Victoria Gateway 自動建立。調查完後請在關閉前留一則留言說明實際原因/怎麼修的，`victoria-gateway sync` 會把它讀回 RAG 資料庫，供未來類似告警參考。"
	if got != want {
		t.Errorf("issue body changed for alerts without a plan:\n got %q\nwant %q", got, want)
	}
}

func TestPollBeyondGraceNotes_CountsMitigationTimeout(t *testing.T) {
	cfg := &config.Config{
		ShutdownGraceSec: 800, // above the 10m investigation alone, below 10m + 5m
		Cloud: &config.CloudConfig{Provider: "aws-devops-agent", DevOpsAgent: &config.DevOpsAgentConfig{
			MitigationPlan: true,
		}},
	}
	notes := pollBeyondGraceNotes(cfg)
	if len(notes) != 1 || !strings.Contains(notes[0], "15m0s") {
		t.Errorf("notes = %q, want one warning counting 10m investigation + 5m mitigation", notes)
	}
	cfg.Cloud.DevOpsAgent.MitigationPlan = false
	if notes := pollBeyondGraceNotes(cfg); len(notes) != 0 {
		t.Errorf("notes = %q, want none without mitigation_plan", notes)
	}
}
