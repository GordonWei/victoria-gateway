package suppress

import (
	"strings"
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

func rec(alertName, host, resolution string, confirmedAt time.Time) rag.Record {
	return rag.Record{AlertName: alertName, Host: host, Resolution: resolution, ConfirmedAt: confirmedAt}
}

func TestFindCandidates_GroupsByAlertNameAndHost(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	confirmed := []rag.Record{
		rec("DiskSpaceWarning", "h1", "known, disk cycles daily, ignore", base),
		rec("DiskSpaceWarning", "h1", "same as before", base.Add(24*time.Hour)),
		rec("DiskSpaceWarning", "h1", "still the same", base.Add(48*time.Hour)),
		rec("DiskSpaceWarning", "h2", "different host, only seen once", base),
		rec("InstanceDown", "h3", "actually down, fixed by restart", base),
	}

	got := FindCandidates(confirmed, 3)
	if len(got) != 1 {
		t.Fatalf("FindCandidates returned %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.AlertName != "DiskSpaceWarning" || c.Host != "h1" {
		t.Errorf("candidate = %+v, want DiskSpaceWarning/h1", c)
	}
	if c.Count != 3 {
		t.Errorf("Count = %d, want 3", c.Count)
	}
	if c.SampleResolution != "still the same" {
		t.Errorf("SampleResolution = %q, want the most recently confirmed resolution", c.SampleResolution)
	}
	if !c.FirstConfirmed.Equal(base) || !c.LastConfirmed.Equal(base.Add(48*time.Hour)) {
		t.Errorf("FirstConfirmed/LastConfirmed = %v/%v, want %v/%v", c.FirstConfirmed, c.LastConfirmed, base, base.Add(48*time.Hour))
	}
}

func TestFindCandidates_BelowMinCountExcluded(t *testing.T) {
	base := time.Now()
	confirmed := []rag.Record{
		rec("Flaky", "h1", "r1", base),
		rec("Flaky", "h1", "r2", base),
	}
	got := FindCandidates(confirmed, 3)
	if len(got) != 0 {
		t.Fatalf("FindCandidates returned %d candidates, want 0 (below min-count): %+v", len(got), got)
	}
}

func TestFindCandidates_DefaultMinCountIsThree(t *testing.T) {
	base := time.Now()
	confirmed := []rag.Record{
		rec("X", "h1", "r1", base),
		rec("X", "h1", "r2", base),
		rec("X", "h1", "r3", base),
	}
	got := FindCandidates(confirmed, 0) // 0 -> default
	if len(got) != 1 {
		t.Fatalf("FindCandidates(0) returned %d candidates, want 1 (default min-count 3 met)", len(got))
	}
}

func TestFindCandidates_SortedByCountDescending(t *testing.T) {
	base := time.Now()
	var confirmed []rag.Record
	for i := 0; i < 3; i++ {
		confirmed = append(confirmed, rec("LowCount", "h1", "r", base))
	}
	for i := 0; i < 5; i++ {
		confirmed = append(confirmed, rec("HighCount", "h1", "r", base))
	}
	got := FindCandidates(confirmed, 3)
	if len(got) != 2 || got[0].AlertName != "HighCount" || got[1].AlertName != "LowCount" {
		t.Fatalf("FindCandidates order = %+v, want HighCount before LowCount", got)
	}
}

func TestRouteYAML_ContainsMatchersAndReceiver(t *testing.T) {
	c := Candidate{AlertName: `Disk"Warning`, Host: "h1"}
	yaml := c.RouteYAML("")
	if !strings.Contains(yaml, `alertname="Disk\"Warning"`) {
		t.Errorf("RouteYAML should escape embedded quotes in the alert name, got: %s", yaml)
	}
	if !strings.Contains(yaml, `host="h1"`) {
		t.Errorf("RouteYAML should include the host matcher, got: %s", yaml)
	}
	if !strings.Contains(yaml, "receiver: null") {
		t.Errorf("RouteYAML should default the receiver name to \"null\" when none given, got: %s", yaml)
	}
	if !strings.Contains(yaml, "continue: false") {
		t.Errorf("RouteYAML must set continue: false — that's what actually stops the notification, got: %s", yaml)
	}
}

func TestRouteYAML_CustomReceiver(t *testing.T) {
	c := Candidate{AlertName: "X", Host: "h1"}
	yaml := c.RouteYAML("silence-bin")
	if !strings.Contains(yaml, "receiver: silence-bin") {
		t.Errorf("RouteYAML should use the given receiver name, got: %s", yaml)
	}
}

func TestSilenceCLI_ContainsExpectedFields(t *testing.T) {
	c := Candidate{AlertName: "DiskSpaceWarning", Host: "h1"}
	cmd := c.SilenceCLI("720h", "confirmed noise, see victoria-gateway")
	for _, want := range []string{"amtool silence add", "alertname=DiskSpaceWarning", "host=h1", "--duration=720h"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("SilenceCLI() = %q, want it to contain %q", cmd, want)
		}
	}
}
