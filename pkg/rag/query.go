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
//
// Field labels are plain ASCII key=value tokens, not natural-language
// words in any one language. This is public source under an MIT license;
// someone can point rag.embedding_model at a different (non-multilingual)
// model, and a hardcoded "告警：...主機：..." sentence would then embed as
// Chinese labels wrapped around whatever-language content — pure noise to
// a model that isn't bge-m3. key=value tokens carry the same structure
// without asserting a language.
func BuildQueryText(alertName, host, description string, logLines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "alert=%s host=%s", alertName, host)
	if description != "" {
		fmt.Fprintf(&b, " description=%s", description)
	}
	const maxLines = 10
	if len(logLines) > 0 {
		b.WriteString(" log=")
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
