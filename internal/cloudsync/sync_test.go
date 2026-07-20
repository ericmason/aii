package cloudsync

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/cloudsync/remote"
	"github.com/ericmason/aii/internal/store"
)

// testRepo is a shared remote + key set — "the bucket".
type testRepo struct {
	dir    *remote.Dir
	keys   *Keys
	repoID string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	d, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	m, err := newRepoMarker(keys)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(m)
	if err := d.PutIfAbsent(context.Background(), markerKey, blob); err != nil {
		t.Fatal(err)
	}
	return &testRepo{dir: d, keys: keys, repoID: m.RepoID}
}

// machine is one synced host: its own DB plus an engine bound to the
// shared repo.
type machine struct {
	db  *store.DB
	eng *Engine
}

func newMachine(t *testing.T, repo *testRepo) *machine {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &machine{
		db: db,
		eng: &Engine{
			DB: db, Remote: repo.dir, Keys: repo.keys,
			Config: &Config{Version: 1, RemoteType: "dir", RepoID: repo.repoID, Fingerprint: repo.keys.Fingerprint},
			Log:    io.Discard,
		},
	}
}

// seed writes a session the way the indexer would.
func (m *machine) seed(t *testing.T, agent, uid, sourcePath string, contents ...string) int64 {
	t.Helper()
	id, err := m.db.UpsertSession(&store.Session{
		Agent: agent, UID: uid, Workspace: "/ws", Title: contents[0],
		StartedAt: 1700000000, EndedAt: 1700000000 + int64(len(contents)*60),
		SourcePath: sourcePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.appendMsgs(t, id, 0, contents...)
	return id
}

func (m *machine) appendMsgs(t *testing.T, sessionID int64, fromOrdinal int, contents ...string) {
	t.Helper()
	msgs := make([]store.Message, len(contents))
	for i, c := range contents {
		role := "user"
		if (fromOrdinal+i)%2 == 1 {
			role = "assistant"
		}
		msgs[i] = store.Message{Ordinal: fromOrdinal + i, Role: role, TS: 1700000000 + int64(fromOrdinal+i)*60, Content: c}
	}
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.db.InsertMessages(tx, sessionID, msgs); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (m *machine) contents(t *testing.T, agent, uid string) []string {
	t.Helper()
	s, err := m.db.SessionByUID(agent, uid)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return nil
	}
	rows, err := m.db.SessionMessages(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Content
	}
	return out
}

func (m *machine) ftsCount(t *testing.T, term string) int {
	t.Helper()
	var n int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, term).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSyncConvergence(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	a.seed(t, "claude_code", "uid-cc-1", "/a/cc.jsonl", "how to fix flumoxide bug", "patch the flumoxide handler")
	a.seed(t, "cursor", "uid-cur-1", "/a/state.vscdb", "cursor session about gizmos")

	ps, err := a.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Pushed != 2 {
		t.Fatalf("push stats = %+v, want 2 pushed", ps)
	}

	pl, err := b.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.New != 2 || pl.Messages != 3 {
		t.Fatalf("pull stats = %+v, want 2 new / 3 msgs", pl)
	}
	if got := b.contents(t, "claude_code", "uid-cc-1"); !eq(got, []string{"how to fix flumoxide bug", "patch the flumoxide handler"}) {
		t.Fatalf("B contents = %v", got)
	}
	// FTS triggers indexed the pulled messages.
	if n := b.ftsCount(t, "flumoxide"); n != 2 {
		t.Fatalf("FTS on B found %d rows for flumoxide, want 2", n)
	}
	// Pulled sessions carry the synthetic source path.
	s, _ := b.db.SessionByUID("claude_code", "uid-cc-1")
	if !store.IsSyntheticSourcePath(s.SourcePath) {
		t.Fatalf("pulled session source_path = %q", s.SourcePath)
	}

	// Steady state: everything reports up-to-date, nothing new moves.
	ps, _ = a.eng.Push(ctx, false)
	if ps.Pushed != 0 || ps.UpToDate != 2 {
		t.Fatalf("second push = %+v", ps)
	}
	ps, _ = b.eng.Push(ctx, false)
	if ps.Pushed != 0 {
		t.Fatalf("B push after pull = %+v, want nothing pushed", ps)
	}
	pl, _ = a.eng.Pull(ctx, false)
	if pl.New != 0 && pl.Extended != 0 {
		t.Fatalf("A pull = %+v, want no changes", pl)
	}
}

func TestAppendBothDirections(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0", "m1", "m2")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.eng.Pull(ctx, false); err != nil {
		t.Fatal(err)
	}

	// B extends the session (as if resumed there) and pushes.
	sb, _ := b.db.SessionByUID("claude_code", "u1")
	b.appendMsgs(t, sb.ID, 3, "m3", "m4")
	ps, err := b.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Pushed != 1 {
		t.Fatalf("B push = %+v", ps)
	}

	pl, err := a.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Extended != 1 || pl.Messages != 2 {
		t.Fatalf("A pull = %+v, want 1 extended / 2 msgs", pl)
	}
	if got := a.contents(t, "claude_code", "u1"); !eq(got, []string{"m0", "m1", "m2", "m3", "m4"}) {
		t.Fatalf("A contents = %v", got)
	}

	// And back: A appends, B pulls.
	sa, _ := a.db.SessionByUID("claude_code", "u1")
	a.appendMsgs(t, sa.ID, 5, "m5")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.eng.Pull(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := b.contents(t, "claude_code", "u1"); len(got) != 6 || got[5] != "m5" {
		t.Fatalf("B contents = %v", got)
	}
}

func TestTruncationEpochBumpSupersedes(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "old0", "old1", "old2", "old3")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.eng.Pull(ctx, false); err != nil {
		t.Fatal(err)
	}

	// A's transcript rotates: the indexer wipes and re-indexes the
	// session with renumbered, different content.
	if err := a.db.DeleteBySourcePath("/a/u1.jsonl"); err != nil {
		t.Fatal(err)
	}
	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "new0", "new1")

	// Push must detect the rewrite (cached chain no longer matches)
	// and bump the epoch rather than uploading an interleavable count.
	ps, err := a.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Pushed != 1 {
		t.Fatalf("A push after truncation = %+v", ps)
	}
	st, _ := a.db.GetSyncState("claude_code", "u1")
	if st.Epoch != 2 {
		t.Fatalf("epoch after truncation = %d, want 2", st.Epoch)
	}

	// B pulls: higher epoch, divergent → supersede, never interleave.
	pl, err := b.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Superseded != 1 {
		t.Fatalf("B pull = %+v, want 1 superseded", pl)
	}
	if got := b.contents(t, "claude_code", "u1"); !eq(got, []string{"new0", "new1"}) {
		t.Fatalf("B contents after supersede = %v", got)
	}
	if n := b.ftsCount(t, "old1"); n != 0 {
		t.Fatalf("stale FTS rows survived supersede: %d", n)
	}
}

func TestPrefixCompatibleHigherEpochMergesWithoutLoss(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	// B pulls while the session is one message long, then lags.
	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.eng.Pull(ctx, false); err != nil {
		t.Fatal(err)
	}
	sa, _ := a.db.SessionByUID("claude_code", "u1")
	a.appendMsgs(t, sa.ID, 1, "m1-old")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}

	// A's transcript rotates and re-indexes with a different tail —
	// an epoch bump — but B's short local copy is still a prefix of
	// the new history.
	if err := a.db.DeleteBySourcePath("/a/u1.jsonl"); err != nil {
		t.Fatal(err)
	}
	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0", "m1-new", "m2-new")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	st, _ := a.db.GetSyncState("claude_code", "u1")
	if st.Epoch != 2 {
		t.Fatalf("A epoch = %d, want 2", st.Epoch)
	}

	// B must adopt the epoch and merge the tail — not delete+rewrite.
	pl, err := b.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Extended != 1 || pl.Superseded != 0 {
		t.Fatalf("B pull = %+v, want extended not superseded", pl)
	}
	if got := b.contents(t, "claude_code", "u1"); !eq(got, []string{"m0", "m1-new", "m2-new"}) {
		t.Fatalf("B contents = %v", got)
	}
	stB, _ := b.db.GetSyncState("claude_code", "u1")
	if stB.Epoch != 2 {
		t.Fatalf("B did not adopt epoch: %d", stB.Epoch)
	}
}

func TestSameEpochDivergenceConverges(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0", "m1")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.eng.Pull(ctx, false); err != nil {
		t.Fatal(err)
	}

	// A extends normally; B's copy mutates in place (e.g. content
	// diverged via redaction differences).
	sa, _ := a.db.SessionByUID("claude_code", "u1")
	a.appendMsgs(t, sa.ID, 2, "m2-from-a")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}

	sb, _ := b.db.SessionByUID("claude_code", "u1")
	if _, err := b.db.Exec(`UPDATE messages SET content = 'REWRITTEN' WHERE session_id = ? AND ordinal = 1`, sb.ID); err != nil {
		t.Fatal(err)
	}

	// B pull: same-epoch conflict → keep local, no interleave.
	pl, err := b.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Conflicts != 1 {
		t.Fatalf("B pull = %+v, want 1 conflict", pl)
	}
	if got := b.contents(t, "claude_code", "u1"); !eq(got, []string{"m0", "REWRITTEN"}) {
		t.Fatalf("B contents after conflict pull = %v", got)
	}

	// B push: self-divergence bumps the epoch; A pull adopts B's
	// version wholesale. Last-diverger-wins, but never corrupted.
	if _, err := b.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	pl, err = a.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Superseded != 1 {
		t.Fatalf("A pull = %+v, want superseded", pl)
	}
	if !eq(a.contents(t, "claude_code", "u1"), b.contents(t, "claude_code", "u1")) {
		t.Fatal("machines did not converge")
	}
}

func TestPurge(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "sensitive stuff")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.eng.Pull(ctx, false); err != nil {
		t.Fatal(err)
	}

	if err := a.eng.Purge(ctx, "claude_code", "u1", true); err != nil {
		t.Fatal(err)
	}
	// A's local copy is gone; remote versions are gone.
	if got := a.contents(t, "claude_code", "u1"); got != nil {
		t.Fatalf("A still has purged session: %v", got)
	}
	name := repo.keys.BundleName("claude_code", "u1")
	objs, _ := repo.dir.List(ctx, "bundles/"+name+"/")
	for _, o := range objs {
		if _, ok := parseVersionKey(o.Key); ok {
			t.Fatalf("version object survived purge: %s", o.Key)
		}
	}

	// B keeps its local copy but must not re-push it.
	ps, err := b.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Excluded != 1 || ps.Pushed != 0 {
		t.Fatalf("B push after purge = %+v, want excluded", ps)
	}
	if b.contents(t, "claude_code", "u1") == nil {
		t.Fatal("B's local copy should survive a remote purge")
	}
	// B's own --local purge drops it.
	if err := b.eng.Purge(ctx, "claude_code", "u1", true); err != nil {
		t.Fatal(err)
	}
	if b.contents(t, "claude_code", "u1") != nil {
		t.Fatal("B still has session after purge --local")
	}
}

func TestForgedTombstoneIgnored(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a := newMachine(t, repo)

	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0")
	name := repo.keys.BundleName("claude_code", "u1")
	// An attacker with bucket write access plants a fake tombstone.
	if err := repo.dir.Put(ctx, tombstoneKey(name), []byte("nope")); err != nil {
		t.Fatal(err)
	}
	ps, err := a.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Pushed != 1 || ps.Excluded != 0 {
		t.Fatalf("push with forged tombstone = %+v, want 1 pushed", ps)
	}
}

func TestGCOnlyAfterSupersedingVersionListed(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a := newMachine(t, repo)

	id := a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0", "m1")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	a.appendMsgs(t, id, 2, "m2")
	// Second push happens AFTER this run's GC pass, so the old
	// version must survive this run...
	ps, err := a.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.GCed != 0 {
		t.Fatalf("GC fired in the same run as the superseding push: %+v", ps)
	}
	name := repo.keys.BundleName("claude_code", "u1")
	objs, _ := repo.dir.List(ctx, "bundles/"+name+"/")
	if countVersions(objs) != 2 {
		t.Fatalf("expected both versions on remote, got %d", countVersions(objs))
	}
	// ...and be collected on the next run, whose listing proves the
	// superseding version is durably visible.
	ps, err = a.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.GCed != 1 {
		t.Fatalf("next-run GC = %+v, want 1 collected", ps)
	}
	objs, _ = repo.dir.List(ctx, "bundles/"+name+"/")
	if countVersions(objs) != 1 {
		t.Fatalf("expected 1 version after GC, got %d", countVersions(objs))
	}
}

func countVersions(objs []remote.Object) int {
	n := 0
	for _, o := range objs {
		if _, ok := parseVersionKey(o.Key); ok {
			n++
		}
	}
	return n
}

func TestCodexHistoryExcluded(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a := newMachine(t, repo)

	home, _ := os.UserHomeDir()
	a.seed(t, "codex", "hist-1", filepath.Join(home, ".codex", "history.jsonl"), "a stray prompt")
	a.seed(t, "codex", "roll-1", filepath.Join(home, ".codex", "sessions", "roll-1.jsonl"), "a real rollout")

	ps, err := a.eng.Push(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Pushed != 1 {
		t.Fatalf("push = %+v, want only the rollout session pushed", ps)
	}
	if _, err := repo.dir.Get(ctx, tombstoneKey(repo.keys.BundleName("codex", "hist-1"))); err == nil {
		t.Fatal("history session should simply be skipped, not tombstoned")
	}
	objs, _ := repo.dir.List(ctx, "bundles/"+repo.keys.BundleName("codex", "hist-1")+"/")
	if len(objs) != 0 {
		t.Fatalf("history session was pushed: %v", objs)
	}
}

func TestPullRejectsTamperedBundle(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	// Flip one byte of the only version object.
	name := repo.keys.BundleName("claude_code", "u1")
	objs, _ := repo.dir.List(ctx, "bundles/"+name+"/")
	if len(objs) != 1 {
		t.Fatal("setup: expected one object")
	}
	data, _ := repo.dir.Get(ctx, objs[0].Key)
	data[len(data)/2] ^= 1
	if err := repo.dir.Put(ctx, objs[0].Key, data); err != nil {
		t.Fatal(err)
	}

	pl, err := b.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Rejected != 1 || pl.New != 0 {
		t.Fatalf("pull of tampered bundle = %+v, want 1 rejected / 0 new", pl)
	}
	if b.contents(t, "claude_code", "u1") != nil {
		t.Fatal("tampered bundle was imported")
	}
}

func TestConflictedCopyFilenamesIgnored(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	a, b := newMachine(t, repo), newMachine(t, repo)

	a.seed(t, "claude_code", "u1", "/a/u1.jsonl", "m0")
	if _, err := a.eng.Push(ctx, false); err != nil {
		t.Fatal(err)
	}
	// Dropbox-style droppings inside the repo.
	name := repo.keys.BundleName("claude_code", "u1")
	junk := []string{
		"bundles/" + name + "/1-1-cafebabe (Eric's conflicted copy 2026-07-19).age",
		"bundles/stray-upload.txt",
	}
	for _, k := range junk {
		if err := repo.dir.Put(ctx, k, []byte("junk")); err != nil {
			t.Fatal(err)
		}
	}
	pl, err := b.eng.Pull(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl.New != 1 || pl.Rejected != 0 {
		t.Fatalf("pull with junk files = %+v, want clean 1 new", pl)
	}
}

func TestRepoBindingRefusals(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	t.Run("wrong-keys", func(t *testing.T) {
		m := newMachine(t, repo)
		other, _ := GenerateKeys()
		m.eng.Keys = other
		m.eng.Config.Fingerprint = other.Fingerprint
		if _, err := m.eng.Push(ctx, false); err == nil || !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("push with wrong keys = %v, want marker auth failure", err)
		}
	})

	t.Run("wrong-config-pin", func(t *testing.T) {
		m := newMachine(t, repo)
		m.eng.Config.RepoID = "0000000000000000"
		if _, err := m.eng.Push(ctx, false); err == nil || !strings.Contains(err.Error(), "does not match the pinned config") {
			t.Fatalf("push with wrong config pin = %v", err)
		}
	})

	t.Run("db-bound-elsewhere", func(t *testing.T) {
		m := newMachine(t, repo)
		if err := m.db.SyncMetaSet("repo_id", "ffff000011112222"); err != nil {
			t.Fatal(err)
		}
		if _, err := m.eng.Push(ctx, false); err == nil || !strings.Contains(err.Error(), "bound to a different sync repo") {
			t.Fatalf("push with foreign DB binding = %v", err)
		}
	})
}

func TestInitCreateAndJoin(t *testing.T) {
	if testing.Short() {
		t.Skip("scrypt work factor makes this slow")
	}
	ctx := context.Background()
	remoteRoot := t.TempDir()
	pass := func(string) func(bool) (string, error) {
		return nil
	}
	_ = pass

	mkDB := func(t *testing.T) (*store.DB, string) {
		dataDir := t.TempDir()
		db, err := store.Open(filepath.Join(dataDir, "aii.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db, dataDir
	}
	fixedPass := func(p string) func(bool) (string, error) {
		return func(bool) (string, error) { return p, nil }
	}

	dbA, dirA := mkDB(t)
	cfgA := &Config{RemoteType: "dir", Path: remoteRoot}
	if err := Init(ctx, dbA, dirA, cfgA, fixedPass("correct horse battery staple"), false); err != nil {
		t.Fatal(err)
	}
	if cfgA.RepoID == "" || cfgA.Fingerprint == "" {
		t.Fatal("init did not pin repo id / fingerprint")
	}

	// Second machine joins with the right passphrase.
	dbB, dirB := mkDB(t)
	cfgB := &Config{RemoteType: "dir", Path: remoteRoot}
	if err := Init(ctx, dbB, dirB, cfgB, fixedPass("correct horse battery staple"), false); err != nil {
		t.Fatal(err)
	}
	if cfgB.RepoID != cfgA.RepoID || cfgB.Fingerprint != cfgA.Fingerprint {
		t.Fatal("join derived a different repo identity")
	}
	// Engines from both dirs can verify the repo.
	engB, err := LoadEngine(dirB, dbB)
	if err != nil {
		t.Fatal(err)
	}
	engB.Log = io.Discard
	if err := engB.VerifyRepo(ctx); err != nil {
		t.Fatal(err)
	}

	// Wrong passphrase fails.
	dbC, dirC := mkDB(t)
	if err := Init(ctx, dbC, dirC, &Config{RemoteType: "dir", Path: remoteRoot}, fixedPass("totally wrong passphrase"), false); err == nil {
		t.Fatal("join with wrong passphrase succeeded")
	}

	// Re-init on a configured machine refuses without --rebind.
	if err := Init(ctx, dbA, dirA, &Config{RemoteType: "dir", Path: remoteRoot}, fixedPass("correct horse battery staple"), false); err == nil {
		t.Fatal("re-init did not refuse")
	}
}
