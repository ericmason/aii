// Package remote abstracts the dumb storage backends `aii sync` talks
// to. A Remote is deliberately minimal — list/get/put/delete over
// opaque byte blobs — because the sync engine assumes nothing beyond
// last-writer-wins semantics from the underlying store. All
// consistency guarantees (immutability, authentication, versioning)
// are layered above via object naming and MACs, not backend features.
package remote

import (
	"context"
	"errors"
	"strings"
	"time"
)

// contentAddressed reports whether a key names its own contents, so
// two writers racing on it necessarily store equivalent objects. Every
// key under bundles/ qualifies: a version key carries the epoch, the
// message count, and the chain head, and a tombstone body is a MAC of
// the name. Keys outside that tree (the wrapped master key, the repo
// marker) do not, so a lost race on them replaces one writer's data
// with another's.
func contentAddressed(key string) bool {
	return strings.HasPrefix(key, "bundles/")
}

var (
	// ErrNotExist is returned by Get for a missing object.
	ErrNotExist = errors.New("remote: object does not exist")
	// ErrExists is returned by PutIfAbsent when the object is already
	// present. Callers treat it as "a peer got there first", not a
	// failure.
	ErrExists = errors.New("remote: object already exists")
)

// Object describes one stored blob. Key is slash-separated and
// relative to the repository root, never absolute.
type Object struct {
	Key     string
	Size    int64
	ModTime time.Time
}

type Remote interface {
	// List returns every object whose key starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Get fetches an object, ErrNotExist if absent.
	Get(ctx context.Context, key string) ([]byte, error)
	// Put writes an object unconditionally. Only ever used for
	// small marker rewrites (e.g. re-binding); bundle and key
	// objects must go through PutIfAbsent.
	Put(ctx context.Context, key string, data []byte) error
	// PutIfAbsent writes an object only if the key is not already
	// present, returning ErrExists otherwise. This is the write path
	// for all immutable objects (bundles, the wrapped master key).
	PutIfAbsent(ctx context.Context, key string, data []byte) error
	// Delete removes an object; deleting an absent key is success.
	Delete(ctx context.Context, key string) error
	// String describes the remote for humans. Must not leak secrets.
	String() string
}
