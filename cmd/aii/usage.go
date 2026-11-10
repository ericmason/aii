package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// cmdUsage reports token usage recorded at index time. Output mirrors
// sessions: pretty on a TTY, ndjson when piped.
func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	agent := fs.String("agent", "", "cc|codex|cursor|pi|opencode|gemini|copilot|aider|cline|roo")
	workspace := fs.String("workspace", "", "exact workspace path ('.' for the current directory)")
	since := fs.String("since", "", "30d | 2026-01-01 (a bare date is local midnight)")
	until := fs.String("until", "", "2026-01-31 — last day to include, or an exclusive RFC3339 time")
	by := fs.String("by", "day", "day|month|model|agent|workspace|session, comma-separated")
	limit := fs.Int("limit", 0, "max rows (0 = all)")
	jsonOut := fs.Bool("json", false, "emit one JSON object with rows and totals")
	ndjsonOut := fs.Bool("ndjson", false, "emit one JSON line per row")
	agentFlag := fs.Bool("agent-mode", false, "force agent-oriented output (default when stdout is piped)")
	pretty := fs.Bool("pretty", false, "force human-readable output even when piped")
	format := fs.String("format", "", "pretty|json|ndjson — alternative spelling of --pretty/--json/--ndjson")
	fs.Parse(reorderFlags(args))

	warnIfIndexStale()

	sinceUnix, err := parseUsageBound(*since, false)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	untilUnix, err := parseUsageBound(*until, true)
	if err != nil {
		return fmt.Errorf("--until: %w", err)
	}
	var keys []string
	for _, k := range strings.Split(*by, ",") {
		if k = strings.TrimSpace(strings.ToLower(k)); k != "" && k != "total" {
			keys = append(keys, k)
		}
	}

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	citeDB = db

	rows, err := db.UsageReport(store.UsageFilter{
		Agent:     normalizeAgent(*agent),
		Workspace: resolveWorkspaceFlag(*workspace),
		SinceUnix: sinceUnix,
		UntilUnix: untilUnix,
		By:        keys,
		Limit:     *limit,
	})
	if err != nil {
		return err
	}
	for i := range rows {
		if rows[i].SessionUID != "" {
			rows[i].Cite = shortAgent(rows[i].Agent) + "/" + shortUID(rows[i].SessionUID)
		}
	}
	total := sumUsage(rows)

	mode, err := resolveListFormat(*format, *jsonOut, *ndjsonOut, *pretty, *agentFlag)
	if err != nil {
		return err
	}
	switch mode {
	case "json":
		if rows == nil {
			rows = []usageRow{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(struct {
			By     []string   `json:"by"`
			Rows   []usageRow `json:"rows"`
			Totals usageRow   `json:"totals"`
		}{keys, rows, total})
	case "ndjson":
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		for _, r := range rows {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	default:
		return writeUsagePretty(rows, keys, total)
	}
}

type usageRow = store.UsageRow

// parseUsageBound reads a --since/--until value. A bare date is local
// midnight; for --until it means the end of that day, so
// --since 2026-01-05 --until 2026-01-05 covers one whole day.
func parseUsageBound(s string, endOfDay bool) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if endOfDay {
				t = t.AddDate(0, 0, 1)
			}
			return t.Unix(), nil
		}
	}
	return parseSince(s)
}

func sumUsage(rows []usageRow) usageRow {
	var t usageRow
	for _, r := range rows {
		t.Requests += r.Requests
		t.Input += r.Input
		t.Output += r.Output
		t.CacheCreation += r.CacheCreation
		t.CacheRead += r.CacheRead
		t.Total += r.Total
	}
	return t
}

func writeUsagePretty(rows []usageRow, keys []string, total usageRow) error {
	if len(rows) == 0 {
		fmt.Println(cMuted("no token usage recorded for this selection — run `aii index --full` once to record usage from transcripts already indexed"))
		return nil
	}
	label := func(r usageRow) string {
		var parts []string
		for _, k := range keys {
			switch k {
			case "day":
				parts = append(parts, r.Day)
			case "month":
				parts = append(parts, r.Month)
			case "model":
				parts = append(parts, orDash(r.Model))
			case "agent":
				parts = append(parts, r.Agent)
			case "workspace":
				parts = append(parts, orDash(r.Workspace))
			case "session":
				s := r.Cite
				if r.Title != "" {
					s += " " + truncate(r.Title, 50)
				}
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "  ")
	}
	width := len("total")
	for _, r := range rows {
		if n := len([]rune(label(r))); n > width {
			width = n
		}
	}
	if width > 90 {
		width = 90
	}
	header := fmt.Sprintf("%s  %s  %s  %s  %s  %s  %s",
		padRight(strings.Join(keys, " / "), width),
		padLeft("requests", 9), padLeft("input", 13), padLeft("output", 13),
		padLeft("cache write", 14), padLeft("cache read", 15), padLeft("total", 15))
	fmt.Println(cMuted(header))
	fmt.Println(cMuted(strings.Repeat("─", len([]rune(header)))))
	line := func(name string, r usageRow) string {
		return fmt.Sprintf("%s  %s  %s  %s  %s  %s  %s",
			padRight(truncate(name, width), width),
			padLeft(commas(r.Requests), 9), padLeft(commas(r.Input), 13), padLeft(commas(r.Output), 13),
			padLeft(commas(r.CacheCreation), 14), padLeft(commas(r.CacheRead), 15), padLeft(commas(r.Total), 15))
	}
	for _, r := range rows {
		fmt.Println(line(label(r), r))
	}
	if len(rows) > 1 {
		fmt.Println(cMuted(strings.Repeat("─", len([]rune(header)))))
		fmt.Println(cBold(line("total", total)))
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// commas formats n with thousands separators.
func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
