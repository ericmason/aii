package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// errSource fails its scan, as a parser does on a corrupt directory.
type errSource struct{ name string }

func (e *errSource) Name() string { return e.name }

func (e *errSource) Scan(context.Context, *store.DB, bool, chan<- source.Batch) error {
	return errors.New("scan failed")
}

// Scans run concurrently with the writer loop, and both count errors, so
// the two counts must not share an unsynchronized field. Run this with
// -race: before the count went through an atomic, the scan goroutines
// and the writer loop wrote Stats.Errors at the same time.
func TestRun_CountsScanAndWriteErrorsTogether(t *testing.T) {
	shortTimeouts(t)
	db := openTestStore(t)
	// Every batch now fails to apply, so the writer loop counts errors
	// of its own while the scans count theirs.
	if _, err := db.Exec(`DROP TABLE token_usage`); err != nil {
		t.Fatalf("drop token_usage: %v", err)
	}

	const pairs = 8
	var sources []source.Source
	for i := 0; i < pairs; i++ {
		sources = append(sources, &errSource{name: fmt.Sprintf("broken-%d", i)})
		sources = append(sources, &fakeSource{
			name:    fmt.Sprintf("writer-%d", i),
			batches: []source.Batch{batchFor(fmt.Sprintf("/tmp/s%d.jsonl", i), fmt.Sprintf("uid-%d", i), "hello")},
		})
	}

	stats, err := Run(context.Background(), db, sources, Options{Full: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := 2 * pairs; stats.Errors != want {
		t.Errorf("Errors = %d, want %d (%d failed scans and %d unwritable batches)", stats.Errors, want, pairs, pairs)
	}
	if stats.SessionsTouch != 0 {
		t.Errorf("SessionsTouch = %d, want 0", stats.SessionsTouch)
	}
}
