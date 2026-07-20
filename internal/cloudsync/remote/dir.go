package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Dir is a Remote backed by a local directory — typically one kept in
// sync by Dropbox, iCloud Drive, Syncthing, or a network mount. It is
// also the backend the test suite runs against.
//
// Writes are atomic: content goes to a dot-prefixed temp file in the
// destination directory, is fsynced, then renamed into place. List
// skips dotfiles entirely, which also hides sync-tool droppings like
// Syncthing's ".syncthing.*.tmp" partials.
type Dir struct {
	root string
}

func NewDir(root string) (*Dir, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("remote dir must be an absolute path, got %q", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Dir{root: root}, nil
}

func (d *Dir) String() string { return "dir:" + d.root }

// keyPath maps a slash-separated object key onto the filesystem,
// rejecting anything that could escape the root.
func (d *Dir) keyPath(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	clean := path.Clean(key)
	if clean != key || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	return filepath.Join(d.root, filepath.FromSlash(clean)), nil
}

func (d *Dir) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(d.root, func(p string, ent fs.DirEntry, err error) error {
		if err != nil {
			// A file vanishing mid-walk (cloud sync churn) is not an
			// error worth failing the whole listing for.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			if ent.IsDir() && p != d.root {
				return filepath.SkipDir
			}
			return nil
		}
		if ent.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := ent.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		out = append(out, Object{Key: key, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", d, err)
	}
	return out, nil
}

func (d *Dir) Get(_ context.Context, key string) ([]byte, error) {
	p, err := d.keyPath(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotExist
	}
	return b, err
}

func (d *Dir) Put(_ context.Context, key string, data []byte) error {
	p, err := d.keyPath(key)
	if err != nil {
		return err
	}
	tmp, err := d.writeTemp(p, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}

func (d *Dir) PutIfAbsent(_ context.Context, key string, data []byte) error {
	p, err := d.keyPath(key)
	if err != nil {
		return err
	}
	tmp, err := d.writeTemp(p, data)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	// Hard-link the temp file onto the final name: succeeds only if
	// the name is free, atomically. Filesystems without hard links
	// (some network mounts) get a create-exclusive fallback — not
	// atomic with respect to content, but pull verifies MACs, so a
	// half-visible file just fails verification and is retried.
	if err := os.Link(tmp, p); err == nil {
		syncDir(filepath.Dir(p))
		return nil
	} else if errors.Is(err, fs.ErrExist) {
		return ErrExists
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(p)
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}

func (d *Dir) Delete(_ context.Context, key string) error {
	p, err := d.keyPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeTemp writes data to a fsynced dot-temp file next to dst and
// returns its path. Dot prefix keeps it invisible to List.
func (d *Dir) writeTemp(dst string, data []byte) (string, error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// syncDir best-effort fsyncs a directory so renames/links survive a
// crash. Errors are ignored — not all filesystems support it.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		f.Close()
	}
}
