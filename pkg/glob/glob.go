// Package glob compiles the shell-glob label patterns shared by
// maintenance windows, notifications.routes and hybrid_routes. It lives on
// its own (rather than inside pkg/maintenance, where it started) so that
// pkg/config can validate patterns at load time: pkg/maintenance already
// imports pkg/config, so config can't import maintenance back.
package glob

import (
	"fmt"
	"regexp"
	"strings"
)

// Compile turns a shell-glob pattern into an anchored regexp. '*' becomes
// ".*" (any run of characters, including '/'), '?' becomes "." (any
// single character), and '[...]' bracket expressions are passed through
// largely as-is -- glob and regexp bracket-class syntax (ranges, leading
// '^' negation) are compatible for the patterns this project uses.
// Everything else is treated as a literal.
func Compile(pattern string) (*regexp.Regexp, error) {
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
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	return re, nil
}

// Validate reports whether pattern compiles. A pattern that doesn't used
// to be treated as "never matches" at match time, which made a typo like
// "web-[0-9" silently disable a route or maintenance window; config load
// calls this so the typo fails startup instead.
func Validate(pattern string) error {
	_, err := Compile(pattern)
	return err
}

// ValidateMatchers checks every pattern in a matchers map, naming the
// offending label in the error.
func ValidateMatchers(matchers map[string]string) error {
	for k, p := range matchers {
		if err := Validate(p); err != nil {
			return fmt.Errorf("matcher %q: %w", k, err)
		}
	}
	return nil
}
