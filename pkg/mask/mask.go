// Package mask provides an opt-in, shape-preserving redaction for log
// excerpts that get stored and displayed by victoria-gateway. It exists
// because RAG's captured log excerpts land in three places (Postgres, the
// summarizer prompt, and the unauthenticated /incidents pages — see the
// README's "What data this stores, and where it goes" section), and an
// operator pointing this at a real production Loki may not want raw
// credential-shaped strings sitting in any of them.
//
// This is opt-in and off by default (rag.mask_log_excerpt: true) for a
// reason spelled out in the README: for most alerts, the actual content of
// an error message — an IP, a port, a status code — is the root-cause
// signal, not noise to hide. Masking is only worth the tradeoff for the
// narrow case of credential-shaped substrings, never applied to a whole
// excerpt indiscriminately.
package mask

import "regexp"

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
// assignments or bearer tokens and replaces just the value portion with
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
