// Package redact scrubs common secret shapes out of transcript content
// before it lands in the index or gets written back to a source file.
//
// The bulk of the rule set is vendored from the gitleaks project
// (https://github.com/gitleaks/gitleaks, MIT). Provenance and refresh
// instructions live in gitleaks_rules.go. A small layer of hand-rolled
// rules below covers context-preserving cases where the secret sits
// inside a surrounding string we want to keep (`Authorization:` headers,
// passwords embedded in URLs, PEM-armored key blocks that gitleaks
// rejects under RE2).
//
// Every match is replaced with [REDACTED:<id>] — a placeholder that's
// safe inside a JSON string (no quotes, backslashes, or control bytes),
// so applying redaction directly to a raw JSONL line won't break its
// parser.
//
// This is a best-effort filter, not a guarantee. High-entropy random
// strings with no distinguishing prefix will slip through; that's the
// price of not drowning legitimate hashes and hex IDs in false positives.
package redact

import (
	"regexp"
	"strings"
)

// namedPattern is what both the vendored gitleaks rules and the
// hand-rolled extras compile down to. A pattern with no capture groups
// replaces the whole match with [REDACTED:<id>]; a pattern with one or
// more groups replaces the first group, so any keyword or header we
// used as context anchor stays visible in the output.
type namedPattern struct {
	id string
	// One of `raw` or `re` is set. `raw` holds the source regex for
	// lazy compilation — with 200+ rules, deferring until first use
	// keeps init cheap.
	raw string
	re  *regexp.Regexp
}

// extraRules are the hand-rolled patterns that survive after gitleaks
// takes a first pass. They're context-anchored so the placeholder lands
// in the middle of a readable string.
var extraRules = []namedPattern{
	// PEM-armored private keys. Gitleaks' private-key rule uses [\s\S]
	// which RE2 handles but over-matches across blocks; our variant
	// uses (?s) with a non-greedy body.
	{id: "private-key", raw: `(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`},

	// JSON Web Tokens — three base64url segments separated by dots,
	// starting with eyJ (base64 of `{"`). Gitleaks' jwt rule has an
	// extra trailing char class that RE2 would keep out of the match;
	// keep ours simple.
	{id: "jwt", raw: `\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`},

	// Authorization: Bearer <token>. Capture the header prefix so it
	// stays readable after redaction. Case-insensitive.
	{id: "bearer-token", raw: `(?i)Authorization:\s*Bearer\s+([A-Za-z0-9._~+/=-]{8,})`},

	// Authorization: Basic <base64>.
	{id: "basic-auth", raw: `(?i)Authorization:\s*Basic\s+([A-Za-z0-9+/=]{8,})`},

	// Password in a URL: scheme://user:PASSWORD@host. Keep user and
	// host visible — the password is the only thing we replace.
	{id: "url-password", raw: `\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@`},
}

// compiled is the merged rule set, built lazily on first call.
var compiled []namedPattern

func ensureCompiled() {
	if compiled != nil {
		return
	}
	out := make([]namedPattern, 0, len(gitleaksRules)+len(extraRules))
	for _, p := range gitleaksRules {
		re, err := regexp.Compile(p.raw)
		if err != nil {
			continue
		}
		out = append(out, namedPattern{id: p.id, re: re})
	}
	for _, p := range extraRules {
		re, err := regexp.Compile(p.raw)
		if err != nil {
			continue
		}
		out = append(out, namedPattern{id: p.id, re: re})
	}
	compiled = out
}

// Redact returns s with every matched secret replaced by a placeholder.
// A string with no matches is returned unchanged.
func Redact(s string) string {
	ensureCompiled()
	for _, p := range compiled {
		s = applyPattern(s, p)
	}
	return s
}

// RedactBytes is the []byte counterpart for file-rewriting callers. The
// conversion round-trip is unavoidable without duplicating the pattern
// loop — fine in practice, since source rewrites are rare.
func RedactBytes(b []byte) []byte {
	return []byte(Redact(string(b)))
}

// HasSecret reports whether s appears to contain any known secret
// shape. Used by the source-rewriter to skip untouched files.
func HasSecret(s string) bool {
	ensureCompiled()
	for _, p := range compiled {
		if p.re.MatchString(s) {
			return true
		}
	}
	return false
}

// applyPattern replaces either the first capture group (when present)
// or the whole match with the pattern's placeholder. Replacing just the
// group keeps anchoring context — `api_key=[REDACTED:…]` reads better
// than `[REDACTED:…]`.
func applyPattern(s string, p namedPattern) string {
	if !p.re.MatchString(s) {
		return s
	}
	placeholder := "[REDACTED:" + p.id + "]"
	if p.re.NumSubexp() == 0 {
		return p.re.ReplaceAllString(s, placeholder)
	}
	idxs := p.re.FindAllStringSubmatchIndex(s, -1)
	if len(idxs) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range idxs {
		// m[0], m[1] = full match; m[2], m[3] = first capture group.
		start, end := m[2], m[3]
		if start < 0 || end < 0 {
			// Group didn't participate in this match — drop the whole
			// span instead.
			start, end = m[0], m[1]
		}
		b.WriteString(s[last:start])
		b.WriteString(placeholder)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}
