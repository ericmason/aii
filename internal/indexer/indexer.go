// Package indexer orchestrates Source parsers into a single SQLite writer.
package indexer

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ericmason/aii/internal/logx"
	"github.com/ericmason/aii/internal/redact"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

type Options struct {
	Full    bool
	Verbose bool
	// Redact controls whether message content is scrubbed of known
	// secret shapes on its way into the DB. Defaults to true; callers
	// opt out explicitly with --no-redact.
	Redact bool
}

type Stats struct {
	Sources       int
	SessionsTouch int
	MessagesAdded int
	Errors        int
	// Deferred counts batches that never reached the database because
	// another process held the write lock. Their files are marked for
	// reindex, so a later run picks them up.
	Deferred int
	Duration time.Duration
}

// applyAttempts is how many times a batch is tried before it is
// deferred, and applyBackoff is the wait before each retry. The store's
// own busy_timeout already waits inside each attempt; these retries
// cover a lock held for longer than that. Tests shorten them.
var (
	applyAttempts = 3
	applyBackoff  = []time.Duration{200 * time.Millisecond, time.Second}
)

// markAttempts and markBackoff govern the reindex marks written after
// the run, when the writer that was in the way has usually finished.
var (
	markAttempts = 5
	markBackoff  = []time.Duration{200 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
)

// Run launches each Source in its own goroutine, funnels batches into
// the single SQLite writer, and returns aggregate stats.
func Run(ctx context.Context, db *store.DB, sources []source.Source, opt Options) (Stats, error) {
	start := time.Now()
	stats := Stats{Sources: len(sources)}

	// Parsers log per-item detail through logx, which is silent unless
	// this run asked for it.
	logx.SetVerbose(opt.Verbose)

	ch := make(chan source.Batch, 32)
	var wg sync.WaitGroup

	for _, s := range sources {
		wg.Add(1)
		go func(s source.Source) {
			defer wg.Done()
			if opt.Verbose {
				log.Printf("[%s] scanning", s.Name())
			}
			if err := s.Scan(ctx, db, opt.Full, ch); err != nil {
				log.Printf("[%s] scan error: %v", s.Name(), err)
				stats.Errors++
			}
			if opt.Verbose {
				log.Printf("[%s] scan complete", s.Name())
			}
		}(s)
	}

	go func() { wg.Wait(); close(ch) }()

	var deferred []string
	for b := range ch {
		if opt.Redact {
			redactBatch(&b)
		}
		err := applyWithRetry(db, b, opt.Verbose)
		if err == nil {
			stats.SessionsTouch++
			stats.MessagesAdded += len(b.Messages)
			continue
		}
		stats.Errors++
		if !store.IsBusy(err) {
			log.Printf("apply batch for %s: %v", b.State.SourcePath, err)
			continue
		}
		// Nothing of this batch reached the database, and its stored
		// state still points at what an earlier run wrote. Mark the
		// file so the next run reparses it rather than seeing an
		// unchanged mtime and skipping it.
		log.Printf("apply batch for %s: %v — marking the file for reindex", b.State.SourcePath, err)
		stats.Deferred++
		deferred = append(deferred, b.State.SourcePath)
	}

	markErr := markForReindex(db, deferred)
	stats.Duration = time.Since(start)
	if markErr != nil {
		return stats, markErr
	}
	if stats.Deferred > 0 {
		return stats, fmt.Errorf("%d batch%s could not be written while another process held the database write lock; marked for reindex on the next run",
			stats.Deferred, plural(stats.Deferred))
	}
	return stats, nil
}

// applyWithRetry writes one batch, retrying while the write lock is
// held elsewhere. Each attempt already waits out the store's
// busy_timeout, so the retries cover a lock held for longer than that.
func applyWithRetry(db *store.DB, b source.Batch, verbose bool) error {
	var err error
	for attempt := 0; attempt < applyAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff(applyBackoff, attempt-1))
		}
		err = apply(db, b, verbose)
		if err == nil || !store.IsBusy(err) {
			return err
		}
	}
	return err
}

// markForReindex flags every file whose batch was dropped. It runs
// after the scan, by when the writer that was in the way has usually
// finished, and retries for as long as it has not.
func markForReindex(db *store.DB, paths []string) error {
	var failed []string
	for _, path := range dedupe(paths) {
		var err error
		for attempt := 0; attempt < markAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(backoff(markBackoff, attempt-1))
			}
			if err = db.MarkNeedsReindex(path); err == nil || !store.IsBusy(err) {
				break
			}
		}
		if err != nil {
			log.Printf("mark %s for reindex: %v", path, err)
			failed = append(failed, path)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d dropped batch%s could not be marked for reindex, so `aii index` will skip %s until you run `aii index --full`: %s",
		len(failed), plural(len(failed)), pronoun(len(failed)), strings.Join(failed, ", "))
}

// backoff returns the wait before retry n, holding the last entry once
// the schedule runs out.
func backoff(schedule []time.Duration, n int) time.Duration {
	if len(schedule) == 0 {
		return 0
	}
	if n >= len(schedule) {
		n = len(schedule) - 1
	}
	return schedule[n]
}

func dedupe(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	var out []string
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func pronoun(n int) string {
	if n == 1 {
		return "that file"
	}
	return "those files"
}

// apply writes one batch in a single transaction, so a batch that
// cannot be written leaves nothing behind: no half-deleted sessions,
// and no index state claiming the file was read.
func apply(db *store.DB, b source.Batch, verbose bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if b.Truncate {
		if err := db.DeleteBySourcePathTx(tx, b.State.SourcePath); err != nil {
			return fmt.Errorf("delete truncated: %w", err)
		}
	}

	// Some batches carry only a state update (e.g. codex history scan
	// advanced the file offset but produced no new sessions), possibly
	// with usage for a session that another file holds.
	if b.Session.UID == "" && len(b.Messages) == 0 {
		if err := db.InsertUsage(tx, b.Usage); err != nil {
			return fmt.Errorf("insert usage: %w", err)
		}
		if err := db.SaveStateTx(tx, b.State); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
		return tx.Commit()
	}

	if b.StaleSessionUID != "" && b.StaleSessionUID != b.Session.UID {
		if err := db.DeleteSessionAtPathTx(tx, b.Session.Agent, b.StaleSessionUID, b.State.SourcePath); err != nil {
			return fmt.Errorf("delete stale session: %w", err)
		}
	}

	id, err := db.UpsertSessionTx(tx, &b.Session)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	if err := db.InsertMessages(tx, id, b.Messages); err != nil {
		return fmt.Errorf("insert messages: %w", err)
	}
	if err := db.InsertUsage(tx, b.Usage); err != nil {
		return fmt.Errorf("insert usage: %w", err)
	}
	if err := db.SaveStateTx(tx, b.State); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if verbose {
		log.Printf("[%s] %s: +%d msgs (workspace=%s)",
			b.Session.Agent, shortUID(b.Session.UID), len(b.Messages), b.Session.Workspace)
	}
	return nil
}

// redactBatch scrubs secrets out of every user-visible text field before
// it hits the DB. Session metadata (title/summary) is covered too, since
// a pasted secret can end up there via the auto-derived title.
func redactBatch(b *source.Batch) {
	b.Session.Title = redact.Redact(b.Session.Title)
	b.Session.Summary = redact.Redact(b.Session.Summary)
	for i := range b.Messages {
		b.Messages[i].Content = redact.Redact(b.Messages[i].Content)
	}
}

func shortUID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
