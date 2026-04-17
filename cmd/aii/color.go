package main

import (
	"os"
	"strings"
)

// ANSI SGR helpers, disabled when stdout isn't a tty or NO_COLOR is set.

var (
	colorEnabled = detectColor()
	stdoutIsTTY  = detectTTY()
)

// detectTTY returns true if stdout is attached to a terminal, false if
// it's been redirected to a pipe or file. Agent mode keys off this.
func detectTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" || os.Getenv("CLICOLOR_FORCE") != "" {
		return true
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
	dim   = "\x1b[2m"
	ital  = "\x1b[3m"
)

// sgr wraps text with a semicolon-joined SGR sequence.
func sgr(text string, codes ...string) string {
	if !colorEnabled || len(codes) == 0 || text == "" {
		return text
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + text + reset
}

// Named palette — 16-color safe.
func cDim(s string) string    { return sgr(s, "2") }
func cBold(s string) string   { return sgr(s, "1") }
func cMuted(s string) string  { return sgr(s, "90") }  // bright black
func cHead(s string) string   { return sgr(s, "1", "97") } // bold white
func cAccent(s string) string { return sgr(s, "36") }  // cyan

// Agent badge colors — 256-color codes chosen to evoke each brand:
//   Claude Code → Anthropic clay orange (#D97757-ish)
//   Codex      → OpenAI teal-green (#10A37F-ish)
//   Cursor     → Cursor's dark monochrome (near-black with light text)
func cAgent(agent string) string {
	label := " " + padCenter(short(agent), 3) + " "
	switch agent {
	case "claude_code", "cc":
		return sgr(label, "1", "38;5;231", "48;5;173") // bold white on clay orange
	case "codex", "cdx":
		return sgr(label, "1", "38;5;231", "48;5;29") // bold white on teal
	case "cursor", "cur":
		return sgr(label, "1", "38;5;252", "48;5;237") // light gray on near-black
	}
	return sgr(label, "7")
}

// Role tags — dim pastel foregrounds, no background.
func cRole(role string) string {
	label := roleTag(role)
	switch role {
	case "user":
		return sgr(label, "94") // bright blue
	case "assistant":
		return sgr(label, "95") // bright magenta
	case "thinking":
		return sgr(label, "92") // bright green
	case "tool":
		return sgr(label, "93") // bright yellow
	}
	return sgr(label, "37")
}

func short(a string) string {
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

func roleTag(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "asst"
	case "thinking":
		return "thnk"
	case "tool":
		return "tool"
	}
	if len(role) > 4 {
		return role[:4]
	}
	return role
}

func padCenter(s string, w int) string {
	if len(s) >= w {
		return s
	}
	left := (w - len(s)) / 2
	right := w - len(s) - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

// highlightHit colors « … » snippet regions so matches pop in terminal.
func highlightHit(s string) string {
	if !colorEnabled {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "«")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i+len("«"):]
		j := strings.Index(rest, "»")
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(sgr(rest[:j], "1", "33")) // bold yellow
		s = rest[j+len("»"):]
	}
}
