package cursor

import (
	"context"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// fixtureRoot builds a Cursor User directory whose global store holds
// composerData rows with empty values, the shape Cursor leaves behind
// for composers it never filled in.
func fixtureRoot(t *testing.T, emptyComposers int) string {
	t.Helper()
	root := t.TempDir()
	globalDir := filepath.Join(root, "globalStorage")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(globalDir, "state.vscdb")
	w, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Exec(`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i := range emptyComposers {
		key := "composerData:00000000-0000-0000-0000-00000000000" + string(rune('0'+i))
		if _, err := w.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, key, []byte{}); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runScan indexes the fixture and returns everything logged during the
// run.
func runScan(t *testing.T, root string, verbose bool) string {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var buf strings.Builder
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}()

	src := &Source{Root: root}
	if _, err := indexer.Run(context.Background(), db, []source.Source{src}, indexer.Options{Verbose: verbose}); err != nil {
		t.Fatalf("indexer.Run: %v", err)
	}
	return buf.String()
}

func TestEmptyComposersStaySilentByDefault(t *testing.T) {
	root := fixtureRoot(t, 5)

	out := runScan(t, root, false)
	if strings.Contains(out, "parse composerData") {
		t.Fatalf("default run logged a per-composer skip:\n%s", out)
	}
	if !strings.Contains(out, "cursor: composers seen=5 emitted=0 skipped=5") {
		t.Fatalf("summary line missing or miscounted:\n%s", out)
	}
}

func TestEmptyComposersAppearUnderVerbose(t *testing.T) {
	root := fixtureRoot(t, 5)

	out := runScan(t, root, true)
	if got := strings.Count(out, "parse composerData: empty value"); got != 5 {
		t.Fatalf("verbose run logged %d per-composer skips, want 5:\n%s", got, out)
	}
	if !strings.Contains(out, "cursor: composers seen=5 emitted=0 skipped=5") {
		t.Fatalf("summary line missing or miscounted:\n%s", out)
	}
}
