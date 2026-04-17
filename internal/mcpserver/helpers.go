package mcpserver

import (
	"fmt"
	"strings"
	"time"
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
	}
	return a
}

// ShortUID is the 8-char prefix used in cite tokens. Stable across
// calls: we never shuffle uid characters.
func ShortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// parseSessionRef accepts either a bare uid (full or 8-char short) or
// a cite token like "cc/abc12345:42". Returns (uid, ordinal) where
// ordinal is -1 when the ref didn't carry one. The agent prefix is
// informational only — SessionByUIDAny ignores it and looks up by uid
// suffix match, so different agents with colliding uids are
// disambiguated by the first hit (rare; uids are UUIDs).
func parseSessionRef(ref string) (string, int) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", -1
	}
	// Strip agent prefix if present: "cc/abc:42" → "abc:42"
	if i := strings.Index(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
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
