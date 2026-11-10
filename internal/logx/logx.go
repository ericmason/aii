// Package logx holds the process-wide verbosity switch that source
// parsers consult before logging per-item detail.
//
// Parsers run as goroutines under the indexer and log through the
// standard library's package-level logger, so they have no option
// struct to read. A scan over thousands of items must not write a line
// per item on every scheduled run: that is what grew `cron.log` without
// bound. Expected per-item outcomes (a skipped record, an unreadable
// store a parser already accounts for in its summary line) go through
// Verbosef, which prints only under `aii index --verbose`. Real errors
// and per-source summary lines keep using log.Printf.
package logx

import (
	"log"
	"sync/atomic"
)

var verbose atomic.Bool

// SetVerbose turns per-item logging on or off. The indexer calls it
// once per run, before any parser starts.
func SetVerbose(v bool) { verbose.Store(v) }

// Verbose reports whether per-item logging is on.
func Verbose() bool { return verbose.Load() }

// Verbosef logs like log.Printf, but only when verbose logging is on.
func Verbosef(format string, args ...any) {
	if verbose.Load() {
		log.Printf(format, args...)
	}
}
