// Package indexer orchestrates Source parsers into a single SQLite writer.
package indexer

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

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
	Sources        int
	SessionsTouch  int
	MessagesAdded  int
	Errors         int
	Duration       time.Duration
}

// Run launches each Source in its own goroutine, funnels batches into
// the single SQLite writer, and returns aggregate stats.
func Run(ctx context.Context, db *store.DB, sources []source.Source, opt Options) (Stats, error) {
	start := time.Now()
	stats := Stats{Sources: len(sources)}

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

	for b := range ch {
		if opt.Redact {
			redactBatch(&b)
		}
		if err := apply(db, b, opt.Verbose); err != nil {
			log.Printf("apply batch for %s: %v", b.State.SourcePath, err)
			stats.Errors++
			continue
		}
		stats.SessionsTouch++
		stats.MessagesAdded += len(b.Messages)
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

func apply(db *store.DB, b source.Batch, verbose bool) error {
	if b.Truncate {
		if err := db.DeleteBySourcePath(b.State.SourcePath); err != nil {
			return fmt.Errorf("delete truncated: %w", err)
		}
	}

	// Some batches carry only a state update (e.g. codex history scan
	// advanced the file offset but produced no new sessions).
	if b.Session.UID == "" && len(b.Messages) == 0 {
		return db.SaveState(b.State)
	}

	id, err := db.UpsertSession(&b.Session)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := db.InsertMessages(tx, id, b.Messages); err != nil {
		tx.Rollback()
		return fmt.Errorf("insert messages: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := db.SaveState(b.State); err != nil {
		return fmt.Errorf("save state: %w", err)
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
