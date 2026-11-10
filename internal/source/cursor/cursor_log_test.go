package cursor

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// composerRow is one synthetic cursorDiskKV row. An empty Value is the
// shape Cursor leaves behind for a composer it never wrote to.
type composerRow struct {
	ID    string
	Value []byte
}

// emptyRows returns n composerData rows with empty values.
func emptyRows(n int) []composerRow {
	rows := make([]composerRow, 0, n)
	for i := range n {
		rows = append(rows, composerRow{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i)})
	}
	return rows
}

// fixtureRoot builds a Cursor User directory whose global store holds
// the given composerData rows.
func fixtureRoot(t *testing.T, rows []composerRow) string {
	t.Helper()
	root := t.TempDir()
	globalDir := filepath.Join(root, "globalStorage")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w := openWritable(t, filepath.Join(globalDir, "state.vscdb"))
	if _, err := w.Exec(`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := w.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`,
			"composerData:"+r.ID, r.Value); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// addWorkspaceDB writes root/workspaceStorage/<name>/state.vscdb. ddl
// runs against it when non-empty; an empty ddl leaves a database with
// no tables, which is what Cursor leaves for a workspace it never used.
func addWorkspaceDB(t *testing.T, root, name, ddl string) string {
	t.Helper()
	dir := filepath.Join(root, "workspaceStorage", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.vscdb")
	w := openWritable(t, path)
	// Force the file into existence even with no DDL to run.
	if _, err := w.Exec(`CREATE TABLE placeholder (x INTEGER); DROP TABLE placeholder`); err != nil {
		t.Fatal(err)
	}
	if ddl != "" {
		if _, err := w.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func openWritable(t *testing.T, path string) *sql.DB {
	t.Helper()
	w, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
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
	root := fixtureRoot(t, emptyRows(5))

	out := runScan(t, root, false)
	if strings.Contains(out, "parse composerData") {
		t.Fatalf("default run logged a per-composer skip:\n%s", out)
	}
	if !strings.Contains(out, "cursor: composers seen=5 emitted=0 skipped=5 parse_errors=0") {
		t.Fatalf("summary line missing or miscounted:\n%s", out)
	}
}

func TestEmptyComposersAppearUnderVerbose(t *testing.T) {
	root := fixtureRoot(t, emptyRows(5))

	out := runScan(t, root, true)
	if got := strings.Count(out, "parse composerData: empty value"); got != 5 {
		t.Fatalf("verbose run logged %d per-composer skips, want 5:\n%s", got, out)
	}
	if !strings.Contains(out, "cursor: composers seen=5 emitted=0 skipped=5 parse_errors=0") {
		t.Fatalf("summary line missing or miscounted:\n%s", out)
	}
}

// A row aii cannot decode is a real fault, not Cursor housekeeping, so
// it must survive the quieting that empty rows get.
func TestMalformedComposerIsReportedByDefault(t *testing.T) {
	const badID = "11111111-1111-1111-1111-111111111111"
	rows := append(emptyRows(3), composerRow{ID: badID, Value: []byte(`{"broken`)})
	root := fixtureRoot(t, rows)

	out := runScan(t, root, false)
	if !strings.Contains(out, badID) {
		t.Fatalf("default run hid the malformed composer %s:\n%s", badID, out)
	}
	if strings.Contains(out, "empty value") {
		t.Fatalf("default run logged an empty row alongside it:\n%s", out)
	}
	if !strings.Contains(out, "cursor: composers seen=4 emitted=0 skipped=4 parse_errors=1") {
		t.Fatalf("summary line missing or miscounted:\n%s", out)
	}
}

// A workspace database with no ItemTable is expected; one holding
// content aii cannot decode costs real composer->workspace mappings and
// must be reported.
func TestWorkspaceDBErrors(t *testing.T) {
	root := fixtureRoot(t, emptyRows(1))
	addWorkspaceDB(t, root, "aaaaempty", "")
	addWorkspaceDB(t, root, "bbbbbroken",
		`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB);`+
			`INSERT INTO ItemTable VALUES ('composer.composerData', '{"broken')`)

	out := runScan(t, root, false)
	if strings.Contains(out, "aaaaempty") {
		t.Fatalf("default run reported a workspace DB with no ItemTable:\n%s", out)
	}
	if !strings.Contains(out, "bbbbbroken") {
		t.Fatalf("default run hid a workspace DB it could not decode:\n%s", out)
	}

	verbose := runScan(t, root, true)
	if !strings.Contains(verbose, "aaaaempty") {
		t.Fatalf("verbose run did not mention the workspace DB with no ItemTable:\n%s", verbose)
	}
}

// A readable workspace database with an ItemTable but no composer row
// is neither an error nor a reportable skip.
func TestWorkspaceDBWithoutComposerRowIsSilent(t *testing.T) {
	root := fixtureRoot(t, emptyRows(1))
	addWorkspaceDB(t, root, "ccccplain", `CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`)

	for _, verbose := range []bool{false, true} {
		out := runScan(t, root, verbose)
		if strings.Contains(out, "ccccplain") {
			t.Fatalf("verbose=%v reported a healthy workspace DB:\n%s", verbose, out)
		}
	}
}
