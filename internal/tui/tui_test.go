package tui

import (
	"path/filepath"
	"testing"

	"github.com/ericmason/aii/internal/store"
)

// Two agents can index sessions with the same id; the preview must show
// the one the result row belongs to.
func TestLoadPreview_UsesResultAgentForSharedID(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	uid := "5e5e5e5e-1111-4222-8333-444455556666"
	for _, agent := range []string{"codex", "claude_code"} {
		if _, err := db.UpsertSession(&store.Session{Agent: agent, UID: uid, Title: agent, SourcePath: "/tmp/" + agent}); err != nil {
			t.Fatal(err)
		}
	}
	m := model{db: db}
	for _, agent := range []string{"codex", "claude_code"} {
		msg, _ := m.loadPreview(store.Result{Agent: agent, SessionUID: uid})().(previewMsg)
		if msg.session == nil || msg.session.Agent != agent {
			t.Fatalf("preview for %s row = %+v; want the %s session", agent, msg.session, agent)
		}
	}
}
