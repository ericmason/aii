package store

import "testing"

func TestNormalizeMatch(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", `""`},
		{"   ", `""`},
		{"auth", `auth*`},
		{"auth login", `auth* login*`},
		{`"exact phrase"`, `"exact phrase"`},
		{`auth "exact phrase" token`, `auth* "exact phrase" token*`},
		// FTS operator characters in bare words must not leak into the MATCH
		// expression — sanitizeToken strips non-alphanumerics.
		{`AND OR NEAR (foo)`, `AND* OR* NEAR* foo*`},
		{`semi:colon`, `semicolon*`},
		{`snake_case`, `snake_case*`},
		{`"un"closed`, `"un" closed*`},
		// Unicode letters survive (anything > 127 is preserved).
		{`café`, `café*`},
		// Quoted phrases pass punctuation through (sanitizePhrase only strips quotes).
		{`"!!!"`, `"!!!"`},
	}
	for _, c := range cases {
		if got := normalizeMatch(c.in); got != c.want {
			t.Errorf("normalizeMatch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeMatchTrigram(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		// <3 char tokens get dropped entirely.
		{"a", ""},
		{"ab", ""},
		{"to be", ""},
		{"auth", `"auth"`},
		{"webhookRetry snake_case", `"webhookRetry" "snake_case"`},
		{`"exact phrase"`, `"exact phrase"`},
		// Phrase shorter than 3 chars also dropped.
		{`"hi"`, ""},
		// Mixed short+long: short dropped, long kept.
		{`a longerword`, `"longerword"`},
		// FTS operators sanitized like bare words.
		{`(AND)`, `"AND"`},
	}
	for _, c := range cases {
		if got := normalizeMatchTrigram(c.in); got != c.want {
			t.Errorf("normalizeMatchTrigram(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeToken(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc", "abc"},
		{"abc123", "abc123"},
		{"snake_case", "snake_case"},
		{"camelCase", "camelCase"},
		{"has-dash", "hasdash"},
		{"a.b.c", "abc"},
		{"a:b", "ab"},
		{"(foo)", "foo"},
		{"café", "café"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeToken(c.in); got != c.want {
			t.Errorf("sanitizeToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizePhrase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`hello world`, `hello world`},
		{`has "quotes" inside`, `has quotes inside`},
		{`  trimmed  `, `trimmed`},
		{`""`, ``},
	}
	for _, c := range cases {
		if got := sanitizePhrase(c.in); got != c.want {
			t.Errorf("sanitizePhrase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
