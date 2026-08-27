// Package maintenance implements maintenance window matching for
// victoria-gateway. A maintenance window defines a time period (periodic
// or one-time) and a set of label matchers; when an alert arrives during
// an active window whose matchers match the alert's labels, the alert is
// either suppressed (skipped entirely) or muted (analyzed but not pushed
// to notification channels).
package maintenance

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
)

// Action is what happens to an alert that matches an active window.
type Action string

const (
	ActionSuppress Action = "suppress" // skip entirely: no Loki, no LLM, no capture, no push
	ActionMute     Action = "mute"     // analyze + RAG capture, but don't push to notification channels
)

// Window is a parsed, ready-to-evaluate maintenance window. Build one per
// config.MaintenanceWindow at startup via ParseWindows; then call
// Check(time, labels) on each incoming alert.
type Window struct {
	Name     string
	Action   Action
	Matchers map[string]string // label name → glob pattern

	// One of these two is set, never both (enforced by config.Validate).
	schedule *schedule     // periodic
	start    time.Time     // one-time
	end      time.Time     // one-time
}

// schedule represents a parsed periodic time expression.
type schedule struct {
	dow       time.Weekday // -1 means "every day" (DAILY)
	nthWeek   int          // 0 means "every week"; 1 means 1st, 2 means 2nd, etc.
	startHour int
	startMin  int
	endHour   int
	endMin    int
}

// ParseWindows parses all maintenance window definitions from config into
// ready-to-evaluate Window values. Returns an error if any schedule string
// is malformed or any one-time start/end can't be parsed as RFC3339.
func ParseWindows(defs []config.MaintenanceWindow) ([]Window, error) {
	windows := make([]Window, 0, len(defs))
	for i, def := range defs {
		label := fmt.Sprintf("maintenance_windows[%d]", i)
		if def.Name != "" {
			label = def.Name
		}

		w := Window{
			Name:     def.Name,
			Action:   Action(def.Action),
			Matchers: def.Matchers,
		}

		if def.Schedule != "" {
			s, err := parseSchedule(def.Schedule)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", label, err)
			}
			w.schedule = s
		} else {
			start, err := time.Parse(time.RFC3339, def.Start)
			if err != nil {
				return nil, fmt.Errorf("%s: parse start: %w", label, err)
			}
			end, err := time.Parse(time.RFC3339, def.End)
			if err != nil {
				return nil, fmt.Errorf("%s: parse end: %w", label, err)
			}
			if !end.After(start) {
				return nil, fmt.Errorf("%s: end must be after start", label)
			}
			w.start = start
			w.end = end
		}

		windows = append(windows, w)
	}
	return windows, nil
}

// parseSchedule parses schedule strings like:
//   - "SAT 02:00-04:00"
//   - "DAILY 04:00-04:30"
//   - "1st-SUN 03:00-06:00"
func parseSchedule(s string) (*schedule, error) {
	s = strings.TrimSpace(s)
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid schedule %q: expected \"<day-spec> HH:MM-HH:MM\"", s)
	}

	daySpec := strings.ToUpper(parts[0])
	timeSpec := parts[1]

	sched := &schedule{}

	// Parse day specifier
	switch {
	case daySpec == "DAILY":
		sched.dow = -1
		sched.nthWeek = 0
	case strings.Contains(daySpec, "-"):
		// "1ST-SUN", "2ND-MON", etc.
		dashParts := strings.SplitN(daySpec, "-", 2)
		nth, err := parseNth(dashParts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", s, err)
		}
		dow, err := parseDOW(dashParts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", s, err)
		}
		sched.dow = dow
		sched.nthWeek = nth
	default:
		dow, err := parseDOW(daySpec)
		if err != nil {
			return nil, fmt.Errorf("invalid schedule %q: %w", s, err)
		}
		sched.dow = dow
		sched.nthWeek = 0
	}

	// Parse time range
	timeParts := strings.SplitN(timeSpec, "-", 2)
	if len(timeParts) != 2 {
		return nil, fmt.Errorf("invalid schedule %q: expected HH:MM-HH:MM time range", s)
	}
	startH, startM, err := parseHHMM(timeParts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid schedule %q: start time: %w", s, err)
	}
	endH, endM, err := parseHHMM(timeParts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid schedule %q: end time: %w", s, err)
	}
	sched.startHour = startH
	sched.startMin = startM
	sched.endHour = endH
	sched.endMin = endM

	return sched, nil
}

func parseDOW(s string) (time.Weekday, error) {
	switch s {
	case "SUN", "SUNDAY":
		return time.Sunday, nil
	case "MON", "MONDAY":
		return time.Monday, nil
	case "TUE", "TUESDAY":
		return time.Tuesday, nil
	case "WED", "WEDNESDAY":
		return time.Wednesday, nil
	case "THU", "THURSDAY":
		return time.Thursday, nil
	case "FRI", "FRIDAY":
		return time.Friday, nil
	case "SAT", "SATURDAY":
		return time.Saturday, nil
	}
	return 0, fmt.Errorf("unknown day of week %q", s)
}

func parseNth(s string) (int, error) {
	orig := s
	upper := strings.ToUpper(s)

	// 2026-08-27 bugfix: this used to strip any trailing run of
	// characters in the set {S,T,N,D,R,H} (strings.TrimRight treats its
	// second argument as a character set, not a literal suffix), so a
	// misspelled ordinal like "2RD" (should be "2ND") or "1TH" (should
	// be "1ST") silently trimmed down to a valid-looking number instead
	// of being rejected. Match one of the four real ordinal suffixes
	// explicitly, then verify it's the *correct* one for the number.
	var suffix string
	switch {
	case strings.HasSuffix(upper, "ST"):
		suffix = "ST"
	case strings.HasSuffix(upper, "ND"):
		suffix = "ND"
	case strings.HasSuffix(upper, "RD"):
		suffix = "RD"
	case strings.HasSuffix(upper, "TH"):
		suffix = "TH"
	default:
		return 0, fmt.Errorf("invalid nth-week specifier %q (expected 1st-5th)", orig)
	}

	n, err := strconv.Atoi(strings.TrimSuffix(upper, suffix))
	if err != nil || n < 1 || n > 5 {
		return 0, fmt.Errorf("invalid nth-week specifier %q (expected 1st-5th)", orig)
	}

	wantSuffix := map[int]string{1: "ST", 2: "ND", 3: "RD", 4: "TH", 5: "TH"}[n]
	if suffix != wantSuffix {
		return 0, fmt.Errorf("invalid nth-week specifier %q: %d takes suffix %q, not %q", orig, n, wantSuffix, suffix)
	}
	return n, nil
}

func parseHHMM(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("hour out of range in %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("minute out of range in %q", s)
	}
	return h, m, nil
}

// Check evaluates whether a given alert (described by its labels) at a
// given time falls within this window. Returns true if both the time is
// within the window's active period AND all matchers match the alert's
// labels.
func (w *Window) Check(now time.Time, labels map[string]string) bool {
	if !w.isTimeActive(now) {
		return false
	}
	return w.matchLabels(labels)
}

func (w *Window) isTimeActive(now time.Time) bool {
	if w.schedule != nil {
		return w.schedule.isActive(now)
	}
	return !now.Before(w.start) && now.Before(w.end)
}

func (s *schedule) isActive(now time.Time) bool {
	// Check day-of-week
	if s.dow >= 0 {
		if now.Weekday() != s.dow {
			// For cross-midnight schedules, also check if we're in the
			// overflow portion (e.g., SAT 23:00-02:00 means Sunday
			// 00:00-02:00 is also valid).
			if s.crossesMidnight() {
				yesterday := now.Add(-24 * time.Hour)
				if yesterday.Weekday() != s.dow {
					return false
				}
				// 2026-08-27 bugfix: the nth-week constraint was only
				// checked in the same-day branch below, never here --
				// so "1st-SAT 23:00-02:00" matched the Sunday overflow
				// of *every* Saturday, not just the first one. The
				// scheduled day-of-week occurrence is "yesterday" from
				// the overflow's point of view, so nth-week must be
				// checked against yesterday's date, not now's.
				if !s.nthWeekMatches(yesterday) {
					return false
				}
				// We're in the day after the scheduled day — only the
				// overflow portion (00:00 to end) applies.
				return s.inOverflowPortion(now)
			}
			return false
		}
		// Check nth-week constraint
		if !s.nthWeekMatches(now) {
			return false
		}
	}

	return s.inTimeRange(now)
}

// nthWeekMatches reports whether day falls in the schedule's nth-week
// (1st through 5th occurrence of the month, by day-of-month bucketing).
// Always true when the schedule has no nth-week constraint (nthWeek == 0,
// i.e. "every week").
func (s *schedule) nthWeekMatches(day time.Time) bool {
	if s.nthWeek == 0 {
		return true
	}
	nth := (day.Day()-1)/7 + 1
	return nth == s.nthWeek
}

func (s *schedule) crossesMidnight() bool {
	startMinutes := s.startHour*60 + s.startMin
	endMinutes := s.endHour*60 + s.endMin
	return endMinutes <= startMinutes
}

func (s *schedule) inTimeRange(now time.Time) bool {
	nowMinutes := now.Hour()*60 + now.Minute()
	startMinutes := s.startHour*60 + s.startMin
	endMinutes := s.endHour*60 + s.endMin

	if s.crossesMidnight() {
		// e.g. 23:00-02:00: active if >= 23:00 OR < 02:00
		return nowMinutes >= startMinutes || nowMinutes < endMinutes
	}
	return nowMinutes >= startMinutes && nowMinutes < endMinutes
}

func (s *schedule) inOverflowPortion(now time.Time) bool {
	nowMinutes := now.Hour()*60 + now.Minute()
	endMinutes := s.endHour*60 + s.endMin
	return nowMinutes < endMinutes
}

func (w *Window) matchLabels(labels map[string]string) bool {
	for matchKey, matchPattern := range w.Matchers {
		labelVal, exists := labels[matchKey]
		if !exists {
			return false
		}
		if !matchGlob(matchPattern, labelVal) {
			return false
		}
	}
	return true
}

// matchGlob matches pattern against value using shell-glob-style wildcards
// (*, ?, and POSIX bracket character classes like [0-9]). An exact string
// (no wildcards) still works as a plain equality check.
//
// 2026-08-27 bugfix: this used to delegate to filepath.Match, whose '*'
// deliberately does NOT cross '/' (it's a filesystem-path matcher, not a
// general string glob). Label values this project actually matches
// against -- image names, request paths, "namespace/pod" strings -- very
// commonly contain '/', so patterns like "/api/*" or "*prod*" silently
// failed to match "/api/v1/users" or "/env/prod/service" with no error at
// all: the maintenance window would just never fire, and there was
// nothing in the logs to say why. Compiling the glob to a regexp instead
// gives '*' the "matches any run of characters" meaning implied by the
// commit message and the design doc's examples.
func matchGlob(pattern, value string) bool {
	re, err := globToRegexp(pattern)
	if err != nil {
		// Malformed pattern: treat as no-match rather than crashing.
		return false
	}
	return re.MatchString(value)
}

// globToRegexp compiles a shell-glob pattern into an anchored regexp.
// '*' becomes ".*" (any run of characters, including '/'), '?' becomes
// "." (any single character), and '[...]' bracket expressions are passed
// through largely as-is -- glob and regexp bracket-class syntax (ranges,
// leading '^' negation) are compatible for the patterns this project
// uses. Everything else is treated as a literal.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var sb strings.Builder
	sb.WriteByte('^')
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		case '[':
			j := i + 1
			for j < len(runes) && runes[j] != ']' {
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unterminated character class in glob %q", pattern)
			}
			sb.WriteString(string(runes[i : j+1]))
			i = j
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteByte('$')
	return regexp.Compile(sb.String())
}

// CheckAll evaluates all windows against an alert's labels at the given
// time. Returns the action of the first matching window and its name, or
// ("", "") if no window matches. Evaluation order is config order: first
// match wins.
func CheckAll(windows []Window, now time.Time, labels map[string]string) (action Action, windowName string) {
	for i := range windows {
		if windows[i].Check(now, labels) {
			return windows[i].Action, windows[i].Name
		}
	}
	return "", ""
}
