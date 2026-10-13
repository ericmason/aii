package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// uuidV7 builds a UUIDv7-shaped id from a millisecond timestamp and a
// random tail, matching what Codex writes for rollout ids.
func uuidV7(ms int64, rand string) string {
	h := fmt.Sprintf("%012x", ms)
	return h[:8] + "-" + h[8:] + "-7" + rand[:3] + "-8" + rand[3:6] + "-" + rand[6:18]
}

func seedUID(t *testing.T, db *DB, agent, uid, title string, started int64) {
	t.Helper()
	seed(t, db, &Session{
		Agent: agent, UID: uid, Title: title, StartedAt: started,
		SourcePath: "/tmp/" + uid + ".jsonl",
	}, nil)
}

func TestSessionByUIDAny_AmbiguousPrefixListsCandidates(t *testing.T) {
	db := openTestDB(t)
	a := uuidV7(1760000000000, "aaaaaaaaaaaaaaaaaa")
	b := uuidV7(1760000001000, "bbbbbbbbbbbbbbbbbb")
	if a[:8] != b[:8] {
		t.Fatalf("fixture ids should share an 8-char prefix: %s %s", a, b)
	}
	seedUID(t, db, "codex", a, "first session", 1760000000)
	seedUID(t, db, "codex", b, "second session", 1760000001)

	s, err := db.SessionByUIDAny(a[:8])
	if s != nil {
		t.Fatalf("ambiguous prefix returned a session (%s); want none", s.UID)
	}
	var amb *AmbiguousUIDError
	if !errors.As(err, &amb) {
		t.Fatalf("want *AmbiguousUIDError, got %v", err)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(amb.Candidates))
	}
	msg := err.Error()
	for _, want := range []string{"first session", "second session", "codex/" + db.CiteUID(a), "codex/" + db.CiteUID(b)} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

func TestCiteUID_UniqueForV7IDsOneSecondApart(t *testing.T) {
	db := openTestDB(t)
	a := uuidV7(1760000000000, "aaaaaaaaaaaaaaaaaa")
	b := uuidV7(1760000001000, "bbbbbbbbbbbbbbbbbb")
	seedUID(t, db, "codex", a, "first", 1)
	seedUID(t, db, "codex", b, "second", 2)

	ca, cb := db.CiteUID(a), db.CiteUID(b)
	if ca == cb {
		t.Fatalf("cite uids collide: %s", ca)
	}
	for uid, cite := range map[string]string{a: ca, b: cb} {
		if len(cite) < 8 || !strings.HasPrefix(uid, cite) {
			t.Fatalf("cite %q is not a >=8-char prefix of %q", cite, uid)
		}
		if strings.HasSuffix(cite, "-") {
			t.Errorf("cite %q ends with a hyphen", cite)
		}
		s, err := db.SessionByUIDAny(cite)
		if err != nil || s == nil || s.UID != uid {
			t.Fatalf("SessionByUIDAny(%q) = %v, %v; want %s", cite, s, err, uid)
		}
	}
}

func TestSessionByUIDAny_OldEightCharTokenResolvesWhenUnique(t *testing.T) {
	db := openTestDB(t)
	a := "32e869ac-1111-4222-8333-444455556666"
	b := "9f00aa11-1111-4222-8333-444455556666"
	seedUID(t, db, "claudecode", a, "a", 1)
	seedUID(t, db, "codex", b, "b", 2)

	s, err := db.SessionByUIDAny("32e869ac")
	if err != nil || s == nil || s.UID != a {
		t.Fatalf("SessionByUIDAny(32e869ac) = %v, %v; want %s", s, err, a)
	}
	if got := db.CiteUID(a); got != "32e869ac" {
		t.Errorf("CiteUID for a unique id = %q; want the 8-char prefix", got)
	}
}

func TestSessionByUIDAny_FullIDResolves(t *testing.T) {
	db := openTestDB(t)
	a := uuidV7(1760000000000, "aaaaaaaaaaaaaaaaaa")
	b := uuidV7(1760000001000, "bbbbbbbbbbbbbbbbbb")
	seedUID(t, db, "codex", a, "first", 1)
	seedUID(t, db, "codex", b, "second", 2)

	for _, uid := range []string{a, b} {
		s, err := db.SessionByUIDAny(uid)
		if err != nil || s == nil || s.UID != uid {
			t.Fatalf("SessionByUIDAny(%q) = %v, %v", uid, s, err)
		}
	}
	// A full id that is also a prefix of another id is an exact match.
	seedUID(t, db, "cursor", "abcdef01", "short", 3)
	seedUID(t, db, "cursor", "abcdef01-2", "longer", 4)
	s, err := db.SessionByUIDAny("abcdef01")
	if err != nil || s == nil || s.UID != "abcdef01" {
		t.Fatalf("exact id lost to prefix match: %v, %v", s, err)
	}
	if s, err := db.SessionByUIDAny("nope-nope"); s != nil || err != nil {
		t.Fatalf("missing id = %v, %v; want nil, nil", s, err)
	}
}

func TestSessionByRef_AgentBreaksTieOnSharedFullID(t *testing.T) {
	db := openTestDB(t)
	uid := "5e5e5e5e-1111-4222-8333-444455556666"
	seedUID(t, db, "codex", uid, "codex copy", 1)
	seedUID(t, db, "claude_code", uid, "claude copy", 2)

	var amb *AmbiguousUIDError
	if _, err := db.SessionByUIDAny(uid); !errors.As(err, &amb) {
		t.Fatalf("unscoped lookup of a shared id = %v; want *AmbiguousUIDError", err)
	}
	cite := db.CiteUID(uid)
	for _, agent := range []string{"codex", "claude_code"} {
		for _, ref := range []string{uid, cite, uid[:8]} {
			s, err := db.SessionByRef(agent, ref)
			if err != nil || s == nil || s.Agent != agent || s.UID != uid {
				t.Fatalf("SessionByRef(%q, %q) = %v, %v; want the %s session", agent, ref, s, err, agent)
			}
		}
	}
	// An agent with no match falls back to the unscoped lookup.
	other := "7a7a7a7a-1111-4222-8333-444455556666"
	seedUID(t, db, "cursor", other, "cursor", 3)
	if s, err := db.SessionByRef("codex", other[:8]); err != nil || s == nil || s.UID != other {
		t.Fatalf("SessionByRef fallback = %v, %v; want %s", s, err, other)
	}
}

func TestSessionByUIDAny_CaseInsensitive(t *testing.T) {
	db := openTestDB(t)
	upper := "ABCDEF12-1111-4222-8333-444455556666"
	lower := "0123abcd-1111-4222-8333-444455556666"
	seedUID(t, db, "copilot", upper, "upper", 1)
	seedUID(t, db, "codex", lower, "lower", 2)

	for ref, want := range map[string]string{
		"abcdef12":             upper,
		"AbCdEf12-1111":        upper,
		strings.ToLower(upper): upper,
		"0123ABCD":             lower,
		strings.ToUpper(lower): lower,
	} {
		s, err := db.SessionByUIDAny(ref)
		if err != nil || s == nil || s.UID != want {
			t.Errorf("SessionByUIDAny(%q) = %v, %v; want %s", ref, s, err, want)
		}
	}
}

func TestCiteUID_UniqueUnderCaseFold(t *testing.T) {
	db := openTestDB(t)
	a := "abcdef12-aaaa-4222-8333-444455556666"
	b := "ABCDEF12-bbbb-4222-8333-444455556666"
	seedUID(t, db, "codex", a, "a", 1)
	seedUID(t, db, "copilot", b, "b", 2)

	ca, cb := db.CiteUID(a), db.CiteUID(b)
	if strings.EqualFold(ca, cb) {
		t.Fatalf("cite uids collide under case folding: %s %s", ca, cb)
	}
	for uid, cite := range map[string]string{a: ca, b: cb} {
		s, err := db.SessionByUIDAny(cite)
		if err != nil || s == nil || s.UID != uid {
			t.Fatalf("SessionByUIDAny(%q) = %v, %v; want %s", cite, s, err, uid)
		}
	}
}
