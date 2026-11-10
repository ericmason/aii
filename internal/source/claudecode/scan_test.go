package claudecode_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/source/claudecode"
	"github.com/ericmason/aii/internal/store"
)

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func userRecord(text string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":"s1","cwd":"/w","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`, text)
}

// A file marked for reindex is read again by the next incremental run,
// even though its mtime and size have not changed since the run that
// could not write it. Without the mark, that run skips the file as
// unchanged and the messages are never indexed.
func TestIncrementalRunReindexesMarkedFile(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s1.jsonl")

	db, err := store.OpenForIndex(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	src := []source.Source{&claudecode.Source{Root: root}}
	run := func() indexer.Stats {
		t.Helper()
		stats, err := indexer.Run(context.Background(), db, src, indexer.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return stats
	}

	appendLine(t, path, userRecord("first turn"))
	appendLine(t, path, userRecord("second turn"))
	if got := run().MessagesAdded; got != 2 {
		t.Fatalf("first run added %d messages, want 2", got)
	}

	// An unchanged file is skipped: this is why a dropped batch does not
	// recover on its own.
	if got := run().SessionsTouch; got != 0 {
		t.Fatalf("second run touched %d sessions, want 0 for an unchanged file", got)
	}

	// Stand in for a run whose batch never reached the database.
	if _, err := db.Exec(`DELETE FROM messages`); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkNeedsReindex(path); err != nil {
		t.Fatalf("MarkNeedsReindex: %v", err)
	}

	if got := run().MessagesAdded; got != 2 {
		t.Fatalf("run after the mark added %d messages, want 2", got)
	}

	state, err := db.GetState(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.NeedsReindex {
		t.Error("the successful run should have cleared the mark")
	}
	if got := run().SessionsTouch; got != 0 {
		t.Errorf("the next run touched %d sessions, want 0: the mark is spent", got)
	}
}

// TestIncrementalRunsKeepFirstTitle indexes a session across three runs.
// The first run sees no user message, so the title comes from the second
// run, and the third run must not replace it with a later turn.
func TestIncrementalRunsKeepFirstTitle(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s1.jsonl")

	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	src := []source.Source{&claudecode.Source{Root: root}}
	run := func() {
		t.Helper()
		if _, err := indexer.Run(context.Background(), db, src, indexer.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	title := func() string {
		t.Helper()
		s, err := db.SessionByUID("claude_code", "s1")
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return ""
		}
		return s.Title
	}

	appendLine(t, path, `{"type":"permission-mode","sessionId":"s1","permissionMode":"default"}`)
	run()
	if got := title(); got != "" {
		t.Fatalf("after run 1 title = %q, want empty", got)
	}

	appendLine(t, path, userRecord("real title"))
	run()
	if got := title(); got != "real title" {
		t.Fatalf("after run 2 title = %q, want %q", got, "real title")
	}

	appendLine(t, path, userRecord("a later turn"))
	run()
	if got := title(); got != "real title" {
		t.Fatalf("after run 3 title = %q, want %q", got, "real title")
	}
}
