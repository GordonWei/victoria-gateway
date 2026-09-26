package main

import (
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/gordonwei/victoria-gateway/pkg/aiops"
	"github.com/gordonwei/victoria-gateway/pkg/model"
)

// maxMitigationRunes caps the mitigation plan quoted into a tracker
// issue. GitHub rejects an issue body over 65,536 characters, and the
// analysis above it can itself be a long Markdown report; this leaves
// room for both. The full plan stays in the agent's own console.
const maxMitigationRunes = 30000

// issueBody renders a tracker issue's body: the analysis, the
// escalation's mitigation plan if one came back, and the footer asking
// for a resolution comment.
func issueBody(analyzedLabel string, result aiops.SummarizeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**分析結果（%s）：**\n\n%s\n\n", analyzedLabel, result.Summary)
	if m := result.Mitigation; m != nil && m.Result == model.MitigationOK && m.Plan != "" {
		plan := m.Plan
		if utf8.RuneCountInString(plan) > maxMitigationRunes {
			plan = string([]rune(plan)[:maxMitigationRunes]) + "\n\n…（內容過長已截斷，完整版本請到來源主控台查看）"
		}
		fmt.Fprintf(&b, "## 建議處置（%s）\n\n> 以下由升級對象自動產生，未經驗證；執行任何變更前請人工審核。\n\n%s\n\n", m.Source, plan)
	}
	b.WriteString("---\n此 Issue 由 Victoria Gateway 自動建立。調查完後請在關閉前留一則留言說明實際原因/怎麼修的，`victoria-gateway sync` 會把它讀回 RAG 資料庫，供未來類似告警參考。")
	return b.String()
}

// recordMitigation logs and counts the mitigation plan outcome of a
// successful escalation to step. A nil m means the target wasn't asked
// for a plan, which is not an outcome worth counting.
func (h *handler) recordMitigation(alertName string, step escalationStep, m *model.Mitigation) {
	if m == nil {
		return
	}
	h.metrics.IncMitigationPlanTotal(step.display, m.Result)
	switch m.Result {
	case model.MitigationOK:
		log.Printf("aiops: alert %q got a mitigation plan from %s (%d chars)", alertName, step.display, utf8.RuneCountInString(m.Plan))
	case model.MitigationNone:
		log.Printf("aiops: alert %q: %s produced no mitigation plan; keeping the investigation result", alertName, step.display)
	default:
		log.Printf("aiops: alert %q: mitigation plan from %s failed, keeping the investigation result: %s", alertName, step.display, m.Err)
	}
}

// mitigationNote is the one line a notification gets about a mitigation
// plan: where to read it. When no issue was filed (no tracker, RAG off,
// or filing failed) the plan would otherwise be lost, so it's written to
// the log instead and the note says so.
func (h *handler) mitigationNote(alertName string, m *model.Mitigation, issueNumber int64) string {
	if m == nil || m.Result != model.MitigationOK || m.Plan == "" {
		return ""
	}
	if issueNumber != 0 {
		note := fmt.Sprintf("已附建議處置（%s）於 issue #%d", m.Source, issueNumber)
		if h.issueURL != nil {
			note += "：" + h.issueURL(issueNumber)
		}
		return note
	}
	log.Printf("aiops: alert %q mitigation plan (%s), no tracker issue to attach it to:\n%s", alertName, m.Source, m.Plan)
	return fmt.Sprintf("已產生建議處置（%s），但沒有建立 tracker issue，全文只在 gateway log", m.Source)
}
