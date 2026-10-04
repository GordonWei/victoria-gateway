// Package leakcheck scans file contents for things that should never land
// in a public repository: private-range IPv4 addresses, home-directory
// paths, credential-shaped strings, and local working notes.
//
// Every rule is a generic shape, never a concrete value. A deny list of
// "the real hostname" or "the real address" would itself have to be
// committed to run in CI, which publishes exactly what it is meant to
// keep out. Shapes catch the same mistakes without naming anything.
//
// A line that legitimately needs to contain one of these shapes (a test
// fixture for the log masker, say) can opt out with the marker
// "leakcheck:allow" anywhere on that line. Prefer documentation values
// that don't match at all (RFC 5737 addresses, example.com) first.
package leakcheck

import (
	"bufio"
	"bytes"
	"net/netip"
	"path"
	"regexp"
	"strings"
)

// AllowMarker on a line exempts that line from every content rule.
const AllowMarker = "leakcheck:allow"

// Finding is one rule hit.
type Finding struct {
	Path string
	Line int // 1-based; 0 for path-level findings
	Rule string
	// Match is the offending text with its middle masked out, so the
	// report itself never reprints a full credential.
	Match string
}

type contentRule struct {
	name string
	re   *regexp.Regexp
	// keep, when set, decides per match whether it is really a hit.
	keep func(match string) bool
}

var ipv4Candidate = regexp.MustCompile(`(?:^|[^0-9.])((?:[0-9]{1,3}\.){3}[0-9]{1,3})(?:[^0-9.]|\.(?:[^0-9]|$)|$)`)

var contentRules = []contentRule{
	{
		name: "home-directory path",
		// macOS and Linux user home directories with a concrete user name.
		re: regexp.MustCompile(`(?:/Users|/home)/[A-Za-z][A-Za-z0-9._-]*/`),
		keep: func(m string) bool {
			// Generic placeholders are fine.
			for _, ok := range []string{"/Users/you/", "/Users/me/", "/home/user/", "/home/you/", "/home/nonroot/"} {
				if m == ok {
					return false
				}
			}
			return true
		},
	},
	{name: "AWS access key ID", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "PEM private key", re: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{name: "GitHub token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,})\b`)},
	{name: "GitLab token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{name: "Slack token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`)},
	{name: "Google API key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{name: "Anthropic/OpenAI API key", re: regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{32,}\b`)},
	{name: "Telegram bot token", re: regexp.MustCompile(`\b[0-9]{8,10}:AA[A-Za-z0-9_-]{33}\b`)},
	{name: "JWT", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
}

// privateV4 reports whether addr is in an RFC 1918 private range.
func privateV4(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return false
	}
	return a.IsPrivate()
}

// CheckPath flags tracked files that are local working notes by naming
// convention: anything under docs/ whose name starts with an underscore.
func CheckPath(p string) *Finding {
	p = path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if !strings.HasPrefix(p, "docs/") {
		return nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "docs/"), "/") {
		if strings.HasPrefix(seg, "_") {
			return &Finding{Path: p, Rule: "local working note under docs/", Match: p}
		}
	}
	return nil
}

// Scan checks one file's contents. Binary content (a NUL byte in the
// first 8 KiB) is skipped.
func Scan(p string, content []byte) []Finding {
	head := content
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil
	}
	var out []Finding
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.Contains(line, AllowMarker) {
			continue
		}
		for _, m := range ipv4Candidate.FindAllStringSubmatch(line, -1) {
			if privateV4(m[1]) {
				out = append(out, Finding{Path: p, Line: n, Rule: "private (RFC 1918) IPv4 address", Match: redact(m[1])})
			}
		}
		for _, r := range contentRules {
			for _, m := range r.re.FindAllString(line, -1) {
				if r.keep != nil && !r.keep(m) {
					continue
				}
				out = append(out, Finding{Path: p, Line: n, Rule: r.name, Match: redact(m)})
			}
		}
	}
	return out
}

// redact keeps the first and last few characters so a finding can be
// located without the report repeating the secret.
func redact(s string) string {
	if len(s) <= 8 {
		return s
	}
	keep := 4
	return s[:keep] + strings.Repeat("*", len(s)-2*keep) + s[len(s)-keep:]
}
