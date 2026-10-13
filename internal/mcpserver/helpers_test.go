package mcpserver

import (
	"path/filepath"
	"testing"

	"github.com/ericmason/aii/internal/source/pi"
	"github.com/ericmason/aii/internal/store"
)

func TestRefAgent(t *testing.T) {
	cases := map[string]string{
		"cdx/019a1b2c:3":   "codex",
		" cc/32e869ac ":    "claude_code",
		"cur/abcdef12":     "cursor",
		"codex/019a1b2c-d": "codex",
		"019a1b2c":         "",
		"/019a1b2c":        "",
	}
	for ref, want := range cases {
		if got := RefAgent(ref); got != want {
			t.Errorf("RefAgent(%q) = %q; want %q", ref, got, want)
		}
	}
}

// A cite token's agent code picks between two agents' sessions that
// share a full id; without it the lookup is ambiguous.
func TestCiteTokenAgentBreaksSharedIDTie(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	uid := "5e5e5e5e-1111-4222-8333-444455556666"
	for _, agent := range []string{"codex", "claude_code"} {
		if _, err := db.UpsertSession(&store.Session{Agent: agent, UID: uid, SourcePath: "/tmp/" + agent}); err != nil {
			t.Fatal(err)
		}
	}
	for ref, want := range map[string]string{
		"cdx/" + ShortUID(db, uid) + ":4": "codex",
		"cc/" + ShortUID(db, uid) + ":4":  "claude_code",
	} {
		id, _ := parseSessionRef(ref)
		s, err := pi.LookupSessionForAgent(db, RefAgent(ref), id)
		if err != nil || s == nil || s.Agent != want {
			t.Fatalf("lookup %q = %v, %v; want the %s session", ref, s, err, want)
		}
	}
	if s, err := pi.LookupSessionForAgent(db, "", uid); err == nil {
		t.Fatalf("bare shared id resolved to %v; want an ambiguity error", s)
	}
}
