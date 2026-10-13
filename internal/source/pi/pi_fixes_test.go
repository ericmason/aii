package pi

import (
	"context"
	"strings"
	"testing"
)

// A later turn appended to the file must not replace the title that
// the first prompt or a /name entry set.
func TestAppendKeepsTitle(t *testing.T) {
	root, path := setup(t)
	db := openDB(t)
	src := &Source{Root: root}
	laterUser := strings.Replace(strings.Replace(piUser, `"a1"`, `"b1"`, 1), "Rename the widget module", "Now add tests", 1)

	writeFile(t, path, piHeader+piUser+piAsst, 1)
	runIndex(t, db, src, false)
	writeFile(t, path, piHeader+piUser+piAsst+laterUser, 2)
	runIndex(t, db, src, false)
	if s, _ := messages(t, db, uid); s.Title != "Rename the widget module" {
		t.Fatalf("title after appended turn = %q, want first prompt", s.Title)
	}

	writeFile(t, path, piHeader+piUser+piAsst+laterUser+piInfo, 3)
	runIndex(t, db, src, false)
	writeFile(t, path, piHeader+piUser+piAsst+laterUser+piInfo+laterUser, 4)
	runIndex(t, db, src, false)
	if s, _ := messages(t, db, uid); s.Title != "Widget rename" {
		t.Fatalf("title after /name and another turn = %q, want %q", s.Title, "Widget rename")
	}
}

// A first pass that saw no user message still titles the session from
// the first prompt on a later pass.
func TestDeltaTitlesUntitledSession(t *testing.T) {
	root, path := setup(t)
	db := openDB(t)
	src := &Source{Root: root}
	writeFile(t, path, piHeader+piSystem, 1)
	runIndex(t, db, src, false)
	writeFile(t, path, piHeader+piSystem+piUser, 2)
	runIndex(t, db, src, false)
	if s, _ := messages(t, db, uid); s.Title != "Rename the widget module" {
		t.Fatalf("title = %q", s.Title)
	}
}

// A line longer than the read limit is skipped, and the lines after it
// are still indexed instead of stalling at that offset.
func TestOversizedLineSkipped(t *testing.T) {
	big := `{"type":"message","id":"zz","message":{"role":"user","content":"` + strings.Repeat("x", 1000) + `"}}` + "\n"
	in := piUser + big + piAsst
	_, msgs, off, _ := parseDeltaMax(context.Background(), strings.NewReader(in), 0, int64(len(in)), 500)
	if off != int64(len(in)) {
		t.Fatalf("offset = %d, want %d", off, len(in))
	}
	if len(msgs) != 4 || msgs[0].Role != "user" || msgs[1].Role != "thinking" {
		t.Fatalf("msgs = %+v", msgs)
	}

	// An oversized line still being written is left for the next run.
	in2 := piUser + strings.TrimSuffix(big, "\n")
	_, _, off, _ = parseDeltaMax(context.Background(), strings.NewReader(in2), 0, int64(len(in2)), 500)
	if off != int64(len(piUser)) {
		t.Fatalf("partial oversized offset = %d, want %d", off, len(piUser))
	}
}

// Two UUIDv7 ids minted in the same minute share their first 8 hex
// digits; the stored uids must not, and the full pi id still resolves.
func TestShortIDsDistinct(t *testing.T) {
	a := "019a0000-1111-7000-8000-aaaaaaaaaaaa"
	b := "019a0000-2222-7000-8000-bbbbbbbbbbbb"
	if SessionUID(a)[:8] == SessionUID(b)[:8] {
		t.Fatalf("short ids collide: %s %s", SessionUID(a), SessionUID(b))
	}
	for _, id := range []string{"not-a-uuid", "3f2b8c1e-1111-4000-8000-aaaaaaaaaaaa"} {
		if got := SessionUID(id); got != id {
			t.Fatalf("SessionUID(%q) = %q, want unchanged", id, got)
		}
	}

	root, path := setup(t)
	writeFile(t, path, piHeader+piUser, 1)
	db := openDB(t)
	runIndex(t, db, &Source{Root: root}, false)
	for _, ref := range []string{uid, SessionUID(uid), SessionUID(uid)[:8]} {
		s, err := LookupSession(db, ref)
		if err != nil || s == nil {
			t.Fatalf("LookupSession(%q) = %v, %v", ref, s, err)
		}
	}
	if s, _ := db.SessionByUIDAny("0190aaaa"); s != nil {
		t.Fatalf("timestamp prefix resolved to %s; the short id should be the random part", s.UID)
	}
}

// A last line without a newline is never consumed, even when it parses:
// pi may still be writing it.
func TestUnterminatedLastLineNotConsumed(t *testing.T) {
	in := piUser + strings.TrimSuffix(piAsst, "\n")
	_, msgs, off, _ := parseDelta(context.Background(), strings.NewReader(in), 0, int64(len(in)))
	if off != int64(len(piUser)) || len(msgs) != 1 {
		t.Fatalf("offset = %d (want %d), msgs = %d (want 1)", off, len(piUser), len(msgs))
	}
}

// Bytes appended after the file was stat'd are left for the next run, so
// the offset and the emitted rows stay in step.
func TestReadBoundedToStatSize(t *testing.T) {
	in := piUser + piAsst
	_, msgs, off, _ := parseDelta(context.Background(), strings.NewReader(in), 0, int64(len(piUser)))
	if off != int64(len(piUser)) || len(msgs) != 1 {
		t.Fatalf("offset = %d (want %d), msgs = %d (want 1)", off, len(piUser), len(msgs))
	}

	// End to end: the next run picks up the rest exactly once.
	root, path := setup(t)
	db := openDB(t)
	src := &Source{Root: root}
	writeFile(t, path, piHeader+piUser+strings.TrimSuffix(piAsst, "\n"), 1)
	runIndex(t, db, src, false)
	writeFile(t, path, piHeader+piUser+piAsst, 2)
	runIndex(t, db, src, false)
	runIndex(t, db, src, false)
	if _, rows := messages(t, db, uid); len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
}
