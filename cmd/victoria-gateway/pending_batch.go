// pending_batch.go serves POST /pending/batch: confirm several groups of
// recurring alerts in one submission, all with the same resolution.
//
// Why this is separate from the per-record confirm in pending.go: that one
// confirms a record an operator has actually read. This one confirms a
// whole (alert_name, host) group from the list page, where they have read
// the representative and are asserting the rest are the same thing. The
// page says so in as many words — that assertion is the operator's to
// make, but it should not be hidden behind a button that looks like the
// single-record one.
package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gordonwei/victoria-gateway/pkg/audit"
)

// maxBatchGroups caps one submission. The list page shows at most 100
// groups, so normal use never reaches this; it bounds how much one request
// can change for anything that posts here directly.
const maxBatchGroups = 200

var batchDoneTmpl = template.Must(template.New("pending-batch-done").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>批次確認結果</title><style>` + incidentsBaseCSS + `
.ok { background:#1d2f22; border-left:3px solid #3a7a4e; padding:.6rem .8rem; margin:1rem 0; }
.warn { background:#3a2a1d; border-left:3px solid #d18b3a; padding:.6rem .8rem; margin:1rem 0; }
</style></head>
<body>
<h1>{{.Title}}</h1>
<div class="{{if .OK}}ok{{else}}warn{{end}}">{{.Message}}</div>
<p class="muted"><a href="/pending">← 回到待確認清單</a></p>
</body></html>`))

type batchDone struct {
	Title   string
	Message string
	OK      bool
}

// handlePendingBatch serves POST /pending/batch.
func (h *handler) handlePendingBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.rag == nil {
		http.Error(w, "rag is disabled", http.StatusServiceUnavailable)
		return
	}
	grouper, ok := h.rag.(pendingGrouper)
	if !ok || !h.batchConfirm {
		// Reachable if someone posts here on a deployment that hasn't run
		// migrate_0003 — the list page wouldn't have rendered the form.
		h.renderBatchDone(w, batchDone{
			Title:   "批次確認未啟用",
			Message: "這個資料庫還沒有 dup_of 欄位。跑 pkg/rag/migrate_0003_dup_of.sql 後重啟即可。",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	resolution := strings.TrimSpace(r.PostForm.Get("resolution"))
	if resolution == "" {
		h.renderBatchDone(w, batchDone{
			Title:   "沒有填寫結論",
			Message: "批次確認會把同一份結論寫進每一筆，所以結論不能空白。",
		})
		return
	}
	reps := r.PostForm["rep"]
	if len(reps) == 0 {
		h.renderBatchDone(w, batchDone{
			Title:   "沒有勾選任何群組",
			Message: "請至少勾選一列再送出。",
		})
		return
	}
	if len(reps) > maxBatchGroups {
		h.renderBatchDone(w, batchDone{
			Title:   "一次送出的群組太多",
			Message: fmt.Sprintf("一次最多 %d 群，這次收到 %d 群。", maxBatchGroups, len(reps)),
		})
		return
	}

	ctx := r.Context()
	actor := actorFromRequest(r, h.webUIAuth != nil)
	var (
		confirmedRows int
		doneGroups    int
		skippedGroups int
		badIDs        int
		failures      []string
	)
	for _, s := range reps {
		repID, err := strconv.ParseInt(s, 10, 64)
		if err != nil || repID <= 0 {
			badIDs++
			continue
		}
		ids, err := grouper.GroupMemberIDs(ctx, repID)
		if err != nil {
			log.Printf("pending batch: group members for %d: %v", repID, err)
			failures = append(failures, fmt.Sprintf("#%d 查詢同群成員失敗", repID))
			continue
		}
		if len(ids) == 0 {
			// Already confirmed by someone else between page load and submit.
			skippedGroups++
			continue
		}
		n, err := grouper.ConfirmPendingGroup(ctx, ids, resolution, repID)
		if err != nil {
			log.Printf("pending batch: confirm group %d: %v", repID, err)
			failures = append(failures, fmt.Sprintf("#%d 確認失敗", repID))
			continue
		}
		if n == 0 {
			skippedGroups++
			continue
		}
		confirmedRows += n
		doneGroups++
		log.Printf("pending batch: confirmed group rep=%d rows=%d by=%s", repID, n, actor)
		// One audit row per group, not per record: the operator made one
		// decision about one group, and N rows for a single click would
		// bury every other action under the biggest group. The row count
		// goes in Detail instead.
		h.recordAudit(ctx, audit.Entry{
			Actor:  actor,
			Action: "pending.batch_confirm",
			Target: fmt.Sprintf("id=%d", repID),
			Detail: fmt.Sprintf("批次確認同群 %d 筆：%s", n, resolution),
		})
	}

	msg := batchMessage(doneGroups, confirmedRows, skippedGroups, badIDs, failures)
	if doneGroups == 0 {
		h.renderBatchDone(w, batchDone{Title: "沒有任何群組被確認", Message: msg})
		return
	}
	h.renderBatchDone(w, batchDone{Title: "批次確認完成", Message: msg, OK: true})
}

// batchMessage says what happened, including everything that didn't.
//
// Reporting only the successes would let an operator who submitted 20
// groups and had 12 go through read "確認完成" and believe the list is
// clear. A failure nobody is told about is worse than the failure.
func batchMessage(groups, rows, skipped, badIDs int, failures []string) string {
	var b strings.Builder
	if groups > 0 {
		fmt.Fprintf(&b, "已確認 %d 群、共 %d 筆。", groups, rows)
		if rows > groups {
			fmt.Fprintf(&b, "其中 %d 筆標記為同群代表列的重複——"+
				"紀錄完整保留、/incidents 照樣看得到，只是不會重複進入 RAG 檢索。", rows-groups)
		}
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "有 %d 群在你送出前已被確認過，這次略過（不會覆蓋先前的結論）。", skipped)
	}
	if badIDs > 0 {
		fmt.Fprintf(&b, "有 %d 個送上來的 id 無法解析，已忽略。", badIDs)
	}
	if len(failures) > 0 {
		fmt.Fprintf(&b, "另有 %d 群處理失敗：%s。", len(failures), strings.Join(failures, "、"))
	}
	if b.Len() == 0 {
		return "沒有任何群組符合條件。"
	}
	return b.String()
}

func (h *handler) renderBatchDone(w http.ResponseWriter, d batchDone) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := batchDoneTmpl.Execute(w, d); err != nil {
		log.Printf("pending batch: render: %v", err)
	}
}
