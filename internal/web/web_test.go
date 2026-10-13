package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ericmason/aii/internal/store"
)

// Two agents can index sessions with the same id; the UI passes the
// clicked row's agent so the preview opens that session.
func TestAPISession_AgentParamPicksSharedID(t *testing.T) {
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
	for _, agent := range []string{"codex", "claude_code"} {
		w := httptest.NewRecorder()
		apiSession(db, w, httptest.NewRequest(http.MethodGet, "/api/session/"+uid+"?agent="+agent, nil))
		var got struct {
			Agent string `json:"agent"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Agent != agent {
			t.Fatalf("agent=%s: status %d, body %s", agent, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	apiSession(db, w, httptest.NewRequest(http.MethodGet, "/api/session/"+uid, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("no agent: status %d; want 409", w.Code)
	}
}
