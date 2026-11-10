package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	srcpkg "github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// fileSource scans one rollout file, so tests don't depend on ~/.codex.
type fileSource struct {
	s    *Source
	path string
}

func (f fileSource) Name() string { return "codex" }
func (f fileSource) Scan(ctx context.Context, db *store.DB, full bool, out chan<- srcpkg.Batch) error {
	return f.s.scanRolloutFile(ctx, db, full, out, f.path)
}

func metaLine(id, extra string) string {
	return fmt.Sprintf(`{"timestamp":"2026-01-02T10:00:00Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-01-02T10:00:00Z","cwd":"/w"%s}}`, id, extra) + "\n"
}

func tokenLine(ts string, in, cached, out int64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d},"last_token_usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"total_tokens":2}}}}`,
		ts, in, cached, out, in+out) + "\n"
}

const (
	turnLine     = `{"timestamp":"2026-01-02T10:00:01Z","type":"turn_context","payload":{"model":"gpt-test"}}` + "\n"
	nullInfoLine = `{"timestamp":"2026-01-02T10:00:01Z","type":"event_msg","payload":{"type":"token_count","info":null}}` + "\n"
	userMsgLine  = `{"timestamp":"2026-01-02T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
)

func index(t *testing.T, db *store.DB, path string, full bool) {
	t.Helper()
	if _, err := indexer.Run(context.Background(), db, []srcpkg.Source{fileSource{New(), path}}, indexer.Options{Full: full}); err != nil {
		t.Fatal(err)
	}
}

func report(t *testing.T, db *store.DB, by ...string) []store.UsageRow {
	t.Helper()
	rows, err := db.UsageReport(store.UsageFilter{By: by})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestRolloutUsageIsLastRunningTotal(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := filepath.Join(dir, "rollout-a.jsonl")
	first := metaLine("sess-1", "") + turnLine + userMsgLine + nullInfoLine +
		tokenLine("2026-01-02T10:00:05Z", 1000, 600, 50) +
		// token_count repeats the running total when no request was made.
		tokenLine("2026-01-02T10:00:06Z", 1000, 600, 50)
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	index(t, db, path, false)

	// Appended on the next day; the next run reads only these bytes.
	more := tokenLine("2026-01-03T10:00:00Z", 2500, 1800, 120)
	if err := os.WriteFile(path, []byte(first+more), 0o644); err != nil {
		t.Fatal(err)
	}
	index(t, db, path, false)

	check := func(label string) {
		t.Helper()
		rows := report(t, db, "session", "model")
		if len(rows) != 1 {
			t.Fatalf("%s: got %d rows, want 1", label, len(rows))
		}
		r := rows[0]
		// The last running total: 2500 input of which 1800 cached, 120 output.
		if r.SessionUID != "sess-1" || r.Model != "gpt-test" || r.Input != 700 || r.CacheRead != 1800 || r.Output != 120 || r.Requests != 2 {
			t.Fatalf("%s: row = %+v", label, r)
		}
		days := report(t, db, "day")
		if len(days) != 2 || days[0].Total != 1050 || days[1].Total != 2620-1050 {
			t.Fatalf("%s: by day = %+v", label, days)
		}
	}
	check("incremental")

	index(t, db, path, true)
	check("after full re-index")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	index(t, db, filepath.Join(dir, "missing.jsonl"), false)
	check("after the rollout was deleted")
}

func TestForkedRolloutKeepsItsOwnSessionID(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := filepath.Join(dir, "rollout-child.jsonl")
	data := metaLine("child", `,"forked_from_id":"parent"`) + metaLine("parent", "") + turnLine + userMsgLine +
		tokenLine("2026-01-02T10:00:05Z", 100, 0, 10)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	index(t, db, path, false)

	if s, _ := db.SessionByUID("codex", "child"); s == nil {
		t.Fatal("forked rollout not indexed under its own id")
	}
	if s, _ := db.SessionByUID("codex", "parent"); s != nil {
		t.Fatal("forked rollout indexed under its parent's id")
	}

	// An incremental read past the first line still finds the child's id.
	more := tokenLine("2026-01-02T10:00:09Z", 300, 0, 30)
	if err := os.WriteFile(path, []byte(data+more), 0o644); err != nil {
		t.Fatal(err)
	}
	index(t, db, path, false)
	rows := report(t, db, "session")
	if len(rows) != 1 || rows[0].SessionUID != "child" || rows[0].Total != 330 {
		t.Fatalf("rows = %+v", rows)
	}
}

// A running total that restarts inside one rollout (a resumed session)
// counts in full from the restart, also when the restart arrives in a
// later incremental read.
func TestRolloutRunningTotalRestart(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	path := filepath.Join(dir, "rollout-r.jsonl")
	first := metaLine("sess-r", "") + turnLine + userMsgLine +
		tokenLine("2026-01-02T10:00:05Z", 1000, 0, 100) +
		tokenLine("2026-01-02T11:00:00Z", 5000, 0, 500)
	more := tokenLine("2026-01-03T09:00:00Z", 300, 0, 30) + // restart
		tokenLine("2026-01-03T10:00:00Z", 800, 0, 80)
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	index(t, db, path, false)
	if err := os.WriteFile(path, []byte(first+more), 0o644); err != nil {
		t.Fatal(err)
	}
	index(t, db, path, false)

	// Seq is the line's absolute byte offset, even in the second read.
	data := first + more
	var want []int64
	for off := 0; off < len(data); {
		end := off + strings.IndexByte(data[off:], '\n') + 1
		if strings.Contains(data[off:end], `"token_count"`) {
			want = append(want, int64(off))
		}
		off = end
	}
	rows, err := db.Query(`SELECT seq FROM token_usage ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	rows.Close()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("seq = %v, want %v", got, want)
	}

	check := func(label string) {
		t.Helper()
		days := report(t, db, "day")
		if len(days) != 2 || days[0].Total != 5500 || days[1].Total != 880 {
			t.Fatalf("%s: by day = %+v", label, days)
		}
	}
	check("incremental")
	index(t, db, path, true)
	check("after full re-index")
}

// Older index runs stored a forked rollout under its parent's id. The
// next read moves the fork's messages to its own session and drops the
// stale row, whether the read is incremental, full, or a first read with
// no saved offset.
func TestForkedRolloutDropsStaleParentRow(t *testing.T) {
	for _, tc := range []struct {
		name            string
		full, saveState bool
	}{{"incremental", false, true}, {"full", true, true}, {"no saved offset", false, false}} {
		full := tc.full
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			path := filepath.Join(dir, "rollout-child.jsonl")
			data := metaLine("child", `,"forked_from_id":"parent"`) + metaLine("parent", "") + turnLine + userMsgLine
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			// What an older index run left behind.
			id, err := db.UpsertSession(&store.Session{Agent: "codex", UID: "parent", SourcePath: path})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if err := db.InsertMessages(tx, id, []store.Message{{Ordinal: 0, Role: "user", Content: "hello"}}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.saveState {
				if err := db.SaveState(store.IndexState{SourcePath: path, MtimeNs: fi.ModTime().UnixNano(), Size: fi.Size(), LastOffset: fi.Size()}); err != nil {
					t.Fatal(err)
				}
			}

			// The fork grows, then gets indexed.
			more := `{"timestamp":"2026-01-02T10:00:09Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}}` + "\n"
			if err := os.WriteFile(path, []byte(data+more), 0o644); err != nil {
				t.Fatal(err)
			}
			index(t, db, path, full)

			if s, _ := db.SessionByUID("codex", "parent"); s != nil {
				t.Fatal("stale parent row still stored for the fork's file")
			}
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM messages m JOIN sessions s ON s.id = m.session_id
                                   WHERE s.agent = 'codex' AND s.session_uid = 'child'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Fatalf("child has %d messages, want 2", n)
			}
		})
	}
}
