// Package source defines the parser interface every agent implements.
//
// A Source enumerates its session artifacts, compares each against any
// stored IndexState, and emits a Batch per artifact on the provided
// channel. The indexer applies each Batch atomically: upsert session,
// insert messages (dedup by (session_id, ordinal)), then save the new
// IndexState. Parsers own their read-side state lookups; they do not
// write sessions/messages directly.
package source

import (
	"context"

	"github.com/ericmason/aii/internal/store"
)

// Batch is one session's incremental delta since the last index run.
type Batch struct {
	Session  store.Session
	Messages []store.Message
	State    store.IndexState // persisted after Messages commit

	// Truncate, when true, tells the indexer to delete any existing
	// rows for State.SourcePath before inserting. Used when a JSONL
	// file rotated/shrank, or a Cursor composer's bubble list changed
	// in a way that invalidates ordinals.
	Truncate bool
}

type Source interface {
	Name() string
	Scan(ctx context.Context, db *store.DB, full bool, out chan<- Batch) error
}
