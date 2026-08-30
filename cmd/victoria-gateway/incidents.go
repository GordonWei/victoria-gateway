// incidents.go serves the read-only web view of confirmed incidents:
// GET /incidents (recent list) and GET /incidents/{id} (one record).
// It exists so a similar-incident reference in a notification always has
// somewhere to link — records confirmed via `note --alert-name` never
// got a tracker issue, and without these pages they'd be reachable only
// by querying Postgres by hand. Confirmed records only: a pending
// record's summary is an unverified LLM guess, not something to hand a
// paged operator as reference material.
package main

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// Templates use a dark, dependency-free inline style — the design doc's
// reasoning: the reader is usually on-call, on a phone, at night.
const incidentsBaseCSS = `
body { background: #16181d; color: #d7dae0; font-family: -apple-system, "Segoe UI", Roboto, sans-serif; margin: 0; padding: 1.5rem; line-height: 1.6; }
a { color: #7aa2f7; text-decoration: none; }
a:hover { text-decoration: underline; }
h1 { font-size: 1.2rem; border-bottom: 1px solid #2a2e37; padding-bottom: .5rem; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #2a2e37; vertical-align: top; }
th { color: #8b90a0; font-weight: 600; white-space: nowrap; }
.k { color: #8b90a0; white-space: nowrap; }
.resolution { background: #1d2430; border-left: 3px solid #7aa2f7; padding: .6rem .8rem; white-space: pre-wrap; }
.mono { font-family: ui-monospace, "SF Mono", Menlo, monospace; font-size: .85em; white-space: pre-wrap; word-break: break-word; }
.muted { color: #6a6f7f; font-size: .85em; }
`

var incidentListTmpl = template.Must(template.New("list").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>victoria-gateway incidents</title><style>` + incidentsBaseCSS + `</style></head>
<body>
<h1>已確認事件（最近 {{len .Records}} 筆）</h1>
{{if .Records}}
<table>
<tr><th>ID</th><th>告警</th><th>主機</th><th>確認時間</th><th>處置摘要</th></tr>
{{range .Records}}
<tr>
<td><a href="/incidents/{{.ID}}">{{.ID}}</a></td>
<td>{{.AlertName}}</td>
<td>{{.Host}}</td>
<td class="k">{{if .ConfirmedAt.IsZero}}—{{else}}{{.ConfirmedAt.Format "2006-01-02"}}{{end}}</td>
<td>{{.Resolution}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="muted">還沒有任何已確認的事件。</p>
{{end}}
<p class="muted">victoria-gateway · 只列 Confirmed 記錄；?limit=N 可調（上限 100）</p>
</body></html>`))

var incidentDetailTmpl = template.Must(template.New("detail").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>incident/{{.ID}} — {{.AlertName}}</title><style>` + incidentsBaseCSS + `</style></head>
<body>
<h1>incident/{{.ID}} — {{.AlertName}}</h1>
<table>
<tr><td class="k">主機</td><td>{{.Host}}</td></tr>
<tr><td class="k">發生時間</td><td>{{.CreatedAt.Format "2006-01-02 15:04:05 MST"}}</td></tr>
<tr><td class="k">確認時間</td><td>{{if .ConfirmedAt.IsZero}}—{{else}}{{.ConfirmedAt.Format "2006-01-02 15:04:05 MST"}}{{end}}</td></tr>
{{if .GiteaIssueNumber}}<tr><td class="k">Issue</td><td>#{{.GiteaIssueNumber}}</td></tr>{{end}}
</table>
<h1>後來確認的處置</h1>
<div class="resolution">{{.Resolution}}</div>
<h1>當時的分析摘要</h1>
<div class="mono">{{.Summary}}</div>
{{if .LogExcerpt}}<h1>當時的 log 片段</h1><div class="mono">{{.LogExcerpt}}</div>{{end}}
<p class="muted"><a href="/incidents">← 全部事件</a></p>
</body></html>`))

// handleIncidentsList serves GET /incidents. Registered only when RAG is
// enabled (see runServe), so h.rag is always non-nil here.
func (h *handler) handleIncidentsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > 100 {
		limit = 100
	}
	records, err := h.rag.ListConfirmed(r.Context(), limit)
	if err != nil {
		log.Printf("incidents: list failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := incidentListTmpl.Execute(w, struct{ Records []rag.Record }{records}); err != nil {
		log.Printf("incidents: render list: %v", err)
	}
}

// handleIncidentDetail serves GET /incidents/{id}.
func (h *handler) handleIncidentDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/incidents/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	rec, err := h.rag.GetConfirmed(r.Context(), id)
	if err != nil {
		if errors.Is(err, rag.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Printf("incidents: get %d failed: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := incidentDetailTmpl.Execute(w, rec); err != nil {
		log.Printf("incidents: render detail: %v", err)
	}
}
