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

	// Usage holds token usage read from the same bytes. It may come
	// without a Session (Claude Code subagent transcripts add usage to
	// their parent session but no messages). Truncate never removes
	// stored usage, so usage recorded from a transcript that later
	// shrinks or disappears stays in the index.
	Usage []store.Usage

	// StaleSessionUID, when set, names a session of the same agent that
	// older index runs wrongly filed this file's messages under (a
	// forked Codex rollout stored under its parent's id). The indexer
	// deletes that session's row for State.SourcePath before upserting.
	StaleSessionUID string

	// Truncate, when true, tells the indexer to delete any existing
	// rows for State.SourcePath before inserting. Used when a JSONL
	// file rotated/shrank, or a Cursor composer's bubble list changed
	// in a way that invalidates ordinals.
	Truncate bool

	// ReindexKey names the index_state row the Source reads back when
	// it decides whether to scan this artifact again. The indexer marks
	// that row when it cannot write the batch, so the next run reads
	// the artifact from the start.
	//
	// Leave it empty when the Source looks up State.SourcePath itself,
	// which is the common case. Set it when a batch carries a different
	// key: Aider files one batch per session inside a history file but
	// looks up the file, and Gemini and Copilot emit a batch for a
	// session's previous path while looking up its current one. Marking
	// a key nothing reads back loses the batch for good.
	ReindexKey string
}

// MarkKey returns the index_state row to mark when this batch cannot be
// written: the one its Source reads back.
func (b Batch) MarkKey() string {
	if b.ReindexKey != "" {
		return b.ReindexKey
	}
	return b.State.SourcePath
}

type Source interface {
	Name() string
	Scan(ctx context.Context, db *store.DB, full bool, out chan<- Batch) error
}

// FullForFile reports whether one file must be read from the start even
// on an incremental run. That is either because the run is a full one,
// or because a previous run could not write this file's batch and
// marked its state: the stored mtime and size still match the file, so
// the usual "unchanged since last run" check would skip it and the
// messages would never be indexed.
//
// Every Source calls it right after reading the file's IndexState and
// uses the result in place of the full flag it was passed.
func FullForFile(full bool, prev *store.IndexState) bool {
	return full || (prev != nil && prev.NeedsReindex)
}
