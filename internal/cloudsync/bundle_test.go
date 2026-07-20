package cloudsync

import (
	"encoding/hex"
	"strings"
	"testing"
)

func testBundle(t *testing.T, k *Keys) (*Bundle, string) {
	t.Helper()
	b := &Bundle{
		Format:     FormatVersion,
		Agent:      "claude_code",
		SessionUID: "32e869ac-0000-0000-0000-000000000000",
		Workspace:  "/home/u/proj",
		Title:      "fix webhook retry",
		StartedAt:  1700000000,
		EndedAt:    1700000600,
		Epoch:      1,
		Messages: []BundleMessage{
			{Ordinal: 0, Role: "user", TS: 1700000000, Content: "how do I retry webhooks"},
			{Ordinal: 1, Role: "assistant", TS: 1700000100, Content: "use exponential backoff"},
			{Ordinal: 2, Role: "tool", Content: "exit 0"}, // ts=0 (cursor-style)
		},
	}
	head := computeChain(b.Agent, b.SessionUID, b.rows())
	b.ChainHead = hex.EncodeToString(head[:])
	key := versionKey(k.BundleName(b.Agent, b.SessionUID), b.Epoch, len(b.Messages), head)
	return b, key
}

func TestBundleRoundtrip(t *testing.T) {
	k, _ := GenerateKeys()
	b, key := testBundle(t, k)

	enc, err := EncodeBundle(b, k, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBundle(enc, k, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionUID != b.SessionUID || got.Title != b.Title || len(got.Messages) != 3 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Messages[2].TS != 0 {
		t.Fatal("zero ts not preserved")
	}
}

func TestBundleMACFailsClosed(t *testing.T) {
	k, _ := GenerateKeys()
	b, key := testBundle(t, k)
	enc, err := EncodeBundle(b, k, key)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("bit-flip", func(t *testing.T) {
		bad := append([]byte(nil), enc...)
		bad[len(bad)/2] ^= 1
		if _, err := DecodeBundle(bad, k, key); err == nil || !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("tampered bundle decoded: %v", err)
		}
	})

	t.Run("renamed-object", func(t *testing.T) {
		// Same bytes fetched under a different version key (replay
		// into another slot) must fail before decrypting.
		head := computeChain(b.Agent, b.SessionUID, b.rows())
		otherKey := versionKey(k.BundleName(b.Agent, b.SessionUID), 2, len(b.Messages), head)
		if _, err := DecodeBundle(enc, k, otherKey); err == nil || !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("renamed bundle decoded: %v", err)
		}
	})

	t.Run("cross-repo", func(t *testing.T) {
		k2, _ := GenerateKeys()
		if _, err := DecodeBundle(enc, k2, key); err == nil || !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("cross-repo bundle decoded: %v", err)
		}
	})
}

func TestBundleInteriorConsistency(t *testing.T) {
	k, _ := GenerateKeys()

	t.Run("wrong-declared-head", func(t *testing.T) {
		b, _ := testBundle(t, k)
		b.ChainHead = strings.Repeat("00", 32)
		head := computeChain(b.Agent, b.SessionUID, b.rows())
		key := versionKey(k.BundleName(b.Agent, b.SessionUID), b.Epoch, len(b.Messages), head)
		enc, err := EncodeBundle(b, k, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeBundle(enc, k, key); err == nil {
			t.Fatal("bundle with lying chain head decoded")
		}
	})

	t.Run("count-mismatch", func(t *testing.T) {
		b, _ := testBundle(t, k)
		head := computeChain(b.Agent, b.SessionUID, b.rows())
		key := versionKey(k.BundleName(b.Agent, b.SessionUID), b.Epoch, 99, head)
		enc, err := EncodeBundle(b, k, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeBundle(enc, k, key); err == nil {
			t.Fatal("bundle with count mismatch decoded")
		}
	})

	t.Run("gapped-ordinals", func(t *testing.T) {
		b, _ := testBundle(t, k)
		b.Messages[2].Ordinal = 7
		head := computeChain(b.Agent, b.SessionUID, b.rows())
		b.ChainHead = hex.EncodeToString(head[:])
		key := versionKey(k.BundleName(b.Agent, b.SessionUID), b.Epoch, len(b.Messages), head)
		enc, err := EncodeBundle(b, k, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeBundle(enc, k, key); err == nil {
			t.Fatal("bundle with gapped ordinals decoded")
		}
	})
}

func TestVersionKeyGrammar(t *testing.T) {
	name := strings.Repeat("ab", 16)
	good := "bundles/" + name + "/3-120-cafebabe.age"
	v, ok := parseVersionKey(good)
	if !ok || v.Epoch != 3 || v.Count != 120 || v.Head8 != "cafebabe" || v.Name != name {
		t.Fatalf("parseVersionKey(%q) = %+v, %v", good, v, ok)
	}
	bad := []string{
		"bundles/" + name + "/3-120-cafebabe.age (conflicted copy)",
		"bundles/" + name + "/3-120-cafebabe (Eric's conflicted copy 2026-07-19).age",
		"bundles/" + name + "/purged",
		"bundles/short/1-1-cafebabe.age",
		"bundles/" + name + "/0-1-cafebabe.age",  // epoch 0 invalid
		"bundles/" + name + "/1-0-cafebabe.age",  // count 0 invalid
		"bundles/" + name + "/1-1-CAFEBABE.age",  // uppercase hex invalid
		"keys/master.age",
		"aii-sync.json",
	}
	for _, b := range bad {
		if _, ok := parseVersionKey(b); ok {
			t.Errorf("parseVersionKey(%q) accepted, want reject", b)
		}
	}

	if n, ok := parseTombstoneKey("bundles/" + name + "/purged"); !ok || n != name {
		t.Fatal("tombstone key not parsed")
	}
	if _, ok := parseTombstoneKey("bundles/" + name + "/1-1-cafebabe.age"); ok {
		t.Fatal("version key parsed as tombstone")
	}
}

func TestTombstoneAuth(t *testing.T) {
	k, _ := GenerateKeys()
	name := k.BundleName("claude_code", "uid-1")
	body := tombstoneBody(k, name)
	if !verifyTombstone(k, name, body) {
		t.Fatal("valid tombstone rejected")
	}
	if verifyTombstone(k, name, []byte("forged")) {
		t.Fatal("forged tombstone accepted")
	}
	// A tombstone for one name must not verify for another.
	other := k.BundleName("claude_code", "uid-2")
	if verifyTombstone(k, other, body) {
		t.Fatal("tombstone transplanted between names")
	}
	// A tombstone from a different repo must not verify.
	k2, _ := GenerateKeys()
	if verifyTombstone(k2, name, body) {
		t.Fatal("cross-repo tombstone accepted")
	}
}
