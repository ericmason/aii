package cloudsync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyHierarchyDeterministic(t *testing.T) {
	k, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	k2 := keysFromIdentity(k.identity)
	if !bytes.Equal(k.NameKey, k2.NameKey) || !bytes.Equal(k.BundleKey, k2.BundleKey) || !bytes.Equal(k.RepoKey, k2.RepoKey) {
		t.Fatal("subkey derivation not deterministic")
	}
	if k.Fingerprint != k2.Fingerprint {
		t.Fatal("fingerprint not deterministic")
	}
	// Subkeys must be pairwise distinct (distinct HKDF info strings).
	if bytes.Equal(k.NameKey, k.BundleKey) || bytes.Equal(k.NameKey, k.RepoKey) || bytes.Equal(k.BundleKey, k.RepoKey) {
		t.Fatal("subkeys not distinct")
	}
	if len(k.NameKey) != 32 || len(k.BundleKey) != 32 || len(k.RepoKey) != 32 {
		t.Fatal("subkeys must be 32 bytes")
	}
}

func TestBundleNameStable(t *testing.T) {
	k, _ := GenerateKeys()
	a := k.BundleName("claude_code", "32e869ac-1111-2222-3333-444444444444")
	b := k.BundleName("claude_code", "32e869ac-1111-2222-3333-444444444444")
	if a != b {
		t.Fatal("BundleName not deterministic")
	}
	if len(a) != 32 {
		t.Fatalf("BundleName length = %d, want 32", len(a))
	}
	if k.BundleName("codex", "32e869ac-1111-2222-3333-444444444444") == a {
		t.Fatal("agent must be part of the name derivation")
	}
	k2, _ := GenerateKeys()
	if k2.BundleName("claude_code", "32e869ac-1111-2222-3333-444444444444") == a {
		t.Fatal("different repos must produce different names")
	}
}

func TestKeyFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sync", "key.json")
	k, _ := GenerateKeys()

	if err := SaveKeyFile(path, k); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perms = %o, want 600", perm)
	}

	got, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != k.Fingerprint || !bytes.Equal(got.BundleKey, k.BundleKey) {
		t.Fatal("loaded keys differ from saved")
	}

	// Tampered fingerprint must be rejected.
	b, _ := os.ReadFile(path)
	tampered := strings.Replace(string(b), k.Fingerprint[:8], "deadbeef", 1)
	if tampered == string(b) {
		t.Fatal("test setup: fingerprint prefix not found")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyFile(path); err == nil {
		t.Fatal("tampered key file loaded without error")
	}
}

func TestWrapUnwrap(t *testing.T) {
	if testing.Short() {
		t.Skip("scrypt work factor makes this slow")
	}
	k, _ := GenerateKeys()

	if _, err := WrapKeys(k, "short"); err == nil {
		t.Fatal("weak passphrase accepted")
	}

	wrapped, err := WrapKeys(k, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKeys(wrapped, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != k.Fingerprint || !bytes.Equal(got.NameKey, k.NameKey) {
		t.Fatal("unwrapped keys differ")
	}

	if _, err := UnwrapKeys(wrapped, "wrong passphrase entirely"); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
}
