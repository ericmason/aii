package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLines fills path with n numbered lines and returns its size.
func writeLines(t *testing.T, path string, n int) int64 {
	t.Helper()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %06d: padding padding padding padding padding\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestTrimLogFileLeavesSmallFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.log")
	size := writeLines(t, path, 10)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	trimmed, err := trimLogFile(path, size+1, size/2)
	if err != nil {
		t.Fatal(err)
	}
	if trimmed {
		t.Fatal("trimmed a file that is still under the cap")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("file contents changed")
	}
}

func TestTrimLogFileKeepsTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.log")
	writeLines(t, path, 5000)

	const maxBytes, keepBytes = 40 << 10, 8 << 10
	trimmed, err := trimLogFile(path, maxBytes, keepBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !trimmed {
		t.Fatal("did not trim a file over the cap")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > keepBytes+512 {
		t.Fatalf("trimmed file is %d bytes, want at most about %d", len(data), keepBytes)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "--- aii trimmed this log") {
		t.Fatalf("first line is %q, want the trim marker", lines[0])
	}
	// Every retained line is whole, and the newest one survived.
	for _, ln := range lines[1:] {
		if !strings.HasPrefix(ln, "line ") {
			t.Fatalf("retained a partial line: %q", ln)
		}
	}
	if want := "line 004999:"; !strings.Contains(lines[len(lines)-1], want) {
		t.Fatalf("last line is %q, want it to contain %q", lines[len(lines)-1], want)
	}
}

func TestTrimLogFileAppendsAfterTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.log")
	writeLines(t, path, 5000)

	// A scheduled run holds this file open in append mode while the
	// trim happens, which is how launchd and a crontab `>>` redirect
	// both open it. The next write must land at the new end.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := trimLogFile(path, 40<<10, 8<<10); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("after the trim\n"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 9<<10 {
		t.Fatalf("file grew to %d bytes after appending, want it to stay near the retained tail", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "after the trim\n") {
		t.Fatal("the appended line is not at the end of the file")
	}
}

func TestTrimLogFileMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.log")
	trimmed, err := trimLogFile(path, 1<<20, 1<<10)
	if err != nil {
		t.Fatalf("missing log should not be an error: %v", err)
	}
	if trimmed {
		t.Fatal("reported a trim of a file that does not exist")
	}
}

func TestTrimLogFileRejectsBadBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.log")
	writeLines(t, path, 10)
	if _, err := trimLogFile(path, 1<<10, 1<<10); err == nil {
		t.Fatal("want an error when keepBytes is not below maxBytes")
	}
}

func TestCronLogBoundsAreConsistent(t *testing.T) {
	if cronLogKeepBytes >= cronLogMaxBytes {
		t.Fatalf("cronLogKeepBytes (%d) must be below cronLogMaxBytes (%d)", cronLogKeepBytes, cronLogMaxBytes)
	}
}
