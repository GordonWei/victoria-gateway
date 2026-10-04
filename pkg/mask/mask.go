// Package mask provides an opt-in, shape-preserving redaction for log text
// handled by victoria-gateway. It exists because a captured log line goes
// to several places (the summarizer and cloud prompts, the embedder,
// Postgres, and the unauthenticated /incidents pages — see the README's
// "What data this stores, and where it goes" section), and an operator
// pointing this at a real production Loki may not want raw
// credential-shaped strings in any of them.
//
// This is opt-in and off by default (rag.mask_log_excerpt: true) for a
// reason spelled out in the README: for most alerts, the actual content of
// an error message — an IP, a port, a status code — is the root-cause
// signal, not noise to hide. Masking is only worth the tradeoff for the
// narrow case of credential-shaped substrings, never applied to a whole
// excerpt indiscriminately.
package mask

import (
	"regexp"
	"strings"
)

// credentialPattern matches "key=value"/"key: value"-shaped assignments
// where the key looks like a secret (password, token, api key, secret,
// credential — case-insensitive, with common separators) and value is a
// contiguous non-whitespace run of at least 4 characters. Neither
// heuristic is exhaustive — this cannot replace not logging secrets in
// the first place — but it catches the common `password="..."`,
// `token: ...` shapes without needing to know anything about a specific
// application's log format. "authorization" is deliberately not one of
// the keys here: an `Authorization: Bearer <token>` header's value starts
// with a scheme name, not the secret itself, and would wrongly mask the
// literal word "Bearer" — see bearerPattern below, which handles that
// shape correctly and runs first.
var credentialPattern = regexp.MustCompile(
	`(?i)(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|credential)\s*[:=]\s*"?([^\s"]{4,})"?`,
)

// bearerPattern matches "Bearer <token>" specifically, since that value
// has no key= prefix for credentialPattern to anchor on, and must run
// before credentialPattern so "Authorization: Bearer ..." isn't
// double-processed (see credentialPattern's doc comment).
var bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+([A-Za-z0-9\-_.~+/]{8,}=*)`)

// prefixedPatterns catch credentials that carry no "key=" to anchor on
// but are recognizable by a fixed vendor prefix or a rigid structure. Only
// such shapes are listed: anything matched by length or randomness alone
// (git SHAs, UUIDs, request IDs) is root-cause evidence far more often
// than it is a secret, and masking it would hide the signal this whole
// feature is careful to keep. Group 1 is kept verbatim (the prefix, so a
// reader can still tell which kind of credential was there); group 2 is
// replaced by its Shape. Because a shaped value still has the same
// prefix and structure, redacting twice gives the same result as once.
var prefixedPatterns = []*regexp.Regexp{
	// AWS access key IDs (long-term AKIA, temporary ASIA, and the other
	// documented unique-ID prefixes).
	regexp.MustCompile(`\b(AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)([0-9A-Z]{16})\b`),
	// GitHub tokens: classic (ghp_/gho_/ghu_/ghs_/ghr_) and fine-grained.
	regexp.MustCompile(`\b(gh[pousr]_)([A-Za-z0-9]{36,})\b`),
	regexp.MustCompile(`\b(github_pat_)([A-Za-z0-9_]{22,})\b`),
	// GitLab personal access tokens.
	regexp.MustCompile(`\b(glpat-)([A-Za-z0-9_-]{20,})`),
	// Slack tokens.
	regexp.MustCompile(`\b(xox[abposr]-)([A-Za-z0-9-]{10,})`),
	// Slack incoming webhook URL path (the path is the credential).
	regexp.MustCompile(`(hooks\.slack\.com/services/)([A-Za-z0-9/]{20,})`),
	// Google API keys.
	regexp.MustCompile(`\b(AIza)([0-9A-Za-z_-]{35})`),
	// Anthropic / OpenAI style secret keys. The 20-character floor keeps
	// short "sk-..." identifiers out.
	regexp.MustCompile(`\b(sk-(?:ant-|proj-)?)([A-Za-z0-9_-]{20,})`),
	// Stripe secret and restricted keys.
	regexp.MustCompile(`\b([sr]k_live_)([0-9A-Za-z]{16,})`),
	// Telegram bot tokens, bare or inside an api.telegram.org/bot<token>/ URL.
	regexp.MustCompile(`(?:^|[^0-9A-Za-z]|bot)([0-9]{8,10}:)(AA[A-Za-z0-9_-]{33})`),
	// JWTs: three base64url segments, the first two JSON objects ("eyJ").
	regexp.MustCompile(`\b(eyJ)([A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`),
	// Passwords in URL userinfo: scheme://user:password@host, which is
	// also how most database DSNs (postgres://, mysql://, redis://,
	// amqp://, mongodb://) carry them. The user name stays readable.
	regexp.MustCompile(`(\b[A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]*:)([^/\s@]+)@`),
}

// pemPrivateKey matches a PEM private key from its BEGIN line through its
// END line, or, when a log line was cut before the END line, through the
// end of the text: anything after a private-key header is key material.
var pemPrivateKey = regexp.MustCompile(`(?s)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----)(.*?)(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)

// Shape rewrites s character-by-character: digits become '0', lowercase
// letters become 'a', uppercase letters become 'A', and every other
// character (punctuation, whitespace, unicode) is left untouched. The
// result is irreversible — it preserves length and the visual "shape" of
// the original (useful for spotting e.g. a UUID pattern) without
// preserving any of its actual value.
func Shape(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			out = append(out, '0')
		case r >= 'a' && r <= 'z':
			out = append(out, 'a')
		case r >= 'A' && r <= 'Z':
			out = append(out, 'A')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// RedactLikelyCredentials scans s for substrings that look like credential
// assignments, bearer tokens, PEM private keys or vendor-prefixed tokens
// (see prefixedPatterns) and replaces just the value portion with
// its Shape — the surrounding text (the key name, punctuation, everything
// else in the log line) is left exactly as-is, so the excerpt stays
// readable for diagnosing what actually happened. Call sites should only
// use this when an operator has explicitly opted in (see rag.mask_log_excerpt
// in config.yaml) — see the package doc for why this isn't the default.
func RedactLikelyCredentials(s string) string {
	s = bearerPattern.ReplaceAllStringFunc(s, func(m string) string {
		loc := bearerPattern.FindStringSubmatchIndex(m)
		if len(loc) < 4 {
			return m
		}
		valStart, valEnd := loc[2], loc[3]
		return m[:valStart] + Shape(m[valStart:valEnd]) + m[valEnd:]
	})
	s = pemPrivateKey.ReplaceAllStringFunc(s, func(m string) string {
		loc := pemPrivateKey.FindStringSubmatchIndex(m)
		return m[:loc[4]] + Shape(m[loc[4]:loc[5]]) + m[loc[5]:]
	})
	for _, re := range prefixedPatterns {
		s = shapeGroup(re, s, 2)
	}
	s = credentialPattern.ReplaceAllStringFunc(s, func(m string) string {
		loc := credentialPattern.FindStringSubmatchIndex(m)
		if len(loc) < 6 {
			return m
		}
		valStart, valEnd := loc[4], loc[5]
		return m[:valStart] + Shape(m[valStart:valEnd]) + m[valEnd:]
	})
	return s
}

// shapeGroup replaces submatch group g of every match of re in s with its
// Shape, leaving the rest of each match untouched.
func shapeGroup(re *regexp.Regexp, s string, g int) string {
	locs := re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, loc := range locs {
		start, end := loc[2*g], loc[2*g+1]
		if start < 0 {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(Shape(s[start:end]))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}
