package maintenance

import (
	"testing"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
)

func TestParseSchedule_DOW(t *testing.T) {
	s, err := parseSchedule("SAT 02:00-04:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.dow != time.Saturday {
		t.Errorf("dow = %v, want Saturday", s.dow)
	}
	if s.nthWeek != 0 {
		t.Errorf("nthWeek = %d, want 0", s.nthWeek)
	}
	if s.startHour != 2 || s.startMin != 0 {
		t.Errorf("start = %d:%d, want 02:00", s.startHour, s.startMin)
	}
	if s.endHour != 4 || s.endMin != 0 {
		t.Errorf("end = %d:%d, want 04:00", s.endHour, s.endMin)
	}
}

func TestParseSchedule_DAILY(t *testing.T) {
	s, err := parseSchedule("DAILY 04:00-04:30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.dow != -1 {
		t.Errorf("dow = %v, want -1 (DAILY)", s.dow)
	}
	if s.startHour != 4 || s.startMin != 0 || s.endHour != 4 || s.endMin != 30 {
		t.Errorf("time range = %d:%d-%d:%d, want 04:00-04:30", s.startHour, s.startMin, s.endHour, s.endMin)
	}
}

func TestParseSchedule_NthDOW(t *testing.T) {
	s, err := parseSchedule("1st-SUN 03:00-06:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.dow != time.Sunday {
		t.Errorf("dow = %v, want Sunday", s.dow)
	}
	if s.nthWeek != 1 {
		t.Errorf("nthWeek = %d, want 1", s.nthWeek)
	}
}

func TestParseSchedule_CaseInsensitive(t *testing.T) {
	_, err := parseSchedule("sat 02:00-04:00")
	if err != nil {
		t.Fatalf("lowercase should be accepted: %v", err)
	}
}

func TestParseSchedule_Invalid(t *testing.T) {
	cases := []string{
		"",
		"SAT",
		"SAT 02:00",
		"BLAH 02:00-04:00",
		"SAT 25:00-04:00",
		"SAT 02:00-04:60",
		"6th-SUN 03:00-06:00",
		"0th-SUN 03:00-06:00",
	}
	for _, c := range cases {
		if _, err := parseSchedule(c); err == nil {
			t.Errorf("parseSchedule(%q) should fail", c)
		}
	}
}

func TestIsActive_DOW_InRange(t *testing.T) {
	s := &schedule{dow: time.Saturday, startHour: 2, startMin: 0, endHour: 4, endMin: 0}
	// Saturday 2026-08-29 03:00 +0800
	sat := time.Date(2026, 8, 29, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !s.isActive(sat) {
		t.Error("expected active on Saturday 03:00 within 02:00-04:00")
	}
}

func TestIsActive_DOW_OutOfRange(t *testing.T) {
	s := &schedule{dow: time.Saturday, startHour: 2, startMin: 0, endHour: 4, endMin: 0}
	// Saturday 2026-08-29 05:00
	sat := time.Date(2026, 8, 29, 5, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(sat) {
		t.Error("expected inactive on Saturday 05:00 (outside 02:00-04:00)")
	}
}

func TestIsActive_DOW_WrongDay(t *testing.T) {
	s := &schedule{dow: time.Saturday, startHour: 2, startMin: 0, endHour: 4, endMin: 0}
	// Friday 2026-08-28 03:00
	fri := time.Date(2026, 8, 28, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(fri) {
		t.Error("expected inactive on Friday even though time is within range")
	}
}

func TestIsActive_DAILY(t *testing.T) {
	s := &schedule{dow: -1, startHour: 4, startMin: 0, endHour: 4, endMin: 30}
	// Any day at 04:15
	mon := time.Date(2026, 8, 25, 4, 15, 0, 0, time.FixedZone("CST", 8*3600))
	if !s.isActive(mon) {
		t.Error("expected DAILY active at 04:15 within 04:00-04:30")
	}
	// Any day at 04:30 (exclusive end)
	monEnd := time.Date(2026, 8, 25, 4, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(monEnd) {
		t.Error("expected DAILY inactive at 04:30 (end is exclusive)")
	}
}

func TestIsActive_NthDOW(t *testing.T) {
	s := &schedule{dow: time.Sunday, nthWeek: 1, startHour: 3, startMin: 0, endHour: 6, endMin: 0}
	// 2026-09-06 is the 1st Sunday of September
	firstSun := time.Date(2026, 9, 6, 4, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !s.isActive(firstSun) {
		t.Error("expected active on 1st Sunday 04:00 within 03:00-06:00")
	}
	// 2026-09-13 is the 2nd Sunday
	secondSun := time.Date(2026, 9, 13, 4, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(secondSun) {
		t.Error("expected inactive on 2nd Sunday (only 1st matches)")
	}
}

func TestIsActive_CrossMidnight(t *testing.T) {
	s := &schedule{dow: time.Saturday, startHour: 23, startMin: 0, endHour: 2, endMin: 0}
	// Saturday 23:30 — within the start portion
	satNight := time.Date(2026, 8, 29, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if !s.isActive(satNight) {
		t.Error("expected active on Saturday 23:30 within SAT 23:00-02:00")
	}
	// Sunday 01:00 — the overflow portion (day after Saturday)
	sunMorning := time.Date(2026, 8, 30, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !s.isActive(sunMorning) {
		t.Error("expected active on Sunday 01:00 (overflow from SAT 23:00-02:00)")
	}
	// Sunday 02:00 — at the exclusive end
	sunEnd := time.Date(2026, 8, 30, 2, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(sunEnd) {
		t.Error("expected inactive on Sunday 02:00 (end is exclusive)")
	}
	// Sunday 23:30 — wrong day (not the overflow from Saturday)
	sunNight := time.Date(2026, 8, 30, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(sunNight) {
		t.Error("expected inactive on Sunday 23:30 (schedule is SAT not SUN)")
	}
}

func TestIsActive_CrossMidnight_NthWeek(t *testing.T) {
	// 2026-08-27 regression test: nth-week constraint must also apply
	// to the cross-midnight overflow branch, not just the same-day
	// branch (see isActive's bugfix comment). Schedule is "1st Saturday
	// 23:00-02:00" -- only the first Saturday of the month's overflow
	// into Sunday should match; other Saturdays' overflow must not.
	s := &schedule{dow: time.Saturday, nthWeek: 1, startHour: 23, startMin: 0, endHour: 2, endMin: 0}

	// 2026-08-01 is the 1st Saturday of August.
	firstSatNight := time.Date(2026, 8, 1, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if !s.isActive(firstSatNight) {
		t.Error("expected active on 1st Saturday 23:30 (same-day portion)")
	}
	firstSunOverflow := time.Date(2026, 8, 2, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !s.isActive(firstSunOverflow) {
		t.Error("expected active on Sunday 01:00 overflow from the 1st Saturday")
	}

	// 2026-08-08 is the 2nd Saturday of August -- same-day portion must
	// NOT match a "1st-SAT" schedule.
	secondSatNight := time.Date(2026, 8, 8, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(secondSatNight) {
		t.Error("expected inactive on 2nd Saturday 23:30 (schedule is 1st-SAT only)")
	}
	// This is the bug this test exists to lock down: overflow into
	// Sunday 2026-08-09 from the 2nd Saturday used to match every
	// week's overflow regardless of nth-week, because the nth-week
	// check was only wired into the same-day branch, never the
	// cross-midnight branch.
	secondSunOverflow := time.Date(2026, 8, 9, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if s.isActive(secondSunOverflow) {
		t.Error("expected inactive on Sunday 01:00 overflow from the 2nd Saturday (only 1st-SAT should match)")
	}
}

func TestParseNth_RejectsMismatchedSuffix(t *testing.T) {
	// 2026-08-27 regression test: parseNth used to strip any trailing
	// character in the set {S,T,N,D,R,H} (strings.TrimRight treats its
	// second argument as a character set, not a literal suffix), so a
	// misspelled ordinal like "1TH" (should be "1ST") or "2ST" (should
	// be "2ND") silently parsed as a valid number instead of erroring.
	badCases := []string{"1TH", "2ST", "3ST", "4RD", "5ST", "1ND"}
	for _, s := range badCases {
		if _, err := parseNth(s); err == nil {
			t.Errorf("parseNth(%q) = no error, want error (mismatched ordinal suffix)", s)
		}
	}

	goodCases := map[string]int{"1st": 1, "2nd": 2, "3rd": 3, "4th": 4, "5th": 5, "1ST": 1}
	for s, want := range goodCases {
		n, err := parseNth(s)
		if err != nil {
			t.Errorf("parseNth(%q) unexpected error: %v", s, err)
			continue
		}
		if n != want {
			t.Errorf("parseNth(%q) = %d, want %d", s, n, want)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"172.16.100.*", "172.16.100.6", true},
		{"172.16.100.*", "172.16.200.6", false},
		{"DiskSpace*", "DiskSpaceWarning", true},
		{"DiskSpace*", "MemoryHigh", false},
		{"exact-match", "exact-match", true},
		{"exact-match", "exact-matc", false},
		{"node-[0-9]", "node-3", true},
		{"node-[0-9]", "node-abc", false},
		// 2026-08-27 regression cases: '*' must cross '/', since label
		// values this project actually matches against (paths, image
		// names, "namespace/pod" strings) commonly contain it. This
		// used to silently fail under filepath.Match semantics.
		{"/api/*", "/api/v1/users", true},
		{"/api/*", "/other/path", false},
		{"*prod*", "/env/prod/service", true},
		{"*prod*", "/env/staging/service", false},
	}
	for _, tc := range cases {
		got := matchGlob(tc.pattern, tc.value)
		if got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestCheck_MatchLabels(t *testing.T) {
	w := Window{
		Name:   "test",
		Action: ActionSuppress,
		Matchers: map[string]string{
			"host":      "172.16.100.*",
			"alertname": "DiskSpace*",
		},
		schedule: &schedule{dow: -1, startHour: 0, startMin: 0, endHour: 23, endMin: 59},
	}

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	// Both match
	if !w.Check(now, map[string]string{"host": "172.16.100.6", "alertname": "DiskSpaceWarning"}) {
		t.Error("expected match when both labels match")
	}

	// One doesn't match
	if w.Check(now, map[string]string{"host": "172.16.200.1", "alertname": "DiskSpaceWarning"}) {
		t.Error("expected no match when host doesn't match")
	}

	// Missing label
	if w.Check(now, map[string]string{"alertname": "DiskSpaceWarning"}) {
		t.Error("expected no match when required label is missing")
	}
}

func TestCheck_OneTimeWindow(t *testing.T) {
	start := time.Date(2026, 9, 1, 22, 0, 0, 0, time.FixedZone("CST", 8*3600))
	end := time.Date(2026, 9, 2, 6, 0, 0, 0, time.FixedZone("CST", 8*3600))
	w := Window{
		Name:     "migration",
		Action:   ActionMute,
		Matchers: map[string]string{"alertname": "*"},
		start:    start,
		end:      end,
	}

	during := time.Date(2026, 9, 2, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !w.Check(during, map[string]string{"alertname": "anything"}) {
		t.Error("expected match during one-time window")
	}

	before := time.Date(2026, 9, 1, 21, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if w.Check(before, map[string]string{"alertname": "anything"}) {
		t.Error("expected no match before one-time window starts")
	}

	after := time.Date(2026, 9, 2, 6, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if w.Check(after, map[string]string{"alertname": "anything"}) {
		t.Error("expected no match at one-time window end (exclusive)")
	}
}

func TestCheckAll_FirstMatchWins(t *testing.T) {
	windows := []Window{
		{
			Name:     "suppress-all",
			Action:   ActionSuppress,
			Matchers: map[string]string{"severity": "warning"},
			schedule: &schedule{dow: -1, startHour: 0, startMin: 0, endHour: 23, endMin: 59},
		},
		{
			Name:     "mute-all",
			Action:   ActionMute,
			Matchers: map[string]string{"severity": "*"},
			schedule: &schedule{dow: -1, startHour: 0, startMin: 0, endHour: 23, endMin: 59},
		},
	}

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	action, name := CheckAll(windows, now, map[string]string{"severity": "warning"})
	if action != ActionSuppress || name != "suppress-all" {
		t.Errorf("got action=%q name=%q, want suppress/suppress-all", action, name)
	}

	action, name = CheckAll(windows, now, map[string]string{"severity": "critical"})
	if action != ActionMute || name != "mute-all" {
		t.Errorf("got action=%q name=%q, want mute/mute-all", action, name)
	}
}

func TestCheckAll_NoMatch(t *testing.T) {
	windows := []Window{
		{
			Name:     "off-hours",
			Action:   ActionSuppress,
			Matchers: map[string]string{"host": "172.16.100.*"},
			schedule: &schedule{dow: time.Saturday, startHour: 2, startMin: 0, endHour: 4, endMin: 0},
		},
	}

	// Wrong day
	now := time.Date(2026, 8, 27, 3, 0, 0, 0, time.UTC) // Wednesday
	action, name := CheckAll(windows, now, map[string]string{"host": "172.16.100.6"})
	if action != "" || name != "" {
		t.Errorf("expected no match, got action=%q name=%q", action, name)
	}
}

func TestParseWindows_Valid(t *testing.T) {
	defs := []config.MaintenanceWindow{
		{
			Name:     "weekly",
			Schedule: "SAT 02:00-04:00",
			Matchers: map[string]string{"host": "172.16.100.*"},
			Action:   "suppress",
		},
		{
			Name:     "migration",
			Start:    "2026-09-01T22:00:00+08:00",
			End:      "2026-09-02T06:00:00+08:00",
			Matchers: map[string]string{"alertname": "DiskSpace*"},
			Action:   "mute",
		},
	}
	windows, err := ParseWindows(defs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("got %d windows, want 2", len(windows))
	}
	if windows[0].schedule == nil {
		t.Error("window 0 should have a schedule")
	}
	if windows[1].start.IsZero() {
		t.Error("window 1 should have a start time")
	}
}

func TestParseWindows_InvalidSchedule(t *testing.T) {
	defs := []config.MaintenanceWindow{
		{
			Name:     "bad",
			Schedule: "FUNDAY 02:00-04:00",
			Matchers: map[string]string{"host": "*"},
			Action:   "suppress",
		},
	}
	if _, err := ParseWindows(defs); err == nil {
		t.Error("expected error for invalid schedule")
	}
}

func TestParseWindows_InvalidStartEnd(t *testing.T) {
	defs := []config.MaintenanceWindow{
		{
			Name:     "bad-start",
			Start:    "not-a-date",
			End:      "2026-09-02T06:00:00+08:00",
			Matchers: map[string]string{"host": "*"},
			Action:   "mute",
		},
	}
	if _, err := ParseWindows(defs); err == nil {
		t.Error("expected error for invalid start time")
	}
}

func TestParseWindows_EndBeforeStart(t *testing.T) {
	defs := []config.MaintenanceWindow{
		{
			Name:     "backwards",
			Start:    "2026-09-02T06:00:00+08:00",
			End:      "2026-09-01T22:00:00+08:00",
			Matchers: map[string]string{"host": "*"},
			Action:   "mute",
		},
	}
	if _, err := ParseWindows(defs); err == nil {
		t.Error("expected error when end is before start")
	}
}

func TestConfigValidate_MaintenanceWindow_BothScheduleAndStartEnd(t *testing.T) {
	c := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
		MaintenanceWindows: []config.MaintenanceWindow{
			{
				Name:     "conflict",
				Schedule: "SAT 02:00-04:00",
				Start:    "2026-09-01T22:00:00+08:00",
				End:      "2026-09-02T06:00:00+08:00",
				Matchers: map[string]string{"host": "*"},
				Action:   "suppress",
			},
		},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error when both schedule and start/end are set")
	}
}

func TestConfigValidate_MaintenanceWindow_NeitherScheduleNorStartEnd(t *testing.T) {
	c := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
		MaintenanceWindows: []config.MaintenanceWindow{
			{
				Name:     "empty-time",
				Matchers: map[string]string{"host": "*"},
				Action:   "suppress",
			},
		},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error when neither schedule nor start/end is set")
	}
}

func TestConfigValidate_MaintenanceWindow_EmptyMatchers(t *testing.T) {
	c := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
		MaintenanceWindows: []config.MaintenanceWindow{
			{
				Name:     "no-matchers",
				Schedule: "SAT 02:00-04:00",
				Matchers: map[string]string{},
				Action:   "suppress",
			},
		},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error when matchers is empty")
	}
}

func TestConfigValidate_MaintenanceWindow_InvalidAction(t *testing.T) {
	c := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
		MaintenanceWindows: []config.MaintenanceWindow{
			{
				Name:     "bad-action",
				Schedule: "SAT 02:00-04:00",
				Matchers: map[string]string{"host": "*"},
				Action:   "ignore",
			},
		},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for invalid action")
	}
}

func TestConfigValidate_MaintenanceWindow_Valid(t *testing.T) {
	c := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
		MaintenanceWindows: []config.MaintenanceWindow{
			{
				Name:     "ok",
				Schedule: "SAT 02:00-04:00",
				Matchers: map[string]string{"host": "172.16.100.*"},
				Action:   "suppress",
			},
		},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestConfigValidate_MaintenanceWindow_InvalidTimezone(t *testing.T) {
	c := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
		MaintenanceWindows: []config.MaintenanceWindow{
			{
				Name:     "bad-tz",
				Schedule: "SAT 02:00-04:00",
				Matchers: map[string]string{"host": "*"},
				Action:   "suppress",
				Timezone: "Not/A_Real_Zone",
			},
		},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for invalid timezone")
	}
}

func TestConfigValidate_MaintenanceWindow_ValidTimezone(t *testing.T) {
	c := &config.Config{
		Loki:       config.LokiConfig{Endpoint: "http://loki:3100"},
		Summarizer: config.LLMConfig{Endpoint: "http://llm:1234", Model: "m"},
		MaintenanceWindows: []config.MaintenanceWindow{
			{
				Name:     "ok-tz",
				Schedule: "SAT 02:00-04:00",
				Matchers: map[string]string{"host": "*"},
				Action:   "suppress",
				Timezone: "Asia/Taipei",
			},
		},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

// TestParseWindows_Timezone_SetsLoc verifies a schedule window with a
// Timezone def carries a parsed *time.Location, and one without keeps
// loc nil (the original, unchanged behavior — Check uses whatever zone
// the caller's time.Time already carries).
func TestParseWindows_Timezone_SetsLoc(t *testing.T) {
	defs := []config.MaintenanceWindow{
		{
			Name:     "with-tz",
			Schedule: "SAT 23:00-02:00",
			Matchers: map[string]string{"host": "*"},
			Action:   "suppress",
			Timezone: "Asia/Taipei",
		},
		{
			Name:     "without-tz",
			Schedule: "SAT 23:00-02:00",
			Matchers: map[string]string{"host": "*"},
			Action:   "suppress",
		},
	}
	windows, err := ParseWindows(defs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if windows[0].loc == nil || windows[0].loc.String() != "Asia/Taipei" {
		t.Errorf("window 0 loc = %v, want Asia/Taipei", windows[0].loc)
	}
	if windows[1].loc != nil {
		t.Errorf("window 1 loc = %v, want nil (no timezone set)", windows[1].loc)
	}
}

func TestParseWindows_Timezone_Invalid(t *testing.T) {
	defs := []config.MaintenanceWindow{
		{
			Name:     "bad-tz",
			Schedule: "SAT 02:00-04:00",
			Matchers: map[string]string{"host": "*"},
			Action:   "suppress",
			Timezone: "Not/A_Real_Zone",
		},
	}
	if _, err := ParseWindows(defs); err == nil {
		t.Error("expected error for invalid timezone")
	}
}

// TestIsActive_Timezone_SameInstantDifferentZones is the point of this
// feature: the same UTC instant should be judged against a "SAT
// 23:00-02:00" cross-midnight schedule differently depending on which
// zone the window is configured to evaluate in — that's what an operator
// running the gateway in one zone but maintaining infra that "means"
// another zone's clock actually needs. Pick a UTC instant that's
// Saturday 23:30 in Asia/Taipei (UTC+8) — inside the window — but a
// Saturday morning (well outside 23:00-02:00) in America/Los_Angeles.
func TestIsActive_Timezone_SameInstantDifferentZones(t *testing.T) {
	taipei, err := ParseWindows([]config.MaintenanceWindow{{
		Name: "taipei", Schedule: "SAT 23:00-02:00",
		Matchers: map[string]string{"host": "*"}, Action: "suppress",
		Timezone: "Asia/Taipei",
	}})
	if err != nil {
		t.Fatalf("parse taipei window: %v", err)
	}
	laNoTZ, err := ParseWindows([]config.MaintenanceWindow{{
		Name: "no-tz", Schedule: "SAT 23:00-02:00",
		Matchers: map[string]string{"host": "*"}, Action: "suppress",
		// no Timezone: evaluated in whatever zone the caller's time.Time carries.
	}})
	if err != nil {
		t.Fatalf("parse no-tz window: %v", err)
	}

	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	// 2026-08-29 15:30 UTC = Sat 23:30 in Asia/Taipei (UTC+8) = Sat 08:30
	// in America/Los_Angeles (UTC-7, PDT in August).
	instant := time.Date(2026, 8, 29, 15, 30, 0, 0, time.UTC)
	labels := map[string]string{"host": "anything"}

	if !taipei[0].Check(instant, labels) {
		t.Error("expected active: Sat 23:30 Asia/Taipei falls in SAT 23:00-02:00")
	}
	// Same instant, viewed in the caller's own America/Los_Angeles zone
	// (no window Timezone override) — Sat 08:30, well outside the window.
	instantLA := instant.In(la)
	if laNoTZ[0].Check(instantLA, labels) {
		t.Error("expected inactive: Sat 08:30 America/Los_Angeles is outside SAT 23:00-02:00")
	}
}
