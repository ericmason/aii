package claudecode

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func userLine(uid, text string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/w","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`, uid, text) + "\n"
}

func TestParseDeltaTitleOnlyFromFullRead(t *testing.T) {
	first := userLine("s1", "real title")
	later := userLine("s1", "a later turn")

	full := first + later
	sess, _, _, err := parseDelta(context.Background(), strings.NewReader(full), 0, int64(len(full)))
	if err != nil {
		t.Fatal(err)
	}
	if sess.Title != "real title" {
		t.Fatalf("full read title = %q, want %q", sess.Title, "real title")
	}

	// Incremental run: only the appended line is parsed.
	start := int64(len(first))
	sess, msgs, _, err := parseDelta(context.Background(), strings.NewReader(later), start, int64(len(full)))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if sess.Title != "" {
		t.Fatalf("incremental read title = %q, want empty so the stored title is kept", sess.Title)
	}
}

func TestParseDeltaSkipsOversizedLine(t *testing.T) {
	before := userLine("s1", "before")
	huge := userLine("s1", strings.Repeat("x", scannerMax+1024))
	after := userLine("s1", "after")
	data := before + huge + after

	sess, msgs, off, err := parseDelta(context.Background(), strings.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len(data)) {
		t.Fatalf("offset = %d, want %d (past the oversized line)", off, len(data))
	}
	if len(msgs) != 2 || msgs[0].Content != "before" || msgs[1].Content != "after" {
		var got []string
		for _, m := range msgs {
			got = append(got, truncateRunes(m.Content, 20))
		}
		t.Fatalf("messages = %q, want [before after]", got)
	}
	if msgs[1].SourceOffset != int64(len(before)+len(huge)) {
		t.Fatalf("after SourceOffset = %d, want %d", msgs[1].SourceOffset, len(before)+len(huge))
	}
	if sess.UID != "s1" {
		t.Fatalf("UID = %q", sess.UID)
	}
}

func TestParseDeltaOversizedUnterminatedTailIsRetried(t *testing.T) {
	before := userLine("s1", "before")
	partial := `{"type":"user","message":"` + strings.Repeat("x", scannerMax+1024)
	data := before + partial

	_, msgs, off, err := parseDelta(context.Background(), strings.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if off != int64(len(before)) {
		t.Fatalf("offset = %d, want %d (start of the unfinished line)", off, len(before))
	}
}
