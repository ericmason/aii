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
		{`(AND) NEAR (foo)`, `"AND"* "NEAR"* foo*`},
		// Uppercase AND/OR/NOT between terms are boolean operators.
		{`download OR extract OR app`, `download* OR extract* OR app*`},
		{`auth AND token NOT oauth`, `auth* AND token* NOT oauth*`},
		{`auth OR "exact phrase"`, `auth* OR "exact phrase"`},
		// Operators without a term on each side are dropped.
		{`OR auth`, `auth*`},
		{`auth OR`, `auth*`},
		{`auth OR AND token`, `auth* AND token*`},
		{`NOT`, `""`},
		{`auth OR !!!`, `auth*`},
		{`auth NOT !!! token`, `auth* token*`},
		{`NOT auth`, `""`},
		{`NOT auth token`, `token*`},
		// Lowercase operator words are ordinary search terms.
		{`this or that`, `this* or* that*`},
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
		// Operators pass through. If a short term is dropped from a query
		// with operators, the trigram branch is skipped entirely.
		{`download OR app`, `"download" OR "app"`},
		{`download NOT scratch`, `"download" NOT "scratch"`},
		{`ab OR download`, ""},
		{`download NOT ab`, ""},
		{`download AND ab OR zzzz`, ""},
		{`a OR NOT download`, ""},
		// A leading NOT excludes; it never turns into a search for its term.
		{`NOT download`, ""},
		{`NOT download extract`, ""},
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
