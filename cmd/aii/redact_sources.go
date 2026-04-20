package main

// Destructively scrub secrets out of the original transcript files on
// disk. Called only when the user passes --redact-sources to `aii index`
// — the assumption is "they really mean it." We cover the JSONL sources
// (Claude Code + Codex); Cursor stores its conversations inside an
// SQLite DB that's often locked by a live Cursor process, so we skip it
// with a clear message instead of risking corruption.

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ericmason/aii/internal/redact"
	"github.com/ericmason/aii/internal/source"
)

// redactSourceFiles walks the transcript paths backing the given
// sources, rewrites any file that contains a recognized secret, and
// returns the number of files touched. It preserves mtime so future
// incremental scans don't needlessly re-read untouched files.
func redactSourceFiles(sources []source.Source, verbose bool) (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}

	var paths []string
	cursorNoted := false
	for _, s := range sources {
		switch s.Name() {
		case "claude_code":
			matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
			paths = append(paths, matches...)
		case "codex":
			paths = append(paths, collectCodexFiles(home)...)
		case "cursor":
			if !cursorNoted {
				fmt.Fprintln(os.Stderr, "redact-sources: cursor stores conversations in an SQLite DB that's usually locked by the Cursor app; skipping")
				cursorNoted = true
			}
		}
	}

	changed := 0
	for _, p := range paths {
		did, err := redactFileInPlace(p)
		if err != nil {
			log.Printf("redact %s: %v", p, err)
			continue
		}
		if did {
			changed++
			if verbose {
				log.Printf("redacted %s", p)
			}
		}
	}
	return changed, nil
}

// redactFileInPlace rewrites p with secrets replaced. Returns (true,
// nil) if the file's contents actually changed. Uses an atomic
// write-to-temp-then-rename so a crash mid-write doesn't leave a
// truncated transcript.
func redactFileInPlace(p string) (bool, error) {
	orig, err := os.ReadFile(p)
	if err != nil {
		return false, err
	}
	if !redact.HasSecret(string(orig)) {
		return false, nil
	}

	redacted := redact.RedactBytes(orig)
	if string(redacted) == string(orig) {
		return false, nil
	}

	info, err := os.Stat(p)
	if err != nil {
		return false, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), ".aii-redact-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	// Clean up if we bail out before rename.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(redacted); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return false, err
	}
	// Keep the original mtime so the incremental scanner doesn't churn
	// on every subsequent run — the content delta it cares about is the
	// set of new messages, not this in-place rewrite.
	_ = os.Chtimes(p, info.ModTime(), info.ModTime())
	return true, nil
}

// collectCodexFiles enumerates the JSONL files the codex source reads:
// every rollout file under ~/.codex/sessions plus the top-level
// history.jsonl. Both are JSONL and safe to rewrite line-by-line via
// the global regex pass.
func collectCodexFiles(home string) []string {
	var out []string
	root := filepath.Join(home, ".codex", "sessions")
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	history := filepath.Join(home, ".codex", "history.jsonl")
	if _, err := os.Stat(history); err == nil {
		out = append(out, history)
	}
	return out
}
