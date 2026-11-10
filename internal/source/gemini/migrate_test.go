package gemini

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// collect returns the batches one Scan emits, without writing them.
func collect(t *testing.T, db *store.DB, src source.Source, full bool) []source.Batch {
	t.Helper()
	ch := make(chan source.Batch, 64)
	done := make(chan []source.Batch, 1)
	go func() {
		var out []source.Batch
		for b := range ch {
			out = append(out, b)
		}
		done <- out
	}()
	err := src.Scan(context.Background(), db, full, ch)
	close(ch)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return <-done
}

// replaySource writes a fixed list of batches, so a test can drop one of
// them and see what the rest leave behind.
type replaySource struct{ batches []source.Batch }

func (r *replaySource) Name() string { return agentName }

func (r *replaySource) Scan(ctx context.Context, db *store.DB, full bool, out chan<- source.Batch) error {
	for _, b := range r.batches {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- b:
		}
	}
	return nil
}

// When Gemini CLI rewrites a .json session as .jsonl, the batch that
// deletes the old path's rows has to name the .jsonl file as the row to
// mark for reindex. Marked on the old path, the mark is read by nobody:
// the old messages stay and win over the new ones under
// ON CONFLICT DO NOTHING.
func TestMigratedSessionMarksTheFileItReadsBack(t *testing.T) {
	uid := "22222222-3333-4444-8555-666666666666"
	gemHome := t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", gemHome)
	root := filepath.Join(gemHome, ".gemini", "tmp")
	writeFile(t, filepath.Join(gemHome, ".gemini", "projects.json"), `{"projects":{"/work/migrated":"migrated-app"}}`, 0)

	jsonPath := filepath.Join(root, "migrated-app", "chats", "session-migrated.json")
	writeFile(t, jsonPath, `{
  "sessionId": "`+uid+`",
  "projectHash": "migrated-app",
  "startTime": "2026-01-05T08:00:00.000Z",
  "lastUpdated": "2026-01-05T08:05:00.000Z",
  "messages": [
    {"id": "m1", "timestamp": "2026-01-05T08:00:01.000Z", "type": "user", "content": "old question"},
    {"id": "m2", "timestamp": "2026-01-05T08:00:02.000Z", "type": "gemini", "content": "old answer"}
  ]
}`, 1)

	db := openDB(t)
	runIndex(t, db, New(), false)
	if _, got := rows(t, db, uid); len(got) != 2 {
		t.Fatalf("first run stored %d messages, want 2", len(got))
	}

	// Gemini CLI replaces the .json file with a .jsonl one.
	if err := os.Remove(jsonPath); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(root, "migrated-app", "chats", "session-migrated.jsonl")
	writeFile(t, jsonlPath, `{"sessionId":"`+uid+`","projectHash":"migrated-app","startTime":"2026-01-05T08:00:00.000Z"}`+"\n"+
		`{"id":"n1","timestamp":"2026-01-05T09:00:01.000Z","type":"user","content":"new question"}`+"\n"+
		`{"id":"n2","timestamp":"2026-01-05T09:00:02.000Z","type":"gemini","content":"new answer"}`+"\n", 2)

	batches := collect(t, db, New(), false)
	var cross *source.Batch
	var rest []source.Batch
	for i, b := range batches {
		if b.Truncate && b.State.SourcePath == jsonPath {
			cross = &batches[i]
			continue
		}
		rest = append(rest, b)
	}
	if cross == nil {
		t.Fatalf("no batch deletes the old path's rows; got %d batches", len(batches))
	}
	if got := cross.MarkKey(); got != jsonlPath {
		t.Errorf("the delete for %s marks %q for reindex, want the file being read, %q", jsonPath, got, jsonlPath)
	}

	// Write every batch but that one, as a run that deferred it would.
	runIndex(t, db, &replaySource{batches: rest}, false)
	if _, got := rows(t, db, uid); len(got) != 2 || got[1].Content != "old answer" {
		t.Fatalf("stored %d messages after the deferred delete: %+v", len(got), got)
	}

	// The key that batch marks is one the next run reads back, so the
	// run after the mark replaces the stale messages.
	if err := db.MarkNeedsReindex(cross.MarkKey()); err != nil {
		t.Fatalf("MarkNeedsReindex: %v", err)
	}
	runIndex(t, db, New(), false)
	_, got := rows(t, db, uid)
	check(t, got, []want{{"user", "new question"}, {"assistant", "new answer"}})
}
