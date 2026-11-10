package main

// The scheduled job has to index the same database as the install that
// created it: the installer reads AII_DB, and the plist, the crontab
// line, and the Task Scheduler action all have to pass that value on.

import (
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wellFormedXML fails the test when s is not parseable XML, so an
// escaping bug in a DB path can't ship a plist launchd refuses to load.
func wellFormedXML(t *testing.T, s string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, s)
		}
	}
}

func TestBuildLaunchdPlistPassesAIIDB(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "aii.db")
	t.Setenv("AII_DB", db)

	plist := buildLaunchdPlist("/opt/aii/aii", 300)
	wellFormedXML(t, plist)

	want := "  <key>EnvironmentVariables</key>\n  <dict>\n    <key>AII_DB</key>\n    <string>" + db + "</string>\n  </dict>\n"
	if !strings.Contains(plist, want) {
		t.Errorf("plist does not pass AII_DB to the job; want\n%s\ngot\n%s", want, plist)
	}
	// The log the job writes and trims is the one next to that DB.
	if logPath := filepath.Join(dir, "cron.log"); !strings.Contains(plist, "<string>"+logPath+"</string>") {
		t.Errorf("plist log path = not %s\n%s", logPath, plist)
	}
}

func TestBuildLaunchdPlistOmitsAIIDBOnDefaultDB(t *testing.T) {
	t.Setenv("AII_DB", "")

	plist := buildLaunchdPlist("/opt/aii/aii", 300)
	wellFormedXML(t, plist)
	if strings.Contains(plist, "EnvironmentVariables") || strings.Contains(plist, "AII_DB") {
		t.Errorf("plist should set no environment on the default DB:\n%s", plist)
	}
}

func TestBuildLaunchdPlistEscapesDBPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), `a&b<c>"d"`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AII_DB", filepath.Join(dir, "aii.db"))

	plist := buildLaunchdPlist("/opt/aii/aii", 300)
	wellFormedXML(t, plist)
	if strings.Contains(plist, "a&b") {
		t.Errorf("`&` in the DB path should be escaped:\n%s", plist)
	}
	if !strings.Contains(plist, "a&amp;b&lt;c&gt;") {
		t.Errorf("escaped DB path missing:\n%s", plist)
	}
}

func TestCronLinePassesAIIDB(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "my db's.sqlite")
	t.Setenv("AII_DB", db)

	line, err := cronLine("/opt/aii/aii", 300)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, `*/5 * * * * AII_DB='`) {
		t.Errorf("cron line should set AII_DB before the command: %s", line)
	}
	if !strings.Contains(line, `' /opt/aii/aii index --quiet >> `+filepath.Join(dir, "cron.log")) {
		t.Errorf("cron line command or log path wrong: %s", line)
	}

	// The quoting has to survive the shell cron runs the command with:
	// stand in for the binary with a script that records what AII_DB
	// the job actually sees.
	if runtime.GOOS == "windows" {
		return
	}
	seenPath := filepath.Join(dir, "seen")
	script := filepath.Join(dir, "fake-aii")
	body := "#!/bin/sh\nprintf '%s' \"$AII_DB\" > " + seenPath + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	line, err = cronLine(script, 300)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.SplitN(line, " ", 6)
	if len(fields) != 6 {
		t.Fatalf("cron line has fewer than 5 schedule fields: %s", line)
	}
	if cmdOut, err := exec.Command("/bin/sh", "-c", fields[5]).CombinedOutput(); err != nil {
		t.Fatalf("run cron command %q: %v: %s", fields[5], err, cmdOut)
	}
	seen, err := os.ReadFile(seenPath)
	if err != nil {
		t.Fatalf("job wrote no AII_DB: %v", err)
	}
	if string(seen) != db {
		t.Errorf("job saw AII_DB = %q, want %q", seen, db)
	}
}

func TestCronLineOmitsAIIDBOnDefaultDB(t *testing.T) {
	t.Setenv("AII_DB", "")

	line, err := cronLine("/opt/aii/aii", 300)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "AII_DB") {
		t.Errorf("cron line should set no environment on the default DB: %s", line)
	}
	if !strings.HasPrefix(line, "*/5 * * * * /opt/aii/aii index --quiet >> ") {
		t.Errorf("unexpected cron line: %s", line)
	}
}

func TestShellQuoteEscapesCronSpecials(t *testing.T) {
	// `%` means newline in a crontab command, and a single quote ends
	// the quoted string.
	if got, want := shellQuote(`/tmp/100%/o'neil.db`), `'/tmp/100\%/o'\''neil.db'`; got != want {
		t.Errorf("shellQuote = %s, want %s", got, want)
	}
}

func TestSchtasksAction(t *testing.T) {
	const exe = `C:\Program Files\aii\aii.exe`
	if got, want := schtasksAction(exe, ""), `"C:\Program Files\aii\aii.exe" index --quiet`; got != want {
		t.Errorf("default-DB action = %s, want %s", got, want)
	}
	got := schtasksAction(exe, `D:\data\my db\aii.db`)
	want := `cmd /c set "AII_DB=D:\data\my db\aii.db" && "C:\Program Files\aii\aii.exe" index --quiet`
	if got != want {
		t.Errorf("action = %s, want %s", got, want)
	}
}
