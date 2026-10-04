package mask

import (
	"strings"
	"testing"
)

// Fixtures are joined from pieces so this file never holds a string the
// repository's leak check would flag (cmd/leakcheck).
func j(parts ...string) string { return strings.Join(parts, "") }

// fakeCredentials pairs each new no-key credential shape with the exact
// text redaction must leave. Exported to the exit-coverage test in
// cmd/victoria-gateway via FakeCredentialSamples.
var fakeCredentials = []struct {
	name, in, want string
}{
	{"aws access key id", j("key AK", "IA", "IOSFODNN7EXAMPLE used"), "key AKIAAAAAAAAA0AAAAAAA used"}, // leakcheck:allow (masked-shape fixture)
	{"aws temporary key id", j("AS", "IA", "Z9Y8X7W6V5U4T3S2"), "ASIAA0A0A0A0A0A0A0A0"},                // leakcheck:allow (masked-shape fixture)
	{"github classic", j("gh", "p_", strings.Repeat("aB3", 12)), "ghp_" + strings.Repeat("aA0", 12)},
	{"github fine-grained", j("github", "_pat_", "11ABCDEFG0_abcdefghijklmnop"), "github_pat_00AAAAAAA0_aaaaaaaaaaaaaaaa"},
	{"gitlab", j("gl", "pat-", "xY1-xY1-xY1-xY1-xY1-xY1"), "glpat-aA0-aA0-aA0-aA0-aA0-aA0"}, // leakcheck:allow (masked-shape fixture)
	{"slack bot token", j("xo", "xb-", "1234-5678-abcDEF"), "xoxb-0000-0000-aaaAAA"},        // leakcheck:allow (masked-shape fixture)
	{"slack webhook url", j("https://hooks.slack.com/services/", "T000/B000/abcdefghijklmnopQRST"), "https://hooks.slack.com/services/A000/A000/aaaaaaaaaaaaaaaaAAAA"},
	{"google api key", j("AI", "za", "SyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q"), "AIzaAaA0a0A0a0A0a0A0a0A0a0A0a0A0a0A0a0A"}, // leakcheck:allow (masked-shape fixture)
	{"anthropic key", j("sk", "-ant-", "api03-abcdefghij0123456789"), "sk-ant-aaa00-aaaaaaaaaa0000000000"},
	{"openai project key", j("sk", "-proj-", "ABCdef0123456789ghijKL"), "sk-proj-AAAaaa0000000000aaaaAA"},
	{"stripe live key", j("sk", "_live_", "51Habcdefghijklmn"), "sk_live_00Aaaaaaaaaaaaaaa"},
	{"telegram bot token in url", j("POST https://api.telegram.org/bot", "123456789:AA", strings.Repeat("Hk", 16), "x/sendMessage"),
		"POST https://api.telegram.org/bot123456789:AA" + strings.Repeat("Aa", 16) + "a/sendMessage"},
	{"jwt", j("auth ey", "JhbGciOiJIUzI1NiJ9.ey", "JzdWIiOiIxMjM0In0.c2lnbmF0dXJlMTIz"),
		"auth eyJaaAaaAaAAAaA0AaA0.aaAaaAAaAaAaAaA0Aa0.a0aaaaA0aAAaAAAa"},
	{"postgres dsn password", "dial postgres://app:S3cr3t-pw@db.internal:5432/app failed", "dial postgres://app:A0aa0a-aa@db.internal:5432/app failed"},
	{"redis url password", "redis://:hunter2@cache:6379/0", "redis://:aaaaaa0@cache:6379/0"},
	{"pem block", j("-----BEGIN RSA ", "PRIVATE KEY-----\nMIIEab12\n-----END RSA ", "PRIVATE KEY-----"),
		j("-----BEGIN RSA ", "PRIVATE KEY-----\nAAAAaa00\n-----END RSA ", "PRIVATE KEY-----")},
	{"pem header cut off", j("loaded -----BEGIN OPENSSH ", "PRIVATE KEY----- b3BlbnNzaC1rZXk="), j("loaded -----BEGIN OPENSSH ", "PRIVATE KEY----- a0AaaaAaaA0aAAa=")},
}

func TestRedact_PrefixedCredentials(t *testing.T) {
	for _, tc := range fakeCredentials {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactLikelyCredentials(tc.in); got != tc.want {
				t.Fatalf("RedactLikelyCredentials(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Ordinary log content that carries the root cause must come through
// untouched: these look random or structured but are not credentials.
func TestRedact_LeavesOrdinaryLogsAlone(t *testing.T) {
	for _, line := range []string{
		"connect to 192.0.2.10:5432 timed out after 3000ms",
		"upstream [2001:db8::1]:443 reset by peer",
		"request_id=3f2504e0-4f89-11d3-9a0c-0305e82c3301 status=502",
		"deployed commit 43b1e7a9c0ffee43b1e7a9c0ffee43b1e7a9c0ff to prod",
		"pod checkout-7d9f8b6c5d-x2x9q OOMKilled (exit 137)",
		"2026-10-04T09:30:00.123Z GET /api/v1/orders/12345 took 2.3s",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"short sk-abc and skeleton-key and task-123",
		"see https://gitea.example.com/ops-team/repo/issues/12",
		"http://user@host/path has no password",
		"-----BEGIN CERTIFICATE----- is public",
		"eyJ alone is not a token",
		"AKIA is just four letters here",
	} {
		if got := RedactLikelyCredentials(line); got != line {
			t.Errorf("ordinary line changed:\n  in  %q\n  out %q", line, got)
		}
	}
}

func TestRedact_Idempotent(t *testing.T) {
	inputs := []string{"password=abc123 Bearer abcdefgh123"}
	for _, tc := range fakeCredentials {
		inputs = append(inputs, tc.in)
	}
	for _, in := range inputs {
		once := RedactLikelyCredentials(in)
		if twice := RedactLikelyCredentials(once); twice != once {
			t.Errorf("not idempotent for %q:\n once %q\ntwice %q", in, once, twice)
		}
	}
}

// FuzzRedact checks that redaction never panics, never changes the
// length in runes (Shape is length-preserving, and nothing else is
// rewritten), and is idempotent.
func FuzzRedact(f *testing.F) {
	for _, tc := range fakeCredentials {
		f.Add(tc.in)
	}
	f.Add("password=hunter2 token: abcd Bearer xyz.123456789")
	f.Add("postgres://a:b@c")
	f.Add("-----BEGIN PRIVATE KEY-----") // leakcheck:allow (header-only fuzz seed)
	f.Fuzz(func(t *testing.T, s string) {
		once := RedactLikelyCredentials(s)
		if len([]rune(once)) != len([]rune(s)) {
			t.Fatalf("length changed: %q -> %q", s, once)
		}
		if twice := RedactLikelyCredentials(once); twice != once {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, once, twice)
		}
	})
}
