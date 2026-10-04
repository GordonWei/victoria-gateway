package leakcheck

import (
	"strings"
	"testing"
)

// Fixtures are assembled from pieces so this file never contains a
// matching string itself (it is scanned like every other tracked file).
func j(parts ...string) string { return strings.Join(parts, "") }

func TestScan_Hits(t *testing.T) {
	cases := []struct {
		name, line, rule string
	}{
		{"10/8", j("endpoint: http://10", ".1.2.3:8090"), "private (RFC 1918) IPv4 address"},
		{"172.16/12", j("host ", "172.", "20.5.9"), "private (RFC 1918) IPv4 address"},
		{"192.168/16", j(`"192`, `.168.0.1"`), "private (RFC 1918) IPv4 address"},
		{"ip end of sentence", j("at 10", ".0.0.1."), "private (RFC 1918) IPv4 address"},
		{"macOS home", j("see /Us", "ers/alice/src/x"), "home-directory path"},
		{"linux home", j("/ho", "me/bob/.ssh/id"), "home-directory path"},
		{"aws key", j("AK", "IA", "ABCDEFGHIJKLMNOP"), "AWS access key ID"},
		{"pem", j("-----BEGIN RSA ", "PRIVATE KEY-----"), "PEM private key"},
		{"openssh pem", j("-----BEGIN OPENSSH ", "PRIVATE KEY-----"), "PEM private key"},
		{"github", j("gh", "p_", strings.Repeat("a1", 18)), "GitHub token"},
		{"gitlab", j("gl", "pat-", strings.Repeat("x", 20)), "GitLab token"},
		{"slack", j("xo", "xb-", "1234567890-abcdef"), "Slack token"},
		{"google", j("AI", "za", strings.Repeat("Z", 35)), "Google API key"},
		{"anthropic", j("sk", "-ant-", strings.Repeat("k", 40)), "Anthropic/OpenAI API key"},
		{"telegram", j("123456789", ":AA", strings.Repeat("b", 33)), "Telegram bot token"},
		{"jwt", j("ey", "JhbGciOiJIUzI1NiJ9.", "ey", "JzdWIiOiIxMjM0In0.", "c2lnbmF0dXJlMTIz"), "JWT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scan("f.txt", []byte("ok line\n"+tc.line+"\n"))
			if len(got) != 1 {
				t.Fatalf("Scan(%q) = %+v, want exactly one finding", tc.line, got)
			}
			if got[0].Rule != tc.rule || got[0].Line != 2 || got[0].Path != "f.txt" {
				t.Errorf("finding = %+v, want rule %q on line 2", got[0], tc.rule)
			}
		})
	}
}

func TestScan_Misses(t *testing.T) {
	for _, line := range []string{
		"documentation 192.0.2.6:8090 198.51.100.7 203.0.113.5",
		"loopback 127.0.0.1 and public 8.8.8.8",
		"version v1.10.0.1 and 1.2.3.4.5 and 10.1.2", // not a dotted quad on its own
		"172.32.0.1 is outside 172.16/12",
		"go.sum h1:RuM4OcluM4xFQcGuRE6R7jA33pqxK/W1EsBxpugdZjg=",
		"uuid 3f2504e0-4f89-11d3-9a0c-0305e82c3301 sha 43b1e7a9c0ffee",
		"/Users/you/ and /home/user/ are placeholders",
		"https://gitea.example.com/ops-team/repo",
		"masked AKIA**** stays",
		"short sk-abc",
		j("-----BEGIN ", "PUBLIC KEY-----"),
		j("fixture 10", ".0.0.1 ", AllowMarker),
	} {
		if got := Scan("f.txt", []byte(line)); len(got) != 0 {
			t.Errorf("Scan(%q) = %+v, want no findings", line, got)
		}
	}
}

func TestScan_SkipsBinary(t *testing.T) {
	b := []byte(j("\x00\x01 10", ".0.0.1"))
	if got := Scan("img.png", b); got != nil {
		t.Errorf("binary content scanned: %+v", got)
	}
}

func TestScan_RedactsMatch(t *testing.T) {
	key := j("AK", "IA", "ABCDEFGHIJKLMNOP")
	got := Scan("f", []byte(key))
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if strings.Contains(got[0].Match, key) || !strings.HasPrefix(got[0].Match, "AKIA") {
		t.Errorf("Match = %q, want the key redacted but recognizable", got[0].Match)
	}
}

func TestCheckPath(t *testing.T) {
	for p, want := range map[string]bool{
		"docs/_draft_plan.md":     true,
		"docs/_archive/README.md": true,
		"docs/sub/_notes.md":      true,
		"docs/images/a.jpg":       false,
		"README.md":               false,
		"pkg/_x/y.go":             false,
		"./docs/_review_x.md":     true,
	} {
		if got := CheckPath(p) != nil; got != want {
			t.Errorf("CheckPath(%q) flagged = %v, want %v", p, got, want)
		}
	}
}
