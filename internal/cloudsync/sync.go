package cloudsync

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ericmason/aii/internal/cloudsync/remote"
	"github.com/ericmason/aii/internal/redact"
	"github.com/ericmason/aii/internal/store"
)

const (
	markerKey    = "aii-sync.json"
	masterKeyKey = "keys/master.age"

	metaRepoID = "repo_id" // sync_meta key binding this DB to a repo

	// applyChunk bounds how many staged bundles are held in memory
	// and how long the index lock is held per apply pass.
	applyChunk = 32

	// maxObjectSize mirrors the remote-layer cap: bundles larger
	// than this are skipped from listing metadata, before download.
	maxObjectSize = 1 << 30
)

// Engine runs sync operations against one (DB, remote, keys) triple.
type Engine struct {
	DB     *store.DB
	Remote remote.Remote
	Keys   *Keys
	Config *Config

	// LockForApply acquires the single-writer index lock around
	// local SQLite write phases. nil means no locking (tests, or a
	// caller that already holds it). Network I/O NEVER runs under
	// this lock.
	LockForApply func() (release func(), err error)

	// Recheck disables the cached-chain fast path so every session's
	// chain is recomputed from DB content (`push --recheck`).
	Recheck bool

	Verbose bool
	Log     io.Writer // warnings + verbose output; nil = os.Stderr
}

func (e *Engine) logw() io.Writer {
	if e.Log != nil {
		return e.Log
	}
	return os.Stderr
}

func (e *Engine) warnf(format string, args ...any) {
	fmt.Fprintf(e.logw(), "aii sync: "+format+"\n", args...)
}

func (e *Engine) verbosef(format string, args ...any) {
	if e.Verbose {
		e.warnf(format, args...)
	}
}

func (e *Engine) lock() (func(), error) {
	if e.LockForApply == nil {
		return func() {}, nil
	}
	return e.LockForApply()
}

// LoadEngine assembles an Engine from the on-disk config and keyfile,
// verifying the pinned master-key fingerprint.
func LoadEngine(dataDir string, db *store.DB) (*Engine, error) {
	cfg, err := LoadConfig(dataDir)
	if err != nil {
		return nil, err
	}
	keys, err := LoadKeyFile(KeyPath(dataDir))
	if err != nil {
		return nil, fmt.Errorf("load sync key: %w", err)
	}
	if cfg.Fingerprint != "" && cfg.Fingerprint != keys.Fingerprint {
		return nil, errors.New("sync key fingerprint does not match the pinned config — the key file was replaced; if intentional, re-run `aii sync init --rebind`")
	}
	rem, err := cfg.OpenRemote()
	if err != nil {
		return nil, err
	}
	return &Engine{DB: db, Remote: rem, Keys: keys, Config: cfg}, nil
}

// --- repo marker --------------------------------------------------------

type repoMarker struct {
	Version int    `json:"version"`
	RepoID  string `json:"repo_id"`
	Created string `json:"created"`
	MAC     string `json:"mac"`
}

func markerMAC(keys *Keys, repoID, created string) string {
	m := hmac.New(sha256.New, keys.RepoKey)
	m.Write([]byte("repo/v1"))
	m.Write([]byte{0})
	m.Write([]byte(repoID))
	m.Write([]byte{0})
	m.Write([]byte(created))
	return hex.EncodeToString(m.Sum(nil))
}

func newRepoMarker(keys *Keys) (*repoMarker, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(raw[:])
	created := time.Now().UTC().Format(time.RFC3339)
	return &repoMarker{Version: 1, RepoID: id, Created: created, MAC: markerMAC(keys, id, created)}, nil
}

func parseAndVerifyMarker(keys *Keys, data []byte) (*repoMarker, error) {
	var m repoMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("repo marker malformed: %w", err)
	}
	want := markerMAC(keys, m.RepoID, m.Created)
	if !hmac.Equal([]byte(want), []byte(m.MAC)) {
		return nil, errors.New("repo marker authentication failed — the repo key changed or the marker was tampered with")
	}
	return &m, nil
}

// VerifyRepo re-checks the root of trust before any sync operation:
// the remote marker must authenticate under our repo key and carry
// the repo id pinned in both the config and this database.
func (e *Engine) VerifyRepo(ctx context.Context) error {
	data, err := e.Remote.Get(ctx, markerKey)
	if errors.Is(err, remote.ErrNotExist) {
		return errors.New("remote repo marker missing — repo not initialized or deleted; run `aii sync init`")
	}
	if err != nil {
		return fmt.Errorf("fetch repo marker: %w", err)
	}
	m, err := parseAndVerifyMarker(e.Keys, data)
	if err != nil {
		return err
	}
	if e.Config.RepoID != "" && m.RepoID != e.Config.RepoID {
		return errors.New("remote repo id does not match the pinned config — possible repo swap; if intentional, re-run `aii sync init --rebind`")
	}
	dbPin, err := e.DB.SyncMetaGet(metaRepoID)
	if err != nil {
		return err
	}
	switch dbPin {
	case m.RepoID:
	case "":
		// Fresh or wiped DB: adopt the (config-verified) repo pin.
		if err := e.DB.SyncMetaSet(metaRepoID, m.RepoID); err != nil {
			return err
		}
	default:
		return errors.New("this database is bound to a different sync repo — syncing would mix corpora; if intentional, re-run `aii sync init --rebind`")
	}
	return nil
}

// --- init / join --------------------------------------------------------

// Init creates or joins a repo. cfg carries the remote settings; the
// pins (RepoID, Fingerprint) are filled in here. passphrase is called
// with confirm=true when creating (prompt twice) and confirm=false
// when joining.
func Init(ctx context.Context, db *store.DB, dataDir string, cfg *Config, passphrase func(confirm bool) (string, error), rebind bool) error {
	if _, err := os.Stat(ConfigPath(dataDir)); err == nil && !rebind {
		return errors.New("sync is already configured for this database — see `aii sync status`, or use --rebind to re-pin after changing repos")
	}
	rem, err := cfg.OpenRemote()
	if err != nil {
		return err
	}

	var keys *Keys
	wrapped, err := rem.Get(ctx, masterKeyKey)
	switch {
	case errors.Is(err, remote.ErrNotExist):
		// Create. The wrapped key is write-once: if a peer wins the
		// race we fall back to joining their repo.
		pass, perr := passphrase(true)
		if perr != nil {
			return perr
		}
		keys, err = GenerateKeys()
		if err != nil {
			return err
		}
		blob, werr := WrapKeys(keys, pass)
		if werr != nil {
			return werr
		}
		switch perr := rem.PutIfAbsent(ctx, masterKeyKey, blob); {
		case perr == nil:
		case errors.Is(perr, remote.ErrExists):
			fmt.Fprintln(os.Stderr, "aii sync: another machine initialized this repo first — joining it instead")
			wrapped, err = rem.Get(ctx, masterKeyKey)
			if err != nil {
				return err
			}
			keys, err = UnwrapKeys(wrapped, pass)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("upload wrapped key: %w", perr)
		}
	case err != nil:
		return fmt.Errorf("check remote for existing repo: %w", err)
	default:
		// Join.
		pass, perr := passphrase(false)
		if perr != nil {
			return perr
		}
		keys, err = UnwrapKeys(wrapped, pass)
		if err != nil {
			return err
		}
	}

	// Repo marker: fetch-or-create, always MAC-verified.
	var marker *repoMarker
	if data, err := rem.Get(ctx, markerKey); err == nil {
		if marker, err = parseAndVerifyMarker(keys, data); err != nil {
			return err
		}
	} else if errors.Is(err, remote.ErrNotExist) {
		if marker, err = newRepoMarker(keys); err != nil {
			return err
		}
		blob, _ := json.Marshal(marker)
		if perr := rem.PutIfAbsent(ctx, markerKey, blob); perr != nil && !errors.Is(perr, remote.ErrExists) {
			return fmt.Errorf("upload repo marker: %w", perr)
		} else if errors.Is(perr, remote.ErrExists) {
			data, gerr := rem.Get(ctx, markerKey)
			if gerr != nil {
				return gerr
			}
			if marker, err = parseAndVerifyMarker(keys, data); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("fetch repo marker: %w", err)
	}

	// Bind: DB pin, config pins, local keyfile.
	dbPin, err := db.SyncMetaGet(metaRepoID)
	if err != nil {
		return err
	}
	if dbPin != "" && dbPin != marker.RepoID && !rebind {
		return errors.New("this database is bound to a different sync repo — syncing would mix corpora; use --rebind if this is intentional")
	}
	if err := db.SyncMetaSet(metaRepoID, marker.RepoID); err != nil {
		return err
	}
	cfg.RepoID = marker.RepoID
	cfg.Fingerprint = keys.Fingerprint
	if err := SaveKeyFile(KeyPath(dataDir), keys); err != nil {
		return err
	}
	if err := SaveConfig(dataDir, cfg); err != nil {
		return err
	}
	fmt.Printf("sync configured: %s\nrepo id: %s\nkey fingerprint: %s\n", cfg.Describe(), marker.RepoID, keys.Fingerprint[:16])
	return nil
}

// --- planning -----------------------------------------------------------

type remoteName struct {
	versions  []version
	best      *version
	tombstone bool // listing showed a purged marker (content unverified)
}

// listRemote parses one listing of bundles/ into per-name version
// sets. Anything outside the strict grammar is ignored.
func (e *Engine) listRemote(ctx context.Context) (map[string]*remoteName, error) {
	objs, err := e.Remote.List(ctx, "bundles/")
	if err != nil {
		return nil, fmt.Errorf("list remote: %w", err)
	}
	out := map[string]*remoteName{}
	get := func(name string) *remoteName {
		if out[name] == nil {
			out[name] = &remoteName{}
		}
		return out[name]
	}
	for _, o := range objs {
		if v, ok := parseVersionKey(o.Key); ok {
			v.Size = o.Size
			get(v.Name).versions = append(get(v.Name).versions, v)
			continue
		}
		if name, ok := parseTombstoneKey(o.Key); ok {
			get(name).tombstone = true
			continue
		}
		e.verbosef("ignoring unrecognized remote object %q", o.Key)
	}
	for _, rn := range out {
		if len(rn.versions) == 0 {
			continue
		}
		// Best = highest (epoch, count); head8 tiebreak keeps every
		// machine deterministic when conflict siblings exist.
		sort.Slice(rn.versions, func(i, j int) bool {
			a, b := rn.versions[i], rn.versions[j]
			if a.Epoch != b.Epoch {
				return a.Epoch > b.Epoch
			}
			if a.Count != b.Count {
				return a.Count > b.Count
			}
			return a.Head8 < b.Head8
		})
		rn.best = &rn.versions[0]
	}
	return out, nil
}

type localSession struct {
	item  store.SessionListItem
	state *store.SyncState // nil = never synced
	name  string
}

func (l *localSession) epoch() int64 {
	if l.state != nil {
		return l.state.Epoch
	}
	return 1
}

// syncEligible: what push will consider. Codex history.jsonl prompt
// sessions are excluded — the shared log's rotation makes them a
// divergence factory, and each is a single prompt with little recall
// value.
func syncEligible(item store.SessionListItem) bool {
	if item.MessageCount == 0 {
		return false
	}
	if item.Agent == "codex" && filepath.Base(item.SourcePath) == "history.jsonl" {
		return false
	}
	return true
}

func (e *Engine) localSessions() ([]*localSession, map[string]*localSession, error) {
	items, err := e.DB.ListSessions(store.SessionFilter{Limit: 1 << 30})
	if err != nil {
		return nil, nil, err
	}
	states, err := e.DB.ListSyncStates()
	if err != nil {
		return nil, nil, err
	}
	stateBy := map[string]*store.SyncState{}
	for i := range states {
		s := states[i]
		stateBy[s.Agent+"\x00"+s.UID] = &s
	}
	var all []*localSession
	byName := map[string]*localSession{}
	for i := range items {
		it := items[i]
		l := &localSession{
			item:  it,
			state: stateBy[it.Agent+"\x00"+it.UID],
			name:  e.Keys.BundleName(it.Agent, it.UID),
		}
		all = append(all, l)
		byName[l.name] = l
	}
	return all, byName, nil
}

// tombstoneValid fetches and authenticates a purge marker, caching
// per run. Forged tombstones are ignored — otherwise bucket write
// access alone could suppress pushes.
func (e *Engine) tombstoneValid(ctx context.Context, name string, cache map[string]bool) bool {
	if v, ok := cache[name]; ok {
		return v
	}
	data, err := e.Remote.Get(ctx, tombstoneKey(name))
	valid := err == nil && verifyTombstone(e.Keys, name, data)
	if err != nil && !errors.Is(err, remote.ErrNotExist) {
		e.warnf("could not fetch tombstone for %s: %v", name, err)
	}
	cache[name] = valid
	return valid
}

func head8(h chainHash) string { return hex.EncodeToString(h[:4]) }

func storedHead8(st *store.SyncState) string {
	if st == nil || len(st.ChainHash) != 32 {
		return ""
	}
	return hex.EncodeToString(st.ChainHash[:4])
}

// --- push ---------------------------------------------------------------

type PushStats struct {
	Pushed, Messages, UpToDate, Skipped, Conflicts, Excluded, GCed int
}

func (e *Engine) Push(ctx context.Context, dryRun bool) (PushStats, error) {
	var st PushStats
	if err := e.VerifyRepo(ctx); err != nil {
		return st, err
	}
	rem, err := e.listRemote(ctx)
	if err != nil {
		return st, err
	}
	locals, byName, err := e.localSessions()
	if err != nil {
		return st, err
	}
	st.GCed = e.gc(ctx, rem, byName, dryRun)
	tombCache := map[string]bool{}

	for _, l := range locals {
		res, msgs, err := e.pushOne(ctx, l, rem[l.name], tombCache, dryRun)
		if err != nil {
			if ctx.Err() != nil {
				return st, ctx.Err()
			}
			e.warnf("push %s/%s: %v", l.item.Agent, shortUID(l.item.UID), err)
			st.Skipped++
			continue
		}
		switch res {
		case pushPushed:
			st.Pushed++
			st.Messages += msgs
		case pushUpToDate:
			st.UpToDate++
		case pushConflictDeferred:
			st.Conflicts++
		case pushExcluded:
			st.Excluded++
		case pushSkipped:
			st.Skipped++
		}
	}
	return st, nil
}

type pushResult int

const (
	pushSkipped pushResult = iota
	pushExcluded
	pushUpToDate
	pushPushed
	pushConflictDeferred
)

func (e *Engine) pushOne(ctx context.Context, l *localSession, rn *remoteName, tombCache map[string]bool, dryRun bool) (pushResult, int, error) {
	if !syncEligible(l.item) {
		return pushSkipped, 0, nil
	}
	if l.state != nil && l.state.Excluded {
		return pushExcluded, 0, nil
	}
	if rn != nil && rn.tombstone && e.tombstoneValid(ctx, l.name, tombCache) {
		// A peer purged this session. Respect it and remember.
		if !dryRun {
			if err := e.DB.SaveSyncState(store.SyncState{
				Agent: l.item.Agent, UID: l.item.UID, Epoch: l.epoch(), Excluded: true,
			}); err != nil {
				return pushSkipped, 0, err
			}
		}
		e.verbosef("%s/%s was purged remotely — excluded from push (use `aii sync purge --local` to drop the local copy too)", l.item.Agent, shortUID(l.item.UID))
		return pushExcluded, 0, nil
	}

	var best *version
	if rn != nil {
		best = rn.best
	}

	// Fast path: local unchanged since last sync and remote carries
	// exactly that version — no message load needed.
	if !e.Recheck && l.state != nil && int64(l.state.ChainCount) == l.item.MessageCount &&
		best != nil && best.Epoch == l.state.Epoch && best.Count == l.state.ChainCount &&
		best.Head8 == storedHead8(l.state) {
		return pushUpToDate, 0, nil
	}

	rows, err := e.DB.SessionMessages(l.item.ID)
	if err != nil {
		return pushSkipped, 0, err
	}
	for i, r := range rows {
		if r.Ordinal != i {
			e.warnf("%s/%s has non-contiguous ordinals — not syncing this session", l.item.Agent, shortUID(l.item.UID))
			return pushSkipped, 0, nil
		}
	}

	epoch := l.epoch()
	// Self-divergence: does our cached chain prefix still describe
	// our own content? Truncated/renumbered/re-redacted local
	// history must supersede, never interleave.
	if l.state != nil && l.state.ChainCount > 0 {
		diverged := l.state.ChainCount > len(rows) ||
			(len(l.state.ChainHash) == 32 && chainAt(l.item.Agent, l.item.UID, rows, l.state.ChainCount) != chainHash(l.state.ChainHash))
		if diverged {
			epoch++
			e.verbosef("%s/%s: local history rewritten — bumping epoch to %d", l.item.Agent, shortUID(l.item.UID), epoch)
		}
	}

	head := computeChain(l.item.Agent, l.item.UID, rows)
	count := len(rows)

	if best != nil {
		switch {
		case best.Epoch > epoch:
			// Remote is ahead; pull first (bare `aii sync` does).
			return pushConflictDeferred, 0, nil
		case best.Epoch == epoch:
			switch {
			case best.Count == count && best.Head8 == head8(head):
				// Remote already has exactly this. Record it.
				if !dryRun {
					if err := e.saveChainState(l, epoch, count, head); err != nil {
						return pushSkipped, 0, err
					}
				}
				return pushUpToDate, 0, nil
			case best.Count < count && best.Head8 == head8(chainAt(l.item.Agent, l.item.UID, rows, best.Count)):
				// Clean extension of what's remote — push.
			case best.Count > count:
				// Remote longer at our epoch: a peer extended it.
				// Pull will merge; nothing to push now.
				return pushConflictDeferred, 0, nil
			default:
				// Same epoch, incompatible histories. Ours wins by
				// bumping the epoch (last-diverger-wins).
				epoch = best.Epoch + 1
				e.verbosef("%s/%s: diverged from remote — bumping epoch to %d", l.item.Agent, shortUID(l.item.UID), epoch)
			}
		}
	}

	if dryRun {
		e.verbosef("would push %s/%s (epoch %d, %d msgs)", l.item.Agent, shortUID(l.item.UID), epoch, count)
		return pushPushed, count, nil
	}

	b := &Bundle{
		Format:     FormatVersion,
		Agent:      l.item.Agent,
		SessionUID: l.item.UID,
		Workspace:  l.item.Workspace,
		Title:      l.item.Title,
		Summary:    l.item.Summary,
		StartedAt:  l.item.StartedAt,
		EndedAt:    l.item.EndedAt,
		Epoch:      epoch,
		ChainHead:  hex.EncodeToString(head[:]),
	}
	b.Messages = make([]BundleMessage, len(rows))
	for i, r := range rows {
		b.Messages[i] = BundleMessage{Ordinal: r.Ordinal, Role: r.Role, TS: r.TS, Content: r.Content}
	}
	key := versionKey(l.name, epoch, count, head)
	blob, err := EncodeBundle(b, e.Keys, key)
	if err != nil {
		return pushSkipped, 0, err
	}
	switch err := e.Remote.PutIfAbsent(ctx, key, blob); {
	case err == nil:
	case errors.Is(err, remote.ErrExists):
		// A peer pushed this identical logical version. Fine.
	default:
		return pushSkipped, 0, err
	}
	if err := e.saveChainState(l, epoch, count, head); err != nil {
		return pushSkipped, 0, err
	}
	e.verbosef("pushed %s/%s (epoch %d, %d msgs, %d bytes)", l.item.Agent, shortUID(l.item.UID), epoch, count, len(blob))
	return pushPushed, count, nil
}

func (e *Engine) saveChainState(l *localSession, epoch int64, count int, head chainHash) error {
	return e.DB.SaveSyncState(store.SyncState{
		Agent: l.item.Agent, UID: l.item.UID,
		Epoch: epoch, ChainCount: count, ChainHash: head[:],
	})
}

// gc deletes remote versions strictly superseded by a version present
// in this same listing — meaning the superseding object is durably
// visible before anything is removed. A lower epoch is always
// superseded. At the same epoch, a shorter version goes only when it
// is provably an ancestor of best, so a divergent sibling survives
// whatever its length. Tombstones never expire.
func (e *Engine) gc(ctx context.Context, rem map[string]*remoteName, byName map[string]*localSession, dryRun bool) int {
	n := 0
	for name, rn := range rem {
		if rn.best == nil {
			continue
		}
		ancestorOfBest := e.ancestorChecker(name, rn, byName)
		for _, v := range rn.versions {
			superseded := v.Epoch < rn.best.Epoch ||
				(v.Epoch == rn.best.Epoch && v.Count < rn.best.Count && ancestorOfBest(v))
			if !superseded {
				continue
			}
			if dryRun {
				n++
				continue
			}
			var h chainHash
			hb, err := hex.DecodeString(v.Head8)
			if err != nil || len(hb) != 4 {
				continue
			}
			copy(h[:4], hb)
			if err := e.Remote.Delete(ctx, versionKey(name, v.Epoch, v.Count, h)); err != nil {
				e.warnf("gc %s: %v", name, err)
				continue
			}
			n++
		}
	}
	return n
}

// ancestorChecker returns a predicate reporting whether a shorter
// same-epoch version is an ancestor of rn.best rather than a divergent
// sibling that happens to be shorter. A listing carries only counts
// and chain heads, so the evidence has to come from content: when best
// is exactly the local copy's chain, chainAt over the local rows
// reproduces the head of every prefix of best, and a version whose
// head matches at its own count is an ancestor. With no local copy, or
// one that is not what best holds, nothing is collected.
func (e *Engine) ancestorChecker(name string, rn *remoteName, byName map[string]*localSession) func(version) bool {
	var (
		loaded bool
		l      *localSession
		rows   []store.Row
	)
	return func(v version) bool {
		if !loaded {
			loaded = true
			l = byName[name]
			if l == nil {
				return false
			}
			r, err := e.DB.SessionMessages(l.item.ID)
			if err != nil {
				e.warnf("gc %s: read local messages: %v", name, err)
				l = nil
				return false
			}
			if rn.best.Count != len(r) || rn.best.Head8 != head8(computeChain(l.item.Agent, l.item.UID, r)) {
				l = nil
				return false
			}
			rows = r
		}
		if l == nil {
			return false
		}
		return v.Count <= len(rows) && v.Head8 == head8(chainAt(l.item.Agent, l.item.UID, rows, v.Count))
	}
}

// --- pull ---------------------------------------------------------------

type PullStats struct {
	New, Extended, Superseded, UpToDate, Conflicts, Rejected, Messages int
}

type stagedBundle struct {
	bundle *Bundle
	key    string
}

func (e *Engine) Pull(ctx context.Context, dryRun bool) (PullStats, error) {
	var st PullStats
	if err := e.VerifyRepo(ctx); err != nil {
		return st, err
	}
	rem, err := e.listRemote(ctx)
	if err != nil {
		return st, err
	}
	_, byName, err := e.localSessions()
	if err != nil {
		return st, err
	}
	tombCache := map[string]bool{}

	// Decide what to fetch.
	type candidate struct {
		name string
		v    version
	}
	var candidates []candidate
	for name, rn := range rem {
		if rn.best == nil {
			continue
		}
		if rn.tombstone && e.tombstoneValid(ctx, name, tombCache) {
			continue
		}
		if rn.best.Size > maxObjectSize {
			e.warnf("remote bundle %s exceeds the size limit (%d bytes) — skipping", name, rn.best.Size)
			st.Rejected++
			continue
		}
		l := byName[name]
		if l == nil {
			candidates = append(candidates, candidate{name, *rn.best})
			continue
		}
		if rn.best.Epoch > l.epoch() ||
			(rn.best.Epoch == l.epoch() && int64(rn.best.Count) > l.item.MessageCount) {
			candidates = append(candidates, candidate{name, *rn.best})
		}
	}
	st.UpToDate = len(rem) - len(candidates)

	if dryRun {
		for _, c := range candidates {
			e.verbosef("would pull %s (epoch %d, %d msgs)", c.name, c.v.Epoch, c.v.Count)
		}
		st.New = len(candidates)
		return st, nil
	}

	// Stage over the network without any lock, then apply each chunk
	// under the index lock, one transaction per session.
	//
	// A first pull of a large corpus is CPU-bound on FTS indexing
	// (like a first `aii index`) and can run for many minutes —
	// report progress so the silence doesn't read as a hang.
	began := time.Now()
	lastProgress := began
	applied := 0
	for start := 0; start < len(candidates); start += applyChunk {
		endIdx := start + applyChunk
		if endIdx > len(candidates) {
			endIdx = len(candidates)
		}
		var staged []stagedBundle
		for _, c := range candidates[start:endIdx] {
			var h chainHash
			hb, err := hex.DecodeString(c.v.Head8)
			if err != nil || len(hb) != 4 {
				st.Rejected++
				continue
			}
			copy(h[:4], hb)
			key := versionKey(c.name, c.v.Epoch, c.v.Count, h)
			data, err := e.Remote.Get(ctx, key)
			if errors.Is(err, remote.ErrNotExist) {
				// GC'd between listing and fetch — converges next run.
				continue
			}
			if err != nil {
				if ctx.Err() != nil {
					return st, ctx.Err()
				}
				e.warnf("fetch %s: %v", key, err)
				st.Rejected++
				continue
			}
			b, err := DecodeBundle(data, e.Keys, key)
			if err != nil {
				e.warnf("reject %s: %v", key, err)
				st.Rejected++
				continue
			}
			staged = append(staged, stagedBundle{bundle: b, key: key})
		}
		if len(staged) == 0 {
			continue
		}

		release, err := e.lock()
		if err != nil {
			return st, fmt.Errorf("acquire index lock for apply: %w", err)
		}
		for _, sb := range staged {
			res, msgs, err := e.applyBundle(sb.bundle)
			if err != nil {
				release()
				return st, fmt.Errorf("apply %s: %w", sb.key, err)
			}
			applied++
			switch res {
			case applyNew:
				st.New++
				st.Messages += msgs
			case applyExtended:
				st.Extended++
				st.Messages += msgs
			case applySuperseded:
				st.Superseded++
				st.Messages += msgs
			case applyUpToDate:
				st.UpToDate++
			case applyConflict:
				st.Conflicts++
			}
		}
		release()
		if time.Since(began) > 10*time.Second && time.Since(lastProgress) > 10*time.Second {
			lastProgress = time.Now()
			e.warnf("pulled %d/%d sessions (%d msgs) — building the local search index, this first pull can take a while", applied, len(candidates), st.Messages)
		}
	}
	return st, nil
}

type applyResult int

const (
	applyUpToDate applyResult = iota
	applyNew
	applyExtended
	applySuperseded
	applyConflict
)

// applyBundle merges one verified bundle into the local DB inside a
// single transaction for its messages. Pulled content re-passes
// redaction (idempotent) so a --no-redact peer can't seed secrets.
func (e *Engine) applyBundle(b *Bundle) (applyResult, int, error) {
	for i := range b.Messages {
		b.Messages[i].Content = redact.Redact(b.Messages[i].Content)
	}
	sess := &store.Session{
		Agent: b.Agent, UID: b.SessionUID,
		Workspace: b.Workspace,
		Title:     redact.Redact(b.Title),
		Summary:   redact.Redact(b.Summary),
		StartedAt: b.StartedAt, EndedAt: b.EndedAt,
	}

	existing, err := e.DB.SessionByUID(b.Agent, b.SessionUID)
	if err != nil {
		return 0, 0, err
	}

	toMessages := func(ms []BundleMessage) []store.Message {
		out := make([]store.Message, len(ms))
		for i, m := range ms {
			out[i] = store.Message{Ordinal: m.Ordinal, Role: m.Role, TS: m.TS, Content: m.Content}
		}
		return out
	}

	var (
		res     applyResult
		id      int64
		newMsgs int
	)
	if existing == nil {
		if id, err = e.DB.UpsertSessionFromSync(sess); err != nil {
			return 0, 0, err
		}
		tx, err := e.DB.Begin()
		if err != nil {
			return 0, 0, err
		}
		if err := e.DB.InsertMessages(tx, id, toMessages(b.Messages)); err != nil {
			tx.Rollback()
			return 0, 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, 0, err
		}
		res, newMsgs = applyNew, len(b.Messages)
	} else {
		id = existing.ID
		localRows, err := e.DB.SessionMessages(id)
		if err != nil {
			return 0, 0, err
		}
		state, err := e.DB.GetSyncState(b.Agent, b.SessionUID)
		if err != nil {
			return 0, 0, err
		}
		localEpoch := int64(1)
		if state != nil {
			localEpoch = state.Epoch
		}
		bundleRows := b.rows()
		prefixCompatible := len(localRows) <= len(bundleRows) &&
			computeChain(b.Agent, b.SessionUID, localRows) == chainAt(b.Agent, b.SessionUID, bundleRows, len(localRows))

		switch {
		case b.Epoch < localEpoch:
			return applyUpToDate, 0, nil
		case b.Epoch == localEpoch && len(bundleRows) <= len(localRows):
			return applyUpToDate, 0, nil
		case prefixCompatible:
			// Clean extension (possibly adopting a higher epoch).
			if _, err := e.DB.UpsertSessionFromSync(sess); err != nil {
				return 0, 0, err
			}
			tail := toMessages(b.Messages[len(localRows):])
			tx, err := e.DB.Begin()
			if err != nil {
				return 0, 0, err
			}
			if err := e.DB.InsertMessages(tx, id, tail); err != nil {
				tx.Rollback()
				return 0, 0, err
			}
			if err := tx.Commit(); err != nil {
				return 0, 0, err
			}
			res, newMsgs = applyExtended, len(tail)
		case b.Epoch > localEpoch:
			// Divergent higher epoch supersedes local content.
			if _, err := e.DB.UpsertSessionFromSync(sess); err != nil {
				return 0, 0, err
			}
			tx, err := e.DB.Begin()
			if err != nil {
				return 0, 0, err
			}
			if err := e.DB.ReplaceSessionMessages(tx, id, toMessages(b.Messages)); err != nil {
				tx.Rollback()
				return 0, 0, err
			}
			if err := tx.Commit(); err != nil {
				return 0, 0, err
			}
			res, newMsgs = applySuperseded, len(b.Messages)
		default:
			// Same epoch, incompatible histories: keep local content
			// and claim the next epoch here, so the next push really
			// does supersede. Push cannot make that decision when the
			// remote is the longer side — it defers to the pull — so
			// without this bump neither side ever moves and the
			// session stays in conflict forever.
			e.warnf("%s/%s diverged from the remote copy — keeping local; next push will supersede", b.Agent, shortUID(b.SessionUID))
			localHead := computeChain(b.Agent, b.SessionUID, localRows)
			if err := e.DB.SaveSyncState(store.SyncState{
				Agent: b.Agent, UID: b.SessionUID,
				Epoch: localEpoch + 1, ChainCount: len(localRows), ChainHash: localHead[:],
			}); err != nil {
				return 0, 0, err
			}
			return applyConflict, 0, nil
		}
	}

	// Record the chain of what is actually in the DB now (redaction
	// may have altered pulled content — the DB is the truth).
	rows, err := e.DB.SessionMessages(id)
	if err != nil {
		return 0, 0, err
	}
	head := computeChain(b.Agent, b.SessionUID, rows)
	if err := e.DB.SaveSyncState(store.SyncState{
		Agent: b.Agent, UID: b.SessionUID,
		Epoch: b.Epoch, ChainCount: len(rows), ChainHash: head[:],
	}); err != nil {
		return 0, 0, err
	}
	return res, newMsgs, nil
}

// --- status -------------------------------------------------------------

type Status struct {
	Remote           string   `json:"remote"`
	RepoID           string   `json:"repo_id"`
	Fingerprint      string   `json:"fingerprint"`
	LocalSessions    int      `json:"local_sessions"`
	SyncEligible     int      `json:"sync_eligible"`
	RemoteSessions   int      `json:"remote_sessions"`
	PendingPush      int      `json:"pending_push"`
	PendingPull      int      `json:"pending_pull"`
	Excluded         int      `json:"excluded"`
	Diverged         []string `json:"diverged,omitempty"`
	ConflictSiblings []string `json:"conflict_siblings,omitempty"`
}

func (e *Engine) Status(ctx context.Context) (*Status, error) {
	if err := e.VerifyRepo(ctx); err != nil {
		return nil, err
	}
	rem, err := e.listRemote(ctx)
	if err != nil {
		return nil, err
	}
	locals, byName, err := e.localSessions()
	if err != nil {
		return nil, err
	}

	s := &Status{
		Remote:      e.Config.Describe(),
		RepoID:      e.Config.RepoID,
		Fingerprint: e.Keys.Fingerprint,
	}
	s.LocalSessions = len(locals)
	s.RemoteSessions = len(rem)

	for _, l := range locals {
		if !syncEligible(l.item) {
			continue
		}
		s.SyncEligible++
		if l.state != nil && l.state.Excluded {
			s.Excluded++
			continue
		}
		rn := rem[l.name]
		cite := shortAgent(l.item.Agent) + "/" + shortUID(l.item.UID)
		switch {
		case rn == nil || rn.best == nil:
			s.PendingPush++
		case l.state != nil && rn.best.Epoch == l.state.Epoch && rn.best.Count == l.state.ChainCount &&
			rn.best.Head8 != storedHead8(l.state) && storedHead8(l.state) != "":
			s.Diverged = append(s.Diverged, cite)
		case l.state == nil || int64(l.state.ChainCount) != l.item.MessageCount ||
			rn.best.Epoch != l.state.Epoch || rn.best.Count != l.state.ChainCount:
			// Something moved on one side or the other.
			if rn.best.Epoch > l.epoch() || (rn.best.Epoch == l.epoch() && int64(rn.best.Count) > l.item.MessageCount) {
				s.PendingPull++
			} else {
				s.PendingPush++
			}
		}
	}
	for name, rn := range rem {
		if rn.best == nil {
			continue
		}
		if byName[name] == nil && !rn.tombstone {
			s.PendingPull++
		}
		for _, v := range rn.versions[1:] {
			if v.Epoch == rn.best.Epoch && v.Count == rn.best.Count && v.Head8 != rn.best.Head8 {
				s.ConflictSiblings = append(s.ConflictSiblings, name)
				break
			}
		}
	}
	return s, nil
}

// --- purge --------------------------------------------------------------

// Purge writes an authenticated tombstone, deletes every remote
// version of the session, and excludes it from future pushes. Peers
// keep their local copies (documented; they run --local themselves).
func (e *Engine) Purge(ctx context.Context, agent, uid string, localToo bool) error {
	if err := e.VerifyRepo(ctx); err != nil {
		return err
	}
	name := e.Keys.BundleName(agent, uid)
	switch err := e.Remote.PutIfAbsent(ctx, tombstoneKey(name), tombstoneBody(e.Keys, name)); {
	case err == nil, errors.Is(err, remote.ErrExists):
	default:
		return fmt.Errorf("write tombstone: %w", err)
	}
	objs, err := e.Remote.List(ctx, "bundles/"+name+"/")
	if err != nil {
		return err
	}
	for _, o := range objs {
		if _, ok := parseVersionKey(o.Key); !ok {
			continue
		}
		if err := e.Remote.Delete(ctx, o.Key); err != nil {
			return fmt.Errorf("delete %s: %w", o.Key, err)
		}
	}

	release, err := e.lock()
	if err != nil {
		return err
	}
	defer release()
	if err := e.DB.SaveSyncState(store.SyncState{Agent: agent, UID: uid, Epoch: 1, Excluded: true}); err != nil {
		return err
	}
	if localToo {
		if err := e.DB.DeleteSessionByUID(agent, uid); err != nil {
			return err
		}
	}
	return nil
}

// --- shared helpers -----------------------------------------------------

func shortUID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// shortAgent mirrors the cite-token prefixes used across aii.
func shortAgent(agent string) string {
	switch agent {
	case "claude_code":
		return "cc"
	case "codex":
		return "cdx"
	case "cursor":
		return "cur"
	}
	return agent
}
