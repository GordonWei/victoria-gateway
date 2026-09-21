// audit_page.go serves GET /audit — a read-only view of pkg/audit's log,
// so "who changed the maintenance windows / confirmed this / applied that
// suppression rule" is answerable from a browser instead of a hand-run
// Postgres query. Registered only when rag.audit_log is set (see
// runServe), gated by the same webUI auth as /incidents and /pending.
package main

import (
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/gordonwei/victoria-gateway/pkg/audit"
)

var auditListTmpl = template.Must(template.New("audit-list").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>victoria-gateway audit log</title><style>` + incidentsBaseCSS + `</style></head>
<body>
<h1>🔍 稽核紀錄（最近 {{len .Entries}} 筆）</h1>
<p class="muted">誰在什麼時候改了維護窗口、確認了 pending 記錄、或套用了抑制規則候選。</p>
{{if .Entries}}
<table>
<tr><th>時間</th><th>操作者</th><th>動作</th><th>對象</th><th>內容</th></tr>
{{range .Entries}}
<tr>
<td class="k">{{.Time.Format "2006-01-02 15:04:05 MST"}}</td>
<td>{{.Actor}}</td>
<td>{{.Action}}</td>
<td>{{.Target}}</td>
<td>{{.Detail}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="muted">目前沒有任何稽核紀錄。</p>
{{end}}
<p class="muted">victoria-gateway · ?limit=N 可調（上限 200）</p>
</body></html>`))

func (h *handler) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > 200 {
		limit = 200
	}
	logger := h.audit
	if logger == nil {
		logger = audit.NoopLogger{}
	}
	entries, err := logger.List(r.Context(), limit)
	if err != nil {
		log.Printf("audit: list failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := struct{ Entries []audit.Entry }{entries}
	if err := auditListTmpl.Execute(w, page); err != nil {
		log.Printf("audit: render list: %v", err)
	}
}
