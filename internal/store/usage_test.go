package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func insertUsage(t *testing.T, db *DB, rows ...Usage) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertUsage(tx, rows); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func localNoon(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 12, 0, 0, 0, time.Local).Unix()
}

func TestUsageReportMixesRequestsAndRunningTotals(t *testing.T) {
	db := openTestDB(t)
	seed(t, db, &Session{Agent: "claude_code", UID: "a", Workspace: "/w/a", SourcePath: "/a.jsonl"}, nil)

	insertUsage(t, db,
		Usage{Agent: "claude_code", SessionUID: "a", Key: "m1|r1", TS: localNoon(2026, 1, 2), Model: "m", Input: 1, Output: 10},
		// The same request seen again with a smaller total is ignored...
		Usage{Agent: "claude_code", SessionUID: "a", Key: "m1|r1", TS: localNoon(2026, 1, 2), Model: "m", Input: 1, Output: 2},
		// ...and with a larger total replaces the stored copy.
		Usage{Agent: "claude_code", SessionUID: "a", Key: "m1|r1", TS: localNoon(2026, 1, 2), Model: "m", Input: 1, Output: 20},
		// Running totals for a Codex session over two days, inserted out of order.
		Usage{Agent: "codex", SessionUID: "c", Key: "c|3", TS: localNoon(2026, 1, 3), Model: "g", Input: 300, CacheRead: 1000, Output: 30, Cumulative: true},
		Usage{Agent: "codex", SessionUID: "c", Key: "c|1", TS: localNoon(2026, 1, 2), Model: "g", Input: 100, CacheRead: 400, Output: 10, Cumulative: true},
	)

	days, err := db.UsageReport(UsageFilter{By: []string{"day"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %+v", days)
	}
	if days[0].Day != "2026-01-02" || days[0].Total != 21+510 || days[0].Requests != 2 {
		t.Fatalf("first day = %+v", days[0])
	}
	if days[1].Day != "2026-01-03" || days[1].Input != 200 || days[1].CacheRead != 600 || days[1].Output != 20 {
		t.Fatalf("second day = %+v", days[1])
	}

	// A window that starts after the first running total still counts
	// only the increase inside the window.
	only, err := db.UsageReport(UsageFilter{Agent: "codex", SinceUnix: localNoon(2026, 1, 3) - 3600})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Total != 820 {
		t.Fatalf("windowed codex = %+v", only)
	}

	ws, err := db.UsageReport(UsageFilter{By: []string{"workspace"}, Workspace: "/w/a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Workspace != "/w/a" || ws[0].Total != 21 {
		t.Fatalf("by workspace = %+v", ws)
	}

	if _, err := db.UsageReport(UsageFilter{By: []string{"week"}}); err == nil {
		t.Fatal("unknown grouping accepted")
	}
	empty, err := db.UsageReport(UsageFilter{Agent: "cursor"})
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty selection = %+v, %v", empty, err)
	}
}

// A running total that restarts (a resumed Codex session) counts in full
// from the restart. Snapshots with the same timestamp keep file order,
// even when they were stored in a different order.
func TestUsageReportRunningTotalRestarts(t *testing.T) {
	db := openTestDB(t)
	day1, day2 := localNoon(2026, 1, 2), localNoon(2026, 1, 3)
	cum := func(key string, ts, seq, in, out int64) Usage {
		return Usage{Agent: "codex", SessionUID: "c", Key: key, TS: ts, Seq: seq, Model: "g", Input: in, Output: out, Cumulative: true}
	}
	insertUsage(t, db,
		// Stored out of file order on purpose.
		cum("c|e", day2, 500, 800, 80),
		cum("c|d", day2, 400, 300, 30),
		cum("c|c", day1, 300, 5000, 500),
		cum("c|b", day1, 200, 5000, 500), // repeated without a new request
		cum("c|a", day1, 100, 1000, 100),
	)
	// The restart shares its timestamp with the last snapshot of day 1's
	// run, so only seq puts it after that snapshot.
	insertUsage(t, db, cum("c|f", day1, 350, 50, 5))

	days, err := db.UsageReport(UsageFilter{By: []string{"day"}})
	if err != nil {
		t.Fatal(err)
	}
	// Day 1: 1,100 then +4,400, then a restart at 55. Day 2: 330 is higher
	// than 55, so +275, then +550.
	if len(days) != 2 || days[0].Total != 1100+4400+55 || days[1].Total != 275+550 {
		t.Fatalf("by day = %+v", days)
	}
	if days[0].Requests != 3 || days[1].Requests != 2 {
		t.Fatalf("requests by day = %+v", days)
	}
}

// A request whose timestamp didn't parse is stored with a NULL ts and
// reported under an empty day and month.
func TestUsageReportNullTimestamp(t *testing.T) {
	db := openTestDB(t)
	insertUsage(t, db,
		Usage{Agent: "claude_code", SessionUID: "a", Key: "k1", Model: "m", Input: 5},
		Usage{Agent: "claude_code", SessionUID: "a", Key: "k2", TS: localNoon(2026, 1, 2), Model: "m", Input: 7},
	)
	for _, by := range []string{"day", "month"} {
		rows, err := db.UsageReport(UsageFilter{By: []string{by}})
		if err != nil {
			t.Fatalf("by %s: %v", by, err)
		}
		if len(rows) != 2 || rows[0].Day+rows[0].Month != "" || rows[0].Total != 5 || rows[1].Total != 7 {
			t.Fatalf("by %s = %+v", by, rows)
		}
	}
}

// An index created before token_usage existed gains the table on Open.
func TestOpenAddsUsageTableToOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE token_usage`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'token_usage'`).Scan(&name)
	if err == sql.ErrNoRows {
		t.Fatal("token_usage not created")
	} else if err != nil {
		t.Fatal(err)
	}
}
