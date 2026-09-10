package rag

import (
	"strings"
	"testing"
	"time"
)

func TestBuildQueryText(t *testing.T) {
	got := BuildQueryText("InstanceDown", "172.16.100.7", "node down", []string{"line1", "line2"})
	for _, want := range []string{"InstanceDown", "172.16.100.7", "node down", "line1", "line2"} {
		if !strings.Contains(got, want) {
			t.Errorf("BuildQueryText() = %q, missing %q", got, want)
		}
	}
}

// Field labels are plain ASCII key=value tokens (not a natural-language
// sentence in any one language) so an embedding model other than the
// multilingual default doesn't see hardcoded Chinese wrapped around
// otherwise-English content as noise.
func TestBuildQueryText_LabelsAreLanguageNeutral(t *testing.T) {
	got := BuildQueryText("InstanceDown", "h", "node down", []string{"line1"})
	for _, notWant := range []string{"告警", "主機", "描述"} {
		if strings.Contains(got, notWant) {
			t.Errorf("BuildQueryText() = %q, expected no hardcoded Chinese labels, found %q", got, notWant)
		}
	}
	for _, want := range []string{"alert=InstanceDown", "host=h", "description=node down"} {
		if !strings.Contains(got, want) {
			t.Errorf("BuildQueryText() = %q, missing %q", got, want)
		}
	}
}

func TestBuildQueryText_TruncatesLogLines(t *testing.T) {
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "line"
	}
	lines[19] = "the-last-line"

	got := BuildQueryText("X", "h", "", lines)
	if !strings.Contains(got, "the-last-line") {
		t.Error("expected the most recent log line to survive truncation")
	}
}

func TestBuildQueryText_NoDescriptionNoLogs(t *testing.T) {
	got := BuildQueryText("X", "h", "", nil)
	if !strings.Contains(got, "X") || !strings.Contains(got, "h") {
		t.Errorf("BuildQueryText() = %q, expected alert name and host at minimum", got)
	}
}

func TestFormatContext_Empty(t *testing.T) {
	if got := FormatContext(nil); got != "" {
		t.Errorf("FormatContext(nil) = %q, want empty string", got)
	}
}

func TestFormatContext(t *testing.T) {
	records := []Record{
		{AlertName: "InstanceDown", Host: "172.16.100.7", Resolution: "舊測試機殘留 target，已下線", CreatedAt: time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)},
	}
	got := FormatContext(records)
	for _, want := range []string{"InstanceDown", "172.16.100.7", "舊測試機殘留 target，已下線", "2026-07-26"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatContext() = %q, missing %q", got, want)
		}
	}
}
