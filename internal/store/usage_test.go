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
