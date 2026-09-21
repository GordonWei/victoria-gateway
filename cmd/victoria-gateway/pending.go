// pending.go serves the pending-record web pages: GET /pending (list),
// GET /pending/{id} (one record + a confirm form), and POST /pending/{id}
// (submit the confirm form). It exists so an operator can find a pending
// record's id from a browser instead of only from a notification message
// or a hand-run Postgres query — and, once they're looking at it, actually
// confirm it from the same phone that received the alert (see the confirm
// form's doc comment for why that gap mattered).
//
// Deliberately not merged into incidents.go's list/detail templates:
// these are unverified LLM guesses, not confirmed resolutions, and must
// never render in the same table as ListConfirmed's output without a
// clear label saying so.
package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gordonwei/victoria-gateway/pkg/audit"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

var pendingListTmpl = template.Must(template.New("pending-list").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>victoria-gateway pending</title><style>` + incidentsBaseCSS + incidentsFormCSS + `</style></head>
<body>
<h1>⚠️ 待確認事件（{{if or .AlertName .Host}}符合篩選條件{{else}}最近{{end}} {{len .Records}} 筆）</h1>
<p class="muted">這些是尚未經人工確認的 LLM 分析猜測，不是驗證過的處置結論。</p>
<form class="filter" method="get" action="/pending">
  <label>告警名稱<input type="text" name="alertname" value="{{.AlertName}}" placeholder="alertname 子字串"></label>
  <label>主機<input type="text" name="host" value="{{.Host}}" placeholder="host 子字串"></label>
  <label>筆數上限<input type="number" name="limit" value="{{.Limit}}" min="1" max="100" style="width:5rem"></label>
  <button type="submit">篩選</button>
</form>
{{if .Records}}
<table>
<tr><th>ID</th><th>告警</th><th>主機</th><th>發生時間</th><th>LLM 摘要（未經驗證）</th></tr>
{{range .Records}}
<tr>
<td><a href="/pending/{{.ID}}">{{.ID}}</a></td>
<td>{{.AlertName}}</td>
<td>{{.Host}}</td>
<td class="k">{{.CreatedAt.Format "2006-01-02 15:04"}}</td>
<td>{{.Summary}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="muted">目前沒有待確認的事件。</p>
{{end}}
<p class="muted">victoria-gateway · ?limit=N 可調（上限 100）、?alertname=/?host= 可做子字串篩選 · <a href="/incidents">已確認事件 →</a></p>
</body></html>`))

var pendingDetailTmpl = template.Must(template.New("pending-detail").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>pending/{{.Record.ID}} — {{.Record.AlertName}}</title><style>` + incidentsBaseCSS + `
form.confirm { margin-top: 1rem; }
form.confirm textarea { width: 100%; box-sizing: border-box; background: #1d2430; border: 1px solid #2a2e37; color: #d7dae0; padding: .6rem; border-radius: 4px; font-size: .95rem; font-family: inherit; min-height: 6rem; }
form.confirm button { margin-top: .6rem; background: #2f5d3a; border: 1px solid #3f7a4e; color: #d7dae0; padding: .5rem 1.2rem; border-radius: 4px; cursor: pointer; font-size: 1rem; }
form.confirm button:hover { background: #386f46; }
.warn { background: #3a2a1d; border-left: 3px solid #d18b3a; padding: .6rem .8rem; margin: 1rem 0; }
` + `</style></head>
<body>
<h1>⚠️ pending/{{.Record.ID}} — {{.Record.AlertName}}</h1>
<div class="warn">尚未確認：以下摘要是 LLM 當時的分析猜測，不是驗證過的結論。</div>
<table>
<tr><td class="k">主機</td><td>{{.Record.Host}}</td></tr>
<tr><td class="k">發生時間</td><td>{{.Record.CreatedAt.Format "2006-01-02 15:04:05 MST"}}</td></tr>
{{if .Record.GiteaIssueNumber}}<tr><td class="k">Issue</td><td>#{{.Record.GiteaIssueNumber}}</td></tr>{{end}}
</table>
<h1>當時的分析摘要</h1>
<div class="mono">{{.Record.Summary}}</div>
{{if .Record.LogExcerpt}}<h1>當時的 log 片段</h1><div class="mono">{{.Record.LogExcerpt}}</div>{{end}}
{{if .JustConfirmed}}
<div class="warn" style="border-left-color:#3a7a4e;background:#1d2f22;">✅ 已確認，感謝回報。</div>
{{else if .AlreadyConfirmed}}
<div class="warn">這筆已經被別人確認過了，沒有東西可以再確認。</div>
{{else}}
<h1>確認這是什麼</h1>
<form class="confirm" method="post" action="/pending/{{.Record.ID}}">
  <textarea name="resolution" placeholder="實際原因是什麼、怎麼處理的（會存進去，供未來類似告警參考）" required></textarea>
  <br><button type="submit">確認</button>
</form>
{{end}}
<p class="muted"><a href="/pending">← 全部待確認事件</a></p>
</body></html>`))

// handlePendingList serves GET /pending. Registered only when RAG is
// enabled (see runServe), so h.rag is always non-nil here.
func (h *handler) handlePendingList(w http.ResponseWriter, r *http.Request) {
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
	filter := rag.ListFilter{
		AlertName: r.URL.Query().Get("alertname"),
		Host:      r.URL.Query().Get("host"),
	}
	records, err := h.rag.ListPending(r.Context(), filter, limit)
	if err != nil {
		log.Printf("pending: list failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := struct {
		Records   []rag.Record
		AlertName string
		Host      string
		Limit     int
	}{records, filter.AlertName, filter.Host, limit}
	if err := pendingListTmpl.Execute(w, page); err != nil {
		log.Printf("pending: render list: %v", err)
	}
}

// handlePendingDetail serves GET and POST /pending/{id}. GET shows the
// record and (if still pending) a confirm form; POST submits that form.
// Both share one handler because they render the same template — a
// failed or repeat POST falls through to showing the same page a GET
// would, rather than a separate error page.
func (h *handler) handlePendingDetail(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/pending/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}

	var justConfirmed, alreadyConfirmed bool
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		resolution := strings.TrimSpace(r.PostFormValue("resolution"))
		if resolution == "" {
			http.Error(w, "resolution is required", http.StatusBadRequest)
			return
		}
		// Fetched before confirming, purely to read GiteaIssueNumber for
		// the close-the-issue-too step below — ConfirmPending itself
		// doesn't need this. Best-effort: if this lookup fails (record
		// vanished between the GET that rendered this form and this
		// POST), fall through and let ConfirmPending's own error handling
		// below report ErrAlreadyConfirmed the normal way; that failure
		// mode doesn't need this record.
		preConfirm, _ := h.rag.GetPending(r.Context(), id)
		// ConfirmPending's WHERE ... AND status = 'pending' is the actual
		// concurrency guard: two submissions racing on the same id (a
		// double click, or a mail/IM client's link-prefetch opening this
		// page and a human separately submitting it) can't both "win" —
		// only the first UPDATE affects a row, the second gets
		// ErrAlreadyConfirmed. This check here is what turns that into a
		// clear message instead of a generic 500.
		switch err := h.rag.ConfirmPending(r.Context(), id, resolution); {
		case err == nil:
			justConfirmed = true
			h.recordAudit(r.Context(), audit.Entry{
				Actor:  actorFromRequest(r),
				Action: "pending.confirm",
				Target: fmt.Sprintf("id=%d", id),
				Detail: resolution,
			})
			// Best-effort, and deliberately after ConfirmPending already
			// succeeded: the database confirmation is the source of
			// truth and must not be rolled back just because the tracker
			// call that mirrors it onto the issue had a bad network day.
			// A failure here means the issue is out of sync with the DB
			// until the next `sync` tick (or someone closes it by hand)
			// — logged, not surfaced to the person confirming, who has
			// no tracker credentials to act on it anyway.
			if h.closeIssueOnWebConfirm && h.tracker != nil && preConfirm.GiteaIssueNumber != 0 {
				if cerr := h.tracker.CloseWithComment(r.Context(), preConfirm.GiteaIssueNumber, resolution); cerr != nil {
					log.Printf("pending: confirmed id=%d in DB but failed to close issue #%d: %v", id, preConfirm.GiteaIssueNumber, cerr)
				}
			}
		case errors.Is(err, rag.ErrAlreadyConfirmed):
			alreadyConfirmed = true
		default:
			log.Printf("pending: confirm %d failed: %v", id, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	} else if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// After a successful confirm the record is no longer Pending, so
	// GetPending won't find it — render from what we already have instead
	// of re-fetching. On GET (or a failed/duplicate POST) it's still
	// Pending, so fetch normally.
	var rec rag.Record
	if justConfirmed {
		rec, err = h.rag.GetPending(r.Context(), id) // best-effort; falls through to GetConfirmed below if already moved
		if err != nil {
			if confirmed, cerr := h.getConfirmedForPendingView(r, id); cerr == nil {
				rec = confirmed
			}
		}
	} else {
		rec, err = h.rag.GetPending(r.Context(), id)
		if err != nil {
			if errors.Is(err, rag.ErrNotFound) {
				// Either it never existed, or someone else confirmed it
				// between when this page linked here and now.
				alreadyConfirmed = true
				if confirmed, cerr := h.getConfirmedForPendingView(r, id); cerr == nil {
					rec = confirmed
				} else {
					http.NotFound(w, r)
					return
				}
			} else {
				log.Printf("pending: get %d failed: %v", id, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := struct {
		Record           rag.Record
		JustConfirmed    bool
		AlreadyConfirmed bool
	}{rec, justConfirmed, alreadyConfirmed}
	if err := pendingDetailTmpl.Execute(w, page); err != nil {
		log.Printf("pending: render detail: %v", err)
	}
}

// getConfirmedForPendingView looks up id as a Confirmed record, for the
// case where /pending/{id} is loaded after the record has already moved
// out of Pending (this handler's own confirm, or someone else's, or the
// `note`/`sync` CLI paths) — so the page can still show what it actually
// says instead of a bare 404.
func (h *handler) getConfirmedForPendingView(r *http.Request, id int64) (rag.Record, error) {
	return h.rag.GetConfirmed(r.Context(), id)
}
