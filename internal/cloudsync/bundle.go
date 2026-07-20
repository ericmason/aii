package cloudsync

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"filippo.io/age"
	"github.com/ericmason/aii/internal/store"
)

// Bundle wire format (Encrypt-then-MAC):
//
//	object     := magic("aiisync1") || ciphertext || mac(32B)
//	ciphertext := age.Encrypt(recipient, gzip(bundleJSON))
//	mac        := HMAC-SHA256(BundleKey,
//	                  "bundle/v1" || 0x00 || objectKey || 0x00 || magic || ciphertext)
//
// The MAC binds the repo-relative object key — which itself embeds the
// pseudonymous name, epoch, count, and chain-head prefix — so a bundle
// that is renamed, moved between sessions, or copied from another repo
// fails closed BEFORE any decryption. age alone authenticates nothing:
// anyone holding the recipient public key could otherwise mint
// ciphertexts we would happily import.

const (
	bundleMagic   = "aiisync1"
	FormatVersion = 1

	// maxBundlePlaintext caps gzip expansion (decompression-bomb guard).
	maxBundlePlaintext = 2 << 30
)

type BundleMessage struct {
	Ordinal int    `json:"ordinal"`
	Role    string `json:"role"`
	TS      int64  `json:"ts,omitempty"`
	Content string `json:"content"`
}

type Bundle struct {
	Format     int             `json:"format"`
	AiiVersion string          `json:"aii_version,omitempty"`
	Agent      string          `json:"agent"`
	SessionUID string          `json:"session_uid"`
	Workspace  string          `json:"workspace,omitempty"`
	Title      string          `json:"title,omitempty"`
	Summary    string          `json:"summary,omitempty"`
	StartedAt  int64           `json:"started_at,omitempty"`
	EndedAt    int64           `json:"ended_at,omitempty"`
	Epoch      int64           `json:"epoch"`
	ChainHead  string          `json:"chain_head"` // hex, full 32 bytes
	Messages   []BundleMessage `json:"messages"`
}

// rows converts bundle messages to store rows (they share shape).
func (b *Bundle) rows() []store.Row {
	out := make([]store.Row, len(b.Messages))
	for i, m := range b.Messages {
		out[i] = store.Row{Ordinal: m.Ordinal, Role: m.Role, TS: m.TS, Content: m.Content}
	}
	return out
}

// --- object key grammar -------------------------------------------------

// versionKey formats the immutable object key for one bundle version:
// bundles/<name32>/<epoch>-<count>-<head8>.age
func versionKey(name string, epoch int64, count int, head chainHash) string {
	return fmt.Sprintf("bundles/%s/%d-%d-%s.age", name, epoch, count, hex.EncodeToString(head[:4]))
}

// tombstoneKey is the purge marker for a session name.
func tombstoneKey(name string) string { return "bundles/" + name + "/purged" }

var versionKeyRe = regexp.MustCompile(`^bundles/([0-9a-f]{32})/([0-9]{1,10})-([0-9]{1,10})-([0-9a-f]{8})\.age$`)

type version struct {
	Name  string
	Epoch int64
	Count int
	Head8 string // hex of first 4 chain-head bytes
	Size  int64
}

// parseVersionKey parses an object key against the strict grammar.
// Anything that doesn't match — Dropbox "conflicted copy" files,
// stray uploads — is simply not a version object.
func parseVersionKey(key string) (version, bool) {
	m := versionKeyRe.FindStringSubmatch(key)
	if m == nil {
		return version{}, false
	}
	epoch, err1 := strconv.ParseInt(m[2], 10, 64)
	count, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || epoch < 1 || count < 1 {
		return version{}, false
	}
	return version{Name: m[1], Epoch: epoch, Count: count, Head8: m[4]}, true
}

func parseTombstoneKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "bundles/") || !strings.HasSuffix(key, "/purged") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, "bundles/"), "/purged")
	if len(name) != 32 || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// --- codec --------------------------------------------------------------

func bundleMAC(keys *Keys, objectKey string, magicAndCiphertext []byte) []byte {
	m := hmac.New(sha256.New, keys.BundleKey)
	m.Write([]byte("bundle/v1"))
	m.Write([]byte{0})
	m.Write([]byte(objectKey))
	m.Write([]byte{0})
	m.Write(magicAndCiphertext)
	return m.Sum(nil)
}

// EncodeBundle serializes, compresses, encrypts, and MACs a bundle for
// storage at objectKey.
func EncodeBundle(b *Bundle, keys *Keys, objectKey string) ([]byte, error) {
	plain, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz) // header ModTime left zero: deterministic
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.WriteString(bundleMagic)
	ew, err := age.Encrypt(&out, keys.Recipient())
	if err != nil {
		return nil, err
	}
	if _, err := ew.Write(gz.Bytes()); err != nil {
		return nil, err
	}
	if err := ew.Close(); err != nil {
		return nil, err
	}
	out.Write(bundleMAC(keys, objectKey, out.Bytes()))
	return out.Bytes(), nil
}

// DecodeBundle verifies and decodes an object fetched from objectKey.
// Order matters: MAC first (over the untrusted bytes and the key they
// were fetched under), then decrypt, then structural validation:
// format version, interior identity consistent with the object key's
// name/epoch/count/head, contiguous ordinals, and a full chain
// recompute matching the declared head.
func DecodeBundle(data []byte, keys *Keys, objectKey string) (*Bundle, error) {
	if len(data) < len(bundleMagic)+sha256.Size+1 {
		return nil, errors.New("bundle too short")
	}
	if string(data[:len(bundleMagic)]) != bundleMagic {
		return nil, errors.New("bad bundle magic")
	}
	body, mac := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	if !hmac.Equal(mac, bundleMAC(keys, objectKey, body)) {
		return nil, errors.New("bundle authentication failed — object was tampered with, renamed, or belongs to a different repo")
	}

	rd, err := age.Decrypt(bytes.NewReader(body[len(bundleMagic):]), keys.Identity())
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	zr, err := gzip.NewReader(rd)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	plain, err := io.ReadAll(io.LimitReader(zr, maxBundlePlaintext+1))
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	if len(plain) > maxBundlePlaintext {
		return nil, errors.New("bundle plaintext exceeds size limit")
	}

	var b Bundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if b.Format > FormatVersion {
		return nil, fmt.Errorf("bundle format %d is newer than this aii understands — upgrade aii", b.Format)
	}
	if b.Format < 1 || b.Agent == "" || b.SessionUID == "" {
		return nil, errors.New("bundle missing required fields")
	}
	for i, m := range b.Messages {
		if m.Ordinal != i {
			return nil, fmt.Errorf("bundle ordinals not contiguous at %d", i)
		}
		if m.Role == "" {
			return nil, fmt.Errorf("bundle message %d missing role", i)
		}
	}

	// Interior identity must match the slot the object was fetched
	// from — a valid MAC only proves it's OUR object, not that it
	// lives under the right name.
	v, ok := parseVersionKey(objectKey)
	if !ok {
		return nil, fmt.Errorf("object key %q does not match version grammar", objectKey)
	}
	if keys.BundleName(b.Agent, b.SessionUID) != v.Name {
		return nil, errors.New("bundle identity does not match its object name")
	}
	if b.Epoch != v.Epoch || len(b.Messages) != v.Count {
		return nil, errors.New("bundle epoch/count does not match its object name")
	}
	head := computeChain(b.Agent, b.SessionUID, b.rows())
	if hex.EncodeToString(head[:]) != b.ChainHead {
		return nil, errors.New("bundle chain head does not match its messages")
	}
	if hex.EncodeToString(head[:4]) != v.Head8 {
		return nil, errors.New("bundle chain head does not match its object name")
	}
	return &b, nil
}

// --- purge tombstones ---------------------------------------------------

// tombstoneBody is the authenticated content of a purge marker.
func tombstoneBody(keys *Keys, name string) []byte {
	m := hmac.New(sha256.New, keys.BundleKey)
	m.Write([]byte("purge/v1"))
	m.Write([]byte{0})
	m.Write([]byte(name))
	return []byte(hex.EncodeToString(m.Sum(nil)) + "\n")
}

// verifyTombstone reports whether data is a valid purge marker for
// name. Unauthenticated tombstones are ignored — otherwise anyone
// with bucket write access could suppress pushes.
func verifyTombstone(keys *Keys, name string, data []byte) bool {
	return hmac.Equal(bytes.TrimSpace(data), bytes.TrimSpace(tombstoneBody(keys, name)))
}
