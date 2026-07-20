// Package cloudsync implements aii's end-to-end-encrypted session
// sync: one age-encrypted, HMAC-authenticated bundle per session,
// pushed to dumb bring-your-own storage (S3-compatible or a synced
// directory) and merged back with hash-chain prefix consistency.
package cloudsync

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"golang.org/x/crypto/hkdf"
)

// scryptWorkFactor is the log2(N) cost for the passphrase-wrapped
// master key. age's default is 18; we go one notch up (~512 MB, a few
// seconds) because unwrapping only happens at interactive join time,
// while an attacker with bucket read access gets to brute-force the
// passphrase offline forever.
const scryptWorkFactor = 19

// MinPassphraseLen is the floor enforced at init. The passphrase is
// the only thing standing between a bucket reader and the corpus.
const MinPassphraseLen = 10

const hkdfSalt = "aii-sync/v1"

// Keys is the full key hierarchy, all derived from one age X25519
// identity so the wrapped identity alone reconstitutes everything on
// join. Subkeys are HKDF-SHA256 expansions with distinct info strings
// — the raw identity is never used directly as a MAC key.
type Keys struct {
	identity *age.X25519Identity

	NameKey   []byte // pseudonymous object naming
	BundleKey []byte // bundle + purge-tombstone authentication
	RepoKey   []byte // repo-id marker authentication

	// Fingerprint is the hex SHA-256 of the recipient (public key)
	// string. Pinned locally at init/join and re-checked on every
	// load so a swapped keyfile or repo re-key is detected.
	Fingerprint string
}

func GenerateKeys() (*Keys, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	return keysFromIdentity(id), nil
}

func keysFromIdentity(id *age.X25519Identity) *Keys {
	k := &Keys{identity: id}
	// The identity's canonical string encoding is a deterministic
	// bijection of the scalar — equivalent input keying material,
	// without reaching into age's internals.
	secret := []byte(id.String())
	for _, sub := range []struct {
		info string
		dst  *[]byte
	}{
		{"naming", &k.NameKey},
		{"bundle-mac", &k.BundleKey},
		{"repo-mac", &k.RepoKey},
	} {
		buf := make([]byte, 32)
		r := hkdf.New(sha256.New, secret, []byte(hkdfSalt), []byte(sub.info))
		if _, err := io.ReadFull(r, buf); err != nil {
			panic("hkdf: " + err.Error()) // cannot fail for SHA-256/32B
		}
		*sub.dst = buf
	}
	sum := sha256.Sum256([]byte(id.Recipient().String()))
	k.Fingerprint = hex.EncodeToString(sum[:])
	return k
}

func (k *Keys) Recipient() age.Recipient    { return k.identity.Recipient() }
func (k *Keys) Identity() *age.X25519Identity { return k.identity }

// BundleName is the pseudonymous remote directory name for a session:
// hex(HMAC(NameKey, agent||0x00||uid))[:32]. Deterministic per repo,
// meaningless to anyone without the master key.
func (k *Keys) BundleName(agent, uid string) string {
	m := hmac.New(sha256.New, k.NameKey)
	m.Write([]byte(agent))
	m.Write([]byte{0})
	m.Write([]byte(uid))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

type keyFile struct {
	Version     int    `json:"version"`
	Identity    string `json:"identity"`
	Fingerprint string `json:"fingerprint"`
}

// SaveKeyFile writes the identity to path with 0600 perms (0700 dir).
func SaveKeyFile(path string, k *Keys) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(keyFile{Version: 1, Identity: k.identity.String(), Fingerprint: k.Fingerprint}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// LoadKeyFile reads the identity and re-derives the whole hierarchy,
// verifying the stored fingerprint still matches the identity — a
// mismatch means the keyfile was swapped or corrupted.
func LoadKeyFile(path string) (*Keys, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if kf.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported key file version %d (upgrade aii)", path, kf.Version)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(kf.Identity))
	if err != nil {
		return nil, fmt.Errorf("parse identity in %s: %w", path, err)
	}
	k := keysFromIdentity(id)
	if kf.Fingerprint != "" && kf.Fingerprint != k.Fingerprint {
		return nil, fmt.Errorf("%s: stored fingerprint does not match identity — key file corrupted or tampered with", path)
	}
	return k, nil
}

// WrapKeys encrypts the keyfile JSON to a passphrase (age scrypt
// recipient). This is the only form the master key takes on the
// remote.
func WrapKeys(k *Keys, passphrase string) ([]byte, error) {
	if len(passphrase) < MinPassphraseLen {
		return nil, fmt.Errorf("passphrase must be at least %d characters — it is the only protection for your synced history", MinPassphraseLen)
	}
	rec, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, err
	}
	rec.SetWorkFactor(scryptWorkFactor)
	inner, err := json.Marshal(keyFile{Version: 1, Identity: k.identity.String(), Fingerprint: k.Fingerprint})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rec)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(inner); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnwrapKeys decrypts a wrapped master key with the passphrase.
func UnwrapKeys(data []byte, passphrase string) (*Keys, error) {
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		var badAge *age.NoIdentityMatchError
		if errors.As(err, &badAge) {
			return nil, errors.New("could not unlock the sync key: wrong passphrase, or the remote key object was tampered with")
		}
		return nil, fmt.Errorf("could not unlock the sync key: %w", err)
	}
	inner, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("could not unlock the sync key: %w", err)
	}
	var kf keyFile
	if err := json.Unmarshal(inner, &kf); err != nil {
		return nil, fmt.Errorf("wrapped key contents malformed: %w", err)
	}
	parsed, err := age.ParseX25519Identity(strings.TrimSpace(kf.Identity))
	if err != nil {
		return nil, fmt.Errorf("wrapped key contents malformed: %w", err)
	}
	k := keysFromIdentity(parsed)
	if kf.Fingerprint != "" && kf.Fingerprint != k.Fingerprint {
		return nil, errors.New("wrapped key fingerprint does not match its identity — key object corrupted or tampered with")
	}
	return k, nil
}
