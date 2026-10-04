package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestDir(t *testing.T) *Dir {
	t.Helper()
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDirRoundtrip(t *testing.T) {
	ctx := context.Background()
	d := newTestDir(t)

	if _, err := d.Get(ctx, "bundles/ab/1-2-cafe.age"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get missing = %v, want ErrNotExist", err)
	}
	want := []byte("hello world")
	if err := d.Put(ctx, "bundles/ab/1-2-cafe.age", want); err != nil {
		t.Fatal(err)
	}
	got, err := d.Get(ctx, "bundles/ab/1-2-cafe.age")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("Get = %q, want %q", got, want)
	}

	// Put overwrites.
	if err := d.Put(ctx, "bundles/ab/1-2-cafe.age", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	got, _ = d.Get(ctx, "bundles/ab/1-2-cafe.age")
	if string(got) != "v2" {
		t.Fatalf("after overwrite Get = %q, want v2", got)
	}
}

func TestDirPutIfAbsent(t *testing.T) {
	ctx := context.Background()
	d := newTestDir(t)

	if err := d.PutIfAbsent(ctx, "keys/master.age", []byte("first")); err != nil {
		t.Fatal(err)
	}
	err := d.PutIfAbsent(ctx, "keys/master.age", []byte("second"))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second PutIfAbsent = %v, want ErrExists", err)
	}
	got, err := d.Get(ctx, "keys/master.age")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("loser overwrote winner: %q", got)
	}
}

func TestDirList(t *testing.T) {
	ctx := context.Background()
	d := newTestDir(t)

	keys := []string{"aii-sync.json", "bundles/aa/1-1-x.age", "bundles/bb/2-3-y.age", "keys/master.age"}
	for _, k := range keys {
		if err := d.Put(ctx, k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	// Droppings that must stay invisible: dot-temps and sync-tool files.
	for _, junk := range []string{".tmp-deadbeef", "bundles/aa/.tmp-cafe", "bundles/.syncthing.x.tmp"} {
		if err := os.WriteFile(filepath.Join(d.root, filepath.FromSlash(junk)), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	all, err := d.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(keys) {
		t.Fatalf("List(\"\") returned %d objects, want %d: %+v", len(all), len(keys), all)
	}
	for _, o := range all {
		if o.Size <= 0 {
			t.Errorf("object %s has size %d", o.Key, o.Size)
		}
	}

	bundles, err := d.List(ctx, "bundles/")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 2 {
		t.Fatalf("List(bundles/) returned %d, want 2: %+v", len(bundles), bundles)
	}
	for _, o := range bundles {
		if o.Key != "bundles/aa/1-1-x.age" && o.Key != "bundles/bb/2-3-y.age" {
			t.Errorf("unexpected key %q", o.Key)
		}
	}
}

func TestDirDelete(t *testing.T) {
	ctx := context.Background()
	d := newTestDir(t)

	if err := d.Put(ctx, "bundles/aa/1-1-x.age", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "bundles/aa/1-1-x.age"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, "bundles/aa/1-1-x.age"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Get after delete = %v, want ErrNotExist", err)
	}
	// Deleting an absent key is success.
	if err := d.Delete(ctx, "bundles/aa/1-1-x.age"); err != nil {
		t.Fatalf("delete absent = %v, want nil", err)
	}
}

func TestDirKeyEscapes(t *testing.T) {
	ctx := context.Background()
	d := newTestDir(t)
	for _, bad := range []string{"", "/abs", "../escape", "a/../../b", `a\b`} {
		if err := d.Put(ctx, bad, []byte("x")); err == nil {
			t.Errorf("Put(%q) succeeded, want error", bad)
		}
		if _, err := d.Get(ctx, bad); err == nil || errors.Is(err, ErrNotExist) {
			t.Errorf("Get(%q) = %v, want invalid-key error", bad, err)
		}
	}
}
