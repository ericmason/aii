package main

// `aii sync` — end-to-end encrypted cross-machine sync. The engine
// lives in internal/cloudsync; this file is CLI plumbing: flag
// parsing, passphrase prompting, lock wiring, and output formatting.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/cloudsync"
	"github.com/ericmason/aii/internal/store"
	"golang.org/x/term"
)

const syncUsageText = `aii sync — end-to-end encrypted sync via storage you control

Usage:
  aii sync                      # pull then push
  aii sync init  --remote <s3://bucket[/prefix] | /abs/dir>
                 [--endpoint URL] [--region R] [--path-style]
                 [--access-key-id ID] [--secret-access-key KEY] [--rebind]
  aii sync status [--json]
  aii sync push  [--dry-run] [--verbose] [--quiet] [--recheck]
  aii sync pull  [--dry-run] [--verbose] [--quiet]
  aii sync purge <session-ref> [--local] [--yes]

Everything uploaded is encrypted and authenticated on this machine
(age + HMAC); the storage provider sees only sizes, timing, and
pseudonymous names. The same passphrase joins other machines:
run the same init command there and enter it when prompted.

Environment:
  AII_SYNC_PASSPHRASE            passphrase for non-interactive init/join
  AII_SYNC_S3_ACCESS_KEY_ID      S3 credentials (fall back to AWS_ACCESS_KEY_ID,
  AII_SYNC_S3_SECRET_ACCESS_KEY  AWS_SECRET_ACCESS_KEY, then the config file)
`

func cmdSync(ctx context.Context, args []string) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest := args[0], args[1:]
		switch sub {
		case "init":
			return syncInit(ctx, rest)
		case "status":
			return syncStatus(ctx, rest)
		case "push":
			return syncRun(ctx, rest, false, true)
		case "pull":
			return syncRun(ctx, rest, true, false)
		case "purge":
			return syncPurge(ctx, rest)
		case "help", "-h", "--help":
			fmt.Print(syncUsageText)
			return nil
		default:
			return fmt.Errorf("unknown sync subcommand %q\n\n%s", sub, syncUsageText)
		}
	}
	return syncRun(ctx, args, true, true)
}

// --- init ---------------------------------------------------------------

func syncInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync init", flag.ExitOnError)
	remoteSpec := fs.String("remote", "", "s3://bucket[/prefix] or an absolute directory path")
	endpoint := fs.String("endpoint", "", "S3 endpoint override for R2/B2/MinIO (https://...)")
	region := fs.String("region", "", "S3 region (default us-east-1; 'auto' for R2)")
	pathStyle := fs.Bool("path-style", false, "force path-style S3 addressing (default with --endpoint)")
	accessKey := fs.String("access-key-id", "", "S3 access key id (stored 0600; prefer env vars)")
	secretKey := fs.String("secret-access-key", "", "S3 secret key (stored 0600; prefer env vars)")
	rebind := fs.Bool("rebind", false, "re-pin the repo id / key fingerprint after an intentional repo change")
	fs.Parse(reorderFlags(args))

	cfg := &cloudsync.Config{
		Endpoint: *endpoint, Region: *region, PathStyle: *pathStyle,
		AccessKeyID: *accessKey, SecretAccessKey: *secretKey,
	}
	switch {
	case *remoteSpec == "" && *rebind:
		// Rebind without --remote reuses the stored remote settings.
		old, err := cloudsync.LoadConfig(dataDir())
		if err != nil {
			return err
		}
		old.RepoID, old.Fingerprint = "", ""
		cfg = old
	case *remoteSpec == "":
		return errors.New("--remote is required (s3://bucket[/prefix] or an absolute directory path)")
	case strings.HasPrefix(*remoteSpec, "s3://"):
		rest := strings.TrimPrefix(*remoteSpec, "s3://")
		bucket, prefix, _ := strings.Cut(rest, "/")
		if bucket == "" {
			return fmt.Errorf("invalid --remote %q", *remoteSpec)
		}
		cfg.RemoteType = "s3"
		cfg.Bucket = bucket
		cfg.Prefix = strings.Trim(prefix, "/")
	default:
		p := *remoteSpec
		if !filepath.IsAbs(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				return err
			}
			p = abs
		}
		if err := refuseNestedDirRemote(p); err != nil {
			return err
		}
		cfg.RemoteType = "dir"
		cfg.Path = filepath.Clean(p)
	}

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	return cloudsync.Init(ctx, db, dataDir(), cfg, promptPassphrase, *rebind)
}

// refuseNestedDirRemote blocks the classic foot-gun of pointing the
// dir remote inside the aii data dir (or vice versa) — syncing the
// database into itself, or publishing the key file.
func refuseNestedDirRemote(p string) error {
	data := dataDir()
	rel, err := filepath.Rel(data, p)
	if err == nil && rel == "." || err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("--remote %s is inside the aii data dir (%s) — pick a directory outside it", p, data)
	}
	rel, err = filepath.Rel(p, data)
	if err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("--remote %s contains the aii data dir (%s) — pick a directory outside it", p, data)
	}
	return nil
}

// promptPassphrase satisfies cloudsync.Init's callback. Environment
// wins (scripted joins); otherwise prompt on the terminal, twice when
// creating a new repo.
func promptPassphrase(confirm bool) (string, error) {
	if p := os.Getenv("AII_SYNC_PASSPHRASE"); p != "" {
		return p, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("no terminal to prompt for the passphrase — set AII_SYNC_PASSPHRASE")
	}
	read := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	if confirm {
		fmt.Fprintln(os.Stderr, "Creating a new sync repo. This passphrase lets other machines join —")
		fmt.Fprintln(os.Stderr, "and is the ONLY way to recover your key. There is no reset.")
	}
	p, err := read("sync passphrase: ")
	if err != nil {
		return "", err
	}
	if confirm {
		p2, err := read("confirm passphrase: ")
		if err != nil {
			return "", err
		}
		if p != p2 {
			return "", errors.New("passphrases do not match")
		}
	}
	return p, nil
}

// --- engine loading -----------------------------------------------------

func loadSyncEngine(db *store.DB, verbose bool) (*cloudsync.Engine, error) {
	eng, err := cloudsync.LoadEngine(dataDir(), db)
	if err != nil {
		return nil, err
	}
	eng.Verbose = verbose
	// The engine takes the index lock only around local SQLite apply
	// phases — network staging runs lockless, so a slow pull can't
	// starve the cron indexer.
	eng.LockForApply = func() (func(), error) {
		var pid int
		for attempt := 0; attempt < 20; attempt++ {
			release, ok, otherPID := acquireIndexLock()
			if ok {
				return release, nil
			}
			pid = otherPID
			time.Sleep(500 * time.Millisecond)
		}
		return nil, fmt.Errorf("index lock held by pid %d — an indexer is running; try again shortly", pid)
	}
	return eng, nil
}

// --- push / pull / bare sync -------------------------------------------

func syncRun(ctx context.Context, args []string, doPull, doPush bool) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "show what would transfer without changing anything")
	verbose := fs.Bool("verbose", false, "log each session synced")
	quiet := fs.Bool("quiet", false, "suppress the summary line (used by cron)")
	recheck := fs.Bool("recheck", false, "recompute every session's chain instead of trusting cached state")
	fs.Parse(reorderFlags(args))

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	eng, err := loadSyncEngine(db, *verbose)
	if err != nil {
		return err
	}
	eng.Recheck = *recheck

	release, ok, pid := acquireLockAt(syncLockPath())
	if !ok {
		if !*quiet {
			fmt.Printf("another sync is running (pid %d) — skipping\n", pid)
		}
		return nil
	}
	defer release()

	start := time.Now()
	var (
		pl cloudsync.PullStats
		ps cloudsync.PushStats
	)
	if doPull {
		if pl, err = eng.Pull(ctx, *dryRun); err != nil {
			return err
		}
	}
	if doPush {
		if ps, err = eng.Push(ctx, *dryRun); err != nil {
			return err
		}
	}
	if !*dryRun {
		touchStamp(syncStampPath())
	}
	if *quiet {
		return nil
	}

	var parts []string
	if doPull {
		if n := pl.New + pl.Extended + pl.Superseded; n > 0 {
			parts = append(parts, fmt.Sprintf("pulled %d session%s (%d msgs)", n, plural(n), pl.Messages))
		}
		if pl.Conflicts > 0 {
			parts = append(parts, fmt.Sprintf("%d conflict%s kept local", pl.Conflicts, plural(pl.Conflicts)))
		}
		if pl.Rejected > 0 {
			parts = append(parts, fmt.Sprintf("%d rejected", pl.Rejected))
		}
	}
	if doPush {
		if ps.Pushed > 0 {
			parts = append(parts, fmt.Sprintf("pushed %d session%s (%d msgs)", ps.Pushed, plural(ps.Pushed), ps.Messages))
		}
		if ps.Conflicts > 0 {
			parts = append(parts, fmt.Sprintf("%d deferred", ps.Conflicts))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "up to date")
	}
	verb := ""
	if *dryRun {
		verb = " (dry run)"
	}
	fmt.Printf("%s in %.1fs%s\n", strings.Join(parts, ", "), time.Since(start).Seconds(), verb)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func touchStamp(path string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		fmt.Fprintln(f, time.Now().Format(time.RFC3339Nano))
		f.Close()
	}
}

// --- status -------------------------------------------------------------

func syncStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit machine-readable status")
	fs.Parse(reorderFlags(args))

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	eng, err := loadSyncEngine(db, false)
	if err != nil {
		return err
	}
	st, err := eng.Status(ctx)
	if err != nil {
		return err
	}

	lastSync := ""
	if info, err := os.Stat(syncStampPath()); err == nil {
		lastSync = humanDuration(time.Since(info.ModTime())) + " ago"
	}

	if *asJSON {
		out := struct {
			*cloudsync.Status
			LastSync string `json:"last_sync,omitempty"`
		}{st, lastSync}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Printf("remote:        %s\n", st.Remote)
	fmt.Printf("repo id:       %s\n", st.RepoID)
	fmt.Printf("fingerprint:   %s\n", st.Fingerprint[:16])
	fmt.Printf("local:         %d sessions (%d syncable, %d excluded)\n", st.LocalSessions, st.SyncEligible, st.Excluded)
	fmt.Printf("remote repo:   %d sessions\n", st.RemoteSessions)
	fmt.Printf("pending push:  %d\n", st.PendingPush)
	fmt.Printf("pending pull:  %d\n", st.PendingPull)
	if len(st.Diverged) > 0 {
		fmt.Printf("diverged:      %s (cites into these are not portable until re-pushed)\n", strings.Join(st.Diverged, " "))
	}
	if len(st.ConflictSiblings) > 0 {
		fmt.Printf("conflicts:     %d session(s) with sibling versions — will resolve on next push\n", len(st.ConflictSiblings))
	}
	if lastSync != "" {
		fmt.Printf("last sync:     %s\n", lastSync)
	} else {
		fmt.Println("last sync:     never")
	}
	return nil
}

// --- purge --------------------------------------------------------------

func syncPurge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync purge", flag.ExitOnError)
	localToo := fs.Bool("local", false, "also delete the session from this machine's index")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.Parse(reorderFlags(args))
	if fs.NArg() == 0 {
		return errors.New("purge requires a session ref (uid, uid prefix, or cite token)")
	}

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()

	uid, _ := parseSessionRef(fs.Arg(0))
	session, err := db.SessionByUIDAny(uid)
	if err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("session %q not found", fs.Arg(0))
	}

	if !*yes {
		fmt.Printf("Purge %s/%s %q?\n", shortAgent(session.Agent), shortUID(session.UID), session.Title)
		fmt.Print("This deletes it from the sync remote and stops all future pushes.")
		if *localToo {
			fmt.Print(" It will ALSO be deleted from this machine's index.")
		}
		fmt.Print("\nOther machines keep their local copies until they run purge --local themselves.\nProceed? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			fmt.Println("aborted")
			return nil
		}
	}

	eng, err := loadSyncEngine(db, false)
	if err != nil {
		return err
	}
	release, ok, pid := acquireLockAt(syncLockPath())
	if !ok {
		return fmt.Errorf("another sync is running (pid %d) — try again shortly", pid)
	}
	defer release()

	if err := eng.Purge(ctx, session.Agent, session.UID, *localToo); err != nil {
		return err
	}
	fmt.Printf("purged %s/%s from the remote", shortAgent(session.Agent), shortUID(session.UID))
	if *localToo {
		fmt.Print(" and this machine")
	}
	fmt.Println()
	return nil
}
