package rag

import (
	"fmt"
	"strings"
)

// BuildQueryText turns an alert (plus a little log context) into the text
// that gets embedded for a similarity search. It's deliberately similar
// in shape to what `victoria-gateway note` embeds for a stored record
// (alert name + host + a log/description snippet) — a query and the
// records it's meant to match need to live in the same "kind of text"
// for cosine similarity to mean anything.
func BuildQueryText(alertName, host, description string, logLines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "告警：%s 主機：%s", alertName, host)
	if description != "" {
		fmt.Fprintf(&b, " 描述：%s", description)
	}
	const maxLines = 10
	if len(logLines) > 0 {
		b.WriteString(" log：")
		lines := logLines
		if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		b.WriteString(strings.Join(lines, " | "))
	}
	return b.String()
}

// FormatContext renders retrieved records into the block that gets
// inserted into the summarizer prompt (see aiops.buildPrompt's
// ragContext parameter). Returns "" for an empty slice so callers can
// pass the result straight through without an extra empty check.
func FormatContext(records []Record) string {
	if len(records) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range records {
		fmt.Fprintf(&b, "%d. [%s] %s（主機：%s）\n   後來確認：%s\n",
			i+1, r.CreatedAt.Format("2006-01-02"), r.AlertName, r.Host, r.Resolution)
	}
	return b.String()
}
