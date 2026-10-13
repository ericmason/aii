package opencode

import (
	"context"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
)

func TestReadOnlyURI(t *testing.T) {
	cases := map[string]string{
		`C:\Users\dev\.local\share\opencode\opencode.db`: "file:///C:/Users/dev/.local/share/opencode/opencode.db?mode=ro&_pragma=busy_timeout(5000)",
		`d:/data/opencode.db`:                            "file:///d:/data/opencode.db?mode=ro&_pragma=busy_timeout(5000)",
		"/home/dev/my data/opencode.db":                  "file:///home/dev/my%20data/opencode.db?mode=ro&_pragma=busy_timeout(5000)",
	}
	for in, want := range cases {
		if got := readOnlyURI(in); got != want {
			t.Errorf("readOnlyURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// The same session id in two databases (stable and dev channel) keeps
// separate index state, so neither is re-read on every run.
func TestTwoDatabasesKeepSeparateState(t *testing.T) {
	var srcs []source.Source
	for i, title := range []string{"Stable copy", "Dev copy"} {
		path, w := fixture(t, v1DDL)
		exec(t, w, "INSERT INTO session VALUES (?, 'prj_1', NULL, 'calm-otter', '/work/demo-app', ?, '1.4.2', NULL, NULL, NULL, NULL, NULL, NULL, NULL, 1772359200000, ?, NULL, NULL, NULL)", sid, title, 1772359260000+int64(i))
		exec(t, w, "INSERT INTO message VALUES ('msg_a', ?, 1772359201000, 1772359201000, '{\"role\":\"user\"}')", sid)
		exec(t, w, "INSERT INTO part VALUES ('prt_a1', 'msg_a', ?, 1772359201000, 1772359201000, '{\"type\":\"text\",\"text\":\"Hello\"}')", sid)
		srcs = append(srcs, &Source{DBPath: path})
	}
	db := openDB(t)
	for _, s := range srcs {
		runIndex(t, db, s, false)
	}
	for _, s := range srcs {
		st, err := indexer.Run(context.Background(), db, []source.Source{s}, indexer.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if st.SessionsTouch != 0 {
			t.Fatalf("unchanged database re-read %d sessions", st.SessionsTouch)
		}
	}
}
