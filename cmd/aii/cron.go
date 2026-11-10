package main

// `aii cron` — install/uninstall a periodic indexer.
//
// macOS: writes a launchd LaunchAgent plist (~/Library/LaunchAgents),
// loads it, and lets launchd run `aii index --quiet` every N minutes.
// launchd is preferred over crontab on macOS because it survives sleep,
// runs at login, and reschedules cleanly.
//
// Windows: registers a Task Scheduler task via `schtasks.exe /Create`.
// Output isn't redirected — Task Scheduler keeps its own run history in
// Event Viewer and `aii cron status` surfaces the job state.
//
// Linux/other: appends a single line to the user's crontab. The line is
// tagged with a marker comment so we can find/remove it idempotently.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// exePath returns the absolute path to our own binary, suitable for
// embedding in a launchd plist or crontab line. We resolve symlinks so
// the schedule keeps working even if the user later replaces a symlink
// in ~/.local/bin/.
func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe, nil
}

// runIndexCommand runs `aii index --quiet` synchronously, used by
// `aii cron run`. Reuses the same code path as the real subcommand so
// behavior stays consistent.
func runIndexCommand() error {
	return cmdIndex(context.Background(), []string{"--quiet"})
}

func parseInterval(s string) (int, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		// Bare number → minutes, friendlier for crontab brain.
		if n, err2 := strconv.Atoi(strings.TrimSpace(s)); err2 == nil && n > 0 {
			return n * 60, nil
		}
		return 0, fmt.Errorf("invalid --every %q (try 5m, 30s, 1h)", s)
	}
	if d < time.Second {
		return 0, fmt.Errorf("interval too short: %s", d)
	}
	return int(d.Seconds()), nil
}

func timeSince(t time.Time) time.Duration { return time.Since(t) }

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Bounds for the scheduler's log file. The scheduler appends to
// cron.log on every run and nothing else ever shortens it, so the
// indexer trims it itself: once the file passes cronLogMaxBytes, the
// last cronLogKeepBytes are kept and everything older is dropped.
const (
	cronLogMaxBytes  = 8 << 20 // trim once the log passes 8 MiB
	cronLogKeepBytes = 1 << 20 // retain the last 1 MiB
)

// cronLogPath is where both the LaunchAgent and the crontab line send
// the indexer's output.
func cronLogPath() string { return filepath.Join(dataDir(), "cron.log") }

// trimCronLog keeps the scheduler's log under its size cap. It is a
// no-op when the log is missing or still small.
func trimCronLog() {
	if _, err := trimLogFile(cronLogPath(), cronLogMaxBytes, cronLogKeepBytes); err != nil {
		fmt.Fprintf(os.Stderr, "trim cron log: %v\n", err)
	}
}

// trimLogFile drops everything but the last keepBytes of path once the
// file grows past maxBytes, and reports whether it trimmed.
//
// The file is rewritten in place rather than renamed, because the
// process doing the trimming is usually the one whose stdout the
// scheduler already pointed at this file. Both launchd and a crontab
// `>>` redirect open it in append mode, so writes that follow the trim
// land at the new end of the file. Trimming starts at the first line
// boundary inside the retained tail, so the log never begins with half
// a line.
func trimLogFile(path string, maxBytes, keepBytes int64) (bool, error) {
	if keepBytes >= maxBytes {
		return false, fmt.Errorf("keepBytes (%d) must be below maxBytes (%d)", keepBytes, maxBytes)
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	size := info.Size()
	if size <= maxBytes {
		return false, nil
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()

	tail := make([]byte, keepBytes)
	if _, err := f.ReadAt(tail, size-keepBytes); err != nil {
		return false, err
	}
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}

	header := fmt.Sprintf("--- aii trimmed this log on %s: dropped %d bytes written before this point ---\n",
		time.Now().Format("2006-01-02 15:04:05"), size-int64(len(tail)))
	out := append([]byte(header), tail...)

	// Write before truncating, so a crash mid-trim leaves a longer file
	// rather than a shorter one. A crash between the two calls can still
	// leave one torn line where the new tail meets the old content; the
	// next run trims again and the log is only a log.
	if _, err := f.WriteAt(out, 0); err != nil {
		return false, err
	}
	if err := f.Truncate(int64(len(out))); err != nil {
		return false, err
	}
	return true, f.Sync()
}

const (
	launchdLabel      = "com.aii.index"
	cronTagMarker     = "# aii auto-index"
	schtasksTaskName  = "aii-index"
	schtasksMaxMinute = 1439 // schtasks /SC MINUTE /MO cap (<24h)
)

func cmdCron(args []string) error {
	if len(args) == 0 {
		return cronUsage()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return cronInstall(rest)
	case "uninstall", "remove":
		return cronUninstall()
	case "status":
		return cronStatus()
	case "run":
		// Manual trigger of the same code path the scheduler runs —
		// handy for testing the install without waiting for the
		// interval to elapse.
		return runIndexCommand()
	case "help", "-h", "--help":
		return cronUsage()
	}
	return fmt.Errorf("unknown cron subcommand %q", sub)
}

func cronUsage() error {
	fmt.Print(`aii cron — schedule background indexing

Usage:
  aii cron install   [--every 5m]
  aii cron uninstall
  aii cron status
  aii cron run        # one-off manual run (same as 'aii index --quiet')

Per-platform mechanism:
  macOS    LaunchAgent at ~/Library/LaunchAgents/com.aii.index.plist
  Windows  Task Scheduler task "aii-index" (via schtasks.exe)
  Linux    a tagged line in your user crontab
`)
	return nil
}

func cronInstall(args []string) error {
	fs := flag.NewFlagSet("cron install", flag.ExitOnError)
	every := fs.String("every", "5m", "interval — supports 30s, 5m, 1h, 1d (rounded down to seconds)")
	fs.Parse(args)

	intervalSeconds, err := parseInterval(*every)
	if err != nil {
		return err
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(exe, intervalSeconds)
	case "windows":
		return installSchtasks(exe, intervalSeconds)
	default:
		return installCrontab(exe, intervalSeconds)
	}
}

func cronUninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "windows":
		return uninstallSchtasks()
	default:
		return uninstallCrontab()
	}
}

func cronStatus() error {
	fmt.Printf("DB:    %s\n", store.DefaultPath())
	if info, err := os.Stat(stampPath()); err == nil {
		fmt.Printf("Last:  %s (%s ago)\n",
			info.ModTime().Format("2006-01-02 15:04:05"),
			humanDuration(timeSince(info.ModTime())))
	} else {
		fmt.Println("Last:  (never — no stamp file)")
	}
	if runtime.GOOS == "windows" {
		// The Task Scheduler action has no redirect, so there is no log
		// file to report; Task Scheduler keeps its own run history.
		fmt.Println("Log:   none (Task Scheduler keeps the run history; see Event Viewer)")
	} else if info, err := os.Stat(cronLogPath()); err == nil {
		fmt.Printf("Log:   %s (%.1f MiB, trimmed to %d MiB past %d MiB)\n",
			cronLogPath(), float64(info.Size())/(1<<20),
			cronLogKeepBytes/(1<<20), cronLogMaxBytes/(1<<20))
	} else {
		fmt.Printf("Log:   %s (none yet)\n", cronLogPath())
	}
	if locked, pid := indexLocked(); locked {
		fmt.Printf("Lock:  held by pid %d (indexer running now)\n", pid)
	} else {
		fmt.Println("Lock:  free")
	}
	switch runtime.GOOS {
	case "darwin":
		return statusLaunchd()
	case "windows":
		return statusSchtasks()
	default:
		return statusCrontab()
	}
}

// cronInstallStatus reports whether a scheduler entry is registered
// for aii and identifies the backend ("LaunchAgent" / "Task Scheduler"
// / "crontab") so doctor can print a clean status line. Empty second
// return value when not installed.
func cronInstallStatus() (bool, string) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := os.Stat(launchdPlistPath()); err == nil {
			return true, "LaunchAgent"
		}
		return false, ""
	case "windows":
		if err := exec.Command("schtasks", "/Query", "/TN", schtasksTaskName).Run(); err == nil {
			return true, "Task Scheduler"
		}
		return false, ""
	default:
		cur, _ := readCrontab()
		if strings.Contains(cur, cronTagMarker) {
			return true, "crontab"
		}
		return false, ""
	}
}

// --- launchd (macOS) ---------------------------------------------------

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func installLaunchd(exe string, intervalSeconds int) error {
	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	plist := buildLaunchdPlist(exe, intervalSeconds)
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	// Reload: unload first (ignore errors — may not be loaded).
	_ = exec.Command("launchctl", "unload", path).Run()
	if out, err := exec.Command("launchctl", "load", path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("installed: %s\n", path)
	fmt.Printf("interval:  every %ds\n", intervalSeconds)
	fmt.Printf("inspect:   launchctl list | grep %s\n", launchdLabel)
	return nil
}

func uninstallLaunchd() error {
	path := launchdPlistPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("not installed (no plist found)")
		return nil
	}
	if out, err := exec.Command("launchctl", "unload", path).CombinedOutput(); err != nil {
		// Unload failure isn't fatal — the plist may have already been
		// unloaded. Surface the message but keep going to delete the
		// file so re-install works cleanly.
		fmt.Fprintf(os.Stderr, "launchctl unload: %v: %s\n", err, strings.TrimSpace(string(out)))
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Println("uninstalled")
	return nil
}

func statusLaunchd() error {
	path := launchdPlistPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("Cron:  not installed (run `aii cron install`)")
		return nil
	}
	fmt.Printf("Cron:  installed at %s\n", path)
	out, err := exec.Command("launchctl", "list", launchdLabel).CombinedOutput()
	if err == nil {
		fmt.Println("Job:")
		// Prefix lines for readability.
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			fmt.Printf("  %s\n", line)
		}
	} else {
		fmt.Printf("Job:   not loaded (launchctl list: %s)\n", strings.TrimSpace(string(out)))
	}
	return nil
}

// dbEnvOverride returns the AII_DB value the scheduled job has to
// inherit, or "" when the installer is using the default DB. Everything
// about a scheduled run is derived from the DB path: which database it
// indexes, and which cron.log it writes and trims. The installer reads
// that path from AII_DB, so the job has to see the same value.
func dbEnvOverride() string { return os.Getenv("AII_DB") }

// xmlString escapes s for use inside a plist <string> element.
func xmlString(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// buildLaunchdPlist composes a minimal LaunchAgent that runs
// `aii index --quiet` every intervalSeconds. Output goes to a log file
// next to the DB so you can `tail -f` it if something looks off. When
// the installer runs with AII_DB set, the plist passes that same value
// to the job through EnvironmentVariables.
func buildLaunchdPlist(exe string, intervalSeconds int) string {
	logPath := cronLogPath()
	env := ""
	if db := dbEnvOverride(); db != "" {
		env = fmt.Sprintf(`  <key>EnvironmentVariables</key>
  <dict>
    <key>AII_DB</key>
    <string>%s</string>
  </dict>
`, xmlString(db))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>index</string>
    <string>--quiet</string>
  </array>
  <key>StartInterval</key>
  <integer>%d</integer>
  <key>RunAtLoad</key>
  <true/>
%s  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, launchdLabel, xmlString(exe), intervalSeconds, env, xmlString(logPath), xmlString(logPath))
}

// --- schtasks (Windows) ------------------------------------------------

// schtasksAction builds the command string for schtasks /TR. /TR takes
// a single command, so the exe is quoted for paths with spaces (e.g.
// "C:\Program Files\aii\aii.exe"). With a db path, the command runs
// through `cmd /c` so the task can set AII_DB first; `set "NAME=value"`
// keeps the value's spaces and trailing characters out of the variable
// name and is safe for paths that contain `&` or `(`.
func schtasksAction(exe, db string) string {
	if db == "" {
		return fmt.Sprintf(`"%s" index --quiet`, exe)
	}
	return fmt.Sprintf(`cmd /c set "AII_DB=%s" && "%s" index --quiet`, db, exe)
}

// installSchtasks registers a Task Scheduler task that runs
// `aii index --quiet` every N minutes. schtasks /SC MINUTE caps /MO at
// 1439 (<24h); longer intervals are clamped and warned about so the
// install never silently lies about cadence.
func installSchtasks(exe string, intervalSeconds int) error {
	mins := intervalSeconds / 60
	if mins < 1 {
		fmt.Fprintln(os.Stderr, "warning: schtasks can't go sub-minute; rounding to 1m")
		mins = 1
	}
	if mins > schtasksMaxMinute {
		fmt.Fprintf(os.Stderr, "warning: %dm exceeds schtasks /SC MINUTE cap; capping at %dm\n", mins, schtasksMaxMinute)
		mins = schtasksMaxMinute
	}
	action := schtasksAction(exe, dbEnvOverride())
	out, err := exec.Command("schtasks",
		"/Create", "/F",
		"/SC", "MINUTE",
		"/MO", strconv.Itoa(mins),
		"/TN", schtasksTaskName,
		"/TR", action,
		"/RL", "LIMITED",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("installed: Task Scheduler task %q\n", schtasksTaskName)
	fmt.Printf("interval:  every %d minute(s)\n", mins)
	fmt.Printf("inspect:   schtasks /Query /TN %s /V /FO LIST\n", schtasksTaskName)
	return nil
}

func uninstallSchtasks() error {
	out, err := exec.Command("schtasks", "/Delete", "/F", "/TN", schtasksTaskName).CombinedOutput()
	if err != nil {
		// Most common cause: task doesn't exist. Print the schtasks
		// message verbatim rather than guessing — it's localized and
		// self-explanatory.
		fmt.Printf("not installed (or uninstall failed): %s\n", strings.TrimSpace(string(out)))
		return nil
	}
	fmt.Println("uninstalled")
	return nil
}

func statusSchtasks() error {
	out, err := exec.Command("schtasks", "/Query", "/TN", schtasksTaskName, "/V", "/FO", "LIST").CombinedOutput()
	if err != nil {
		fmt.Println("Cron:  not installed (run `aii cron install`)")
		return nil
	}
	fmt.Printf("Cron:  installed as Task Scheduler task %q\n", schtasksTaskName)
	fmt.Println("Job:")
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		fmt.Printf("  %s\n", line)
	}
	return nil
}

// --- crontab (non-darwin, non-windows) ---------------------------------

func installCrontab(exe string, intervalSeconds int) error {
	cur, err := readCrontab()
	if err != nil {
		return err
	}
	cleaned := stripAiiCronLines(cur)
	line, err := cronLine(exe, intervalSeconds)
	if err != nil {
		return err
	}
	next := cleaned
	if !strings.HasSuffix(next, "\n") && next != "" {
		next += "\n"
	}
	next += line + " " + cronTagMarker + "\n"
	if err := writeCrontab(next); err != nil {
		return err
	}
	fmt.Printf("installed cron line:\n  %s\n", line)
	return nil
}

func uninstallCrontab() error {
	cur, err := readCrontab()
	if err != nil {
		return err
	}
	cleaned := stripAiiCronLines(cur)
	if cleaned == cur {
		fmt.Println("not installed (no aii line in crontab)")
		return nil
	}
	if err := writeCrontab(cleaned); err != nil {
		return err
	}
	fmt.Println("uninstalled")
	return nil
}

func statusCrontab() error {
	cur, _ := readCrontab()
	for _, line := range strings.Split(cur, "\n") {
		if strings.Contains(line, cronTagMarker) {
			fmt.Printf("Cron:  installed in crontab\n  %s\n", line)
			return nil
		}
	}
	fmt.Println("Cron:  not installed (run `aii cron install`)")
	return nil
}

func readCrontab() (string, error) {
	out, err := exec.Command("crontab", "-l").Output()
	if err != nil {
		// `crontab -l` exits 1 when there's no crontab — treat as empty.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", nil
		}
		return "", err
	}
	return string(out), nil
}

func writeCrontab(contents string) error {
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(contents)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("crontab -: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func stripAiiCronLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, ln := range lines {
		if strings.Contains(ln, cronTagMarker) {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// shellQuote wraps s in single quotes for /bin/sh, which is what cron
// runs the command with. A `%` in a crontab command means newline, so
// it is escaped too.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "'", `'\''`), "%", `\%`) + "'"
}

// cronEnvPrefix returns the shell assignment that hands the job the
// installer's AII_DB, with a trailing space, or "" on a default DB.
func cronEnvPrefix() string {
	db := dbEnvOverride()
	if db == "" {
		return ""
	}
	return "AII_DB=" + shellQuote(db) + " "
}

// cronLine produces a 5-field crontab spec (m h dom mon dow) that runs
// the indexer at the requested interval. Sub-minute intervals can't be
// expressed in classic cron, so we round up to 1 minute and warn.
func cronLine(exe string, intervalSeconds int) (string, error) {
	mins := intervalSeconds / 60
	if mins < 1 {
		fmt.Fprintln(os.Stderr, "warning: cron can't go sub-minute; rounding to */1 * * * *")
		mins = 1
	}
	logPath := cronLogPath()
	env := cronEnvPrefix()
	if mins == 1 {
		return fmt.Sprintf("* * * * * %s%s index --quiet >> %s 2>&1", env, exe, logPath), nil
	}
	if 60%mins == 0 {
		return fmt.Sprintf("*/%d * * * * %s%s index --quiet >> %s 2>&1", mins, env, exe, logPath), nil
	}
	// Non-divisor of 60 (e.g. 7m): use closest divisor that doesn't lie
	// about cadence. Cron's */N skips the 60-min boundary, which is fine
	// for "occasional indexing" — pick the nearest divisor.
	chosen := nearestDivisor(60, mins)
	fmt.Fprintf(os.Stderr, "warning: %dm doesn't divide 60 — using */%d for an even cadence\n", mins, chosen)
	return fmt.Sprintf("*/%d * * * * %s%s index --quiet >> %s 2>&1", chosen, env, exe, logPath), nil
}

func nearestDivisor(n, target int) int {
	best := 1
	bestDiff := target - 1
	for d := 1; d <= n; d++ {
		if n%d != 0 {
			continue
		}
		diff := d - target
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			best = d
			bestDiff = diff
		}
	}
	return best
}
