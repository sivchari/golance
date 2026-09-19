package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestClonePath_CopiesReadableContent verifies that a clone of a bbolt
// database, taken while the source is still open (simulating a live
// writer's exclusive lock), is readable via a fresh Open and contains
// exactly what was written to the source before the clone.
func TestClonePath_CopiesReadableContent(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "shared.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatalf("Open(src): %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	const pkgHash = 42
	want := UnitPointer{BlobKey: 1, ContentHash: 2, ExportHash: 3, Files: []FileStat{}}
	if err := src.PutUnit(&UnitEntry{PkgHash: pkgHash, Pointer: want}); err != nil {
		t.Fatalf("PutUnit: %v", err)
	}

	dstPath := filepath.Join(t.TempDir(), "clone.db")
	if err := ClonePath(srcPath, dstPath); err != nil {
		t.Fatalf("ClonePath: %v", err)
	}

	dst, err := OpenReadOnly(dstPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(clone): %v", err)
	}
	defer func() { _ = dst.Close() }()

	got, err := dst.GetUnit(context.Background(), pkgHash)
	if err != nil {
		t.Fatalf("GetUnit(clone): %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetUnit(clone) = %+v, want %+v", got, want)
	}
}

// TestClonePath_MissingSourceErrors verifies that cloning a nonexistent
// source reports an error and never leaves a partial dst file behind.
func TestClonePath_MissingSourceErrors(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "does-not-exist.db")
	dstPath := filepath.Join(t.TempDir(), "clone.db")

	if err := ClonePath(srcPath, dstPath); err == nil {
		t.Fatal("ClonePath(missing src) = nil error, want non-nil")
	}
	if _, err := os.Stat(dstPath); !os.IsNotExist(err) {
		t.Errorf("ClonePath(missing src) left dst behind: err = %v, want IsNotExist", err)
	}
}

// TestClonePath_NeverModifiesSource verifies that cloning a source held
// open by a live writer never disturbs that writer's own handle: the
// source must still be fully writable, through the exact same handle,
// immediately after ClonePath returns.
func TestClonePath_NeverModifiesSource(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "shared.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatalf("Open(src): %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	dstPath := filepath.Join(t.TempDir(), "clone.db")
	if err := ClonePath(srcPath, dstPath); err != nil {
		t.Fatalf("ClonePath: %v", err)
	}

	if err := src.PutBuildFingerprint("still-writable-after-clone"); err != nil {
		t.Fatalf("src no longer writable after ClonePath: %v", err)
	}
}
