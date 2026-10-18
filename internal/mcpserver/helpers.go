package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// NormalizeAgent mirrors the CLI's agent alias table. Exposed (capital)
// so tests can reuse it; the MCP handler uses it to accept both "cc"
// and "claude_code" from clients.
func NormalizeAgent(a string) string {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "", "all":
		return ""
	case "cc", "claude", "claude_code":
		return "claude_code"
	case "codex", "cdx":
		return "codex"
	case "cursor", "cur":
		return "cursor"
	case "pi":
		return "pi"
	case "opencode", "oc":
		return "opencode"
	case "gemini", "gem":
		return "gemini"
	case "copilot", "cop":
		return "copilot"
	case "aider", "aid":
		return "aider"
	case "cline", "cln":
		return "cline"
	case "roo", "roo_code":
		return "roo_code"
	}
	return a
}

// NormalizeRole maps loose role names to the values actually stored
// in the messages table.
func NormalizeRole(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "", "all":
		return ""
	case "user", "u", "human":
		return "user"
	case "assistant", "a", "ai", "asst":
		return "assistant"
	case "thinking", "t", "thnk", "think":
		return "thinking"
	case "tool":
		return "tool"
	case "system", "sys":
		return "system"
	}
	return r
}

// ShortAgent is the 3-letter code that appears in cite tokens.
func ShortAgent(a string) string {
	switch a {
	case "claude_code":
		return "cc"
	case "codex":
		return "cdx"
	case "cursor":
		return "cur"
	case "opencode":
		return "oc"
	case "gemini":
		return "gem"
	case "copilot":
		return "cop"
	case "aider":
		return "aid"
	case "cline":
		return "cln"
	case "roo_code":
		return "roo"
	}
	return a
}

// ShortUID is the id prefix used in cite tokens: the shortest prefix,
// at least 8 chars, that no other indexed session shares.
func ShortUID(db *store.DB, uid string) string { return db.CiteUID(uid) }

// RefAgent returns the agent named by a cite token's code ("cdx/…" →
// "codex"), or "" when the ref has no agent prefix. Lookups use it to
// pick between sessions of different agents that share an id.
func RefAgent(ref string) string {
	ref = strings.TrimSpace(ref)
	i := strings.Index(ref, "/")
	if i <= 0 {
		return ""
	}
	return NormalizeAgent(ref[:i])
}

// parseSessionRef accepts either a bare uid (full or any unique prefix)
// or a cite token like "cc/abc12345:42". Returns (uid, ordinal) where
// ordinal is -1 when the ref didn't carry one. The agent prefix is
// informational only — SessionByUIDAny ignores it, looks up by uid
// prefix, and returns an error listing the candidates when the prefix
// matches more than one session.
func parseSessionRef(ref string) (string, int) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", -1
	}
	// Strip agent prefix if present: "cc/abc:42" → "abc:42"
	if i := strings.Index(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	// opencode prints session ids as ses_<id>; the index stores <id>.
	ref = strings.TrimPrefix(ref, "ses_")
	ordinal := -1
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		var n int
		if _, err := fmt.Sscanf(ref[i+1:], "%d", &n); err == nil {
			ordinal = n
			ref = ref[:i]
		}
	}
	return ref, ordinal
}

// intArg pulls a named float from an arguments map and rounds it down
// to an int. MCP JSON numbers come through as float64 from
// json.Unmarshal into interface{}.
func intArg(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		return floatToInt(v)
	}
	return def
}

func floatToInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case float32:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

// parseSince duplicates the CLI's since parser so the MCP package
// doesn't import cmd. Accepts 7d / 24h / 30m / 1w / YYYY-MM-DD /
// RFC3339. Empty input returns 0 (no filter).
func parseSince(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if len(s) > 1 {
		unit := s[len(s)-1]
		if unit == 'd' || unit == 'h' || unit == 'm' || unit == 'w' {
			var n int
			if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &n); err == nil && n > 0 {
				var dur time.Duration
				switch unit {
				case 'm':
					dur = time.Duration(n) * time.Minute
				case 'h':
					dur = time.Duration(n) * time.Hour
				case 'd':
					dur = time.Duration(n) * 24 * time.Hour
				case 'w':
					dur = time.Duration(n) * 7 * 24 * time.Hour
				}
				return time.Now().Add(-dur).Unix(), nil
			}
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Unix(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("invalid since %q (expected 7d, 2h, or 2026-01-01)", s)
}

// oneLine flattens a multi-line string for compact text fallbacks.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len([]rune(s)) > 180 {
		r := []rune(s)
		s = string(r[:180]) + "…"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
