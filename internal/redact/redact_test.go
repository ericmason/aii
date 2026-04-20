package redact

import (
	"strings"
	"testing"
)

func TestRedactKnownSecrets(t *testing.T) {
	// Most gitleaks rules end in a capture-group anchor like
	// `(?:[\x60'"\s;]|\\[nr]|$)` — we add a trailing space so the
	// boundary matches. Lengths mirror the upstream regex exactly
	// (e.g. Anthropic api03 keys are 93 chars + "AA").
	cases := []struct {
		name, in, wantLabel string
	}{
		{"anthropic", "key=sk-ant-api03-" + strings.Repeat("a", 93) + "AA ", "anthropic-api-key"},
		{"openai_sk", "sk-" + strings.Repeat("a", 20) + "T3BlbkFJ" + strings.Repeat("b", 20) + " ", "openai-api-key"},
		{"github_classic", "ghp_" + strings.Repeat("A", 36), "github-pat"},
		{"github_pat", "github_pat_" + strings.Repeat("A", 82), "github-fine-grained-pat"},
		// Construct Slack fixtures at runtime — GitHub push protection
		// flags the literal shape even in a test file.
		{"slack_bot", "xox" + "b-" + "1234567890-" + "1234567890" + strings.Repeat("a", 10), "slack-bot-token"},
		{"aws", "id=AKIAIOSFODNN7EXAMPLE ", "aws-access-token"},
		{"stripe", "token=sk_live_" + strings.Repeat("9", 30) + " ", "stripe-access-token"},
		{"npm", "npm_" + strings.Repeat("b", 36) + " ", "npm-access-token"},
		{"jwt", "Authorization: Bearer eyJabcdefghij.eyJ" + strings.Repeat("a", 40) + "." + strings.Repeat("b", 20), "jwt"},
		{"url_password", "psql postgres://user:hunter2@db.example.com/mydb", "url-password"},
		{"bearer", "Authorization: Bearer abc123def456ghi789jkl", "bearer-token"},
		{"basic", "Authorization: Basic dXNlcjpwYXNzd29yZA==", "basic-auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if !strings.Contains(got, "[REDACTED:"+tc.wantLabel+"]") {
				t.Fatalf("want label %q, got %q", tc.wantLabel, got)
			}
			if got == tc.in {
				t.Fatalf("input unchanged: %q", tc.in)
			}
		})
	}
}

func TestRedactPrivateKeyBlock(t *testing.T) {
	in := "noise\n-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----\nmore"
	got := Redact(in)
	if !strings.Contains(got, "[REDACTED:private-key]") {
		t.Fatalf("want PEM block redacted, got %q", got)
	}
	if strings.Contains(got, "abc") {
		t.Fatalf("key body leaked: %q", got)
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	in := "token: ghp_" + strings.Repeat("Z", 36)
	once := Redact(in)
	twice := Redact(once)
	if once != twice {
		t.Fatalf("not idempotent: %q -> %q", once, twice)
	}
}

func TestRedactLeavesPlainTextAlone(t *testing.T) {
	in := "nothing to see here — fix the webhook retry logic and run go test ./..."
	if got := Redact(in); got != in {
		t.Fatalf("false positive: %q -> %q", in, got)
	}
	if HasSecret(in) {
		t.Fatalf("HasSecret false positive on %q", in)
	}
}

func TestRedactPreservesContextAroundCapture(t *testing.T) {
	// Capture-group replacement should leave the `Authorization:`
	// header name in place so the redacted line is still readable.
	in := "Authorization: Bearer abc123def456ghi789jkl"
	got := Redact(in)
	if !strings.HasPrefix(got, "Authorization: Bearer ") {
		t.Fatalf("lost header prefix: %q", got)
	}
	if !strings.HasSuffix(got, "[REDACTED:bearer-token]") {
		t.Fatalf("want placeholder at end, got %q", got)
	}
}

func TestRedactPreservesJSONSafety(t *testing.T) {
	// Placeholder must not contain characters that would break a JSON
	// string — no quotes, backslashes, or control bytes.
	in := "sk-ant-api03-" + strings.Repeat("x", 93) + "AA "
	out := Redact(in)
	for _, r := range out {
		if r == '"' || r == '\\' || r < 0x20 {
			t.Fatalf("unsafe rune %q in %q", r, out)
		}
	}
}

func TestRedactBytes(t *testing.T) {
	in := []byte(`{"content":"key=ghp_` + strings.Repeat("Q", 36) + `"}`)
	out := RedactBytes(in)
	if !strings.Contains(string(out), "[REDACTED:github-pat]") {
		t.Fatalf("want redaction in %q", out)
	}
}

// Spot-check a few services that only exist in the gitleaks rule set —
// proves the vendored rules are actually being used.
func TestRedactCoversVendoredServices(t *testing.T) {
	cases := []struct {
		name, in, wantLabel string
	}{
		{"age", "AGE-SECRET-KEY-1" + strings.Repeat("Q", 58), "age-secret-key"},
		{"1password", "A3-ABCDEF-ABCDEFGHIJK-ABCDE-ABCDE-ABCDE", "1password-secret-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			if !strings.Contains(got, "[REDACTED:"+tc.wantLabel+"]") {
				t.Fatalf("want %q in %q", tc.wantLabel, got)
			}
		})
	}
}
