package index

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// TestBuild_RelativePaths_StoresRootRelativeFileTable verifies that
// Options.RelativePaths makes Build store each package's file table (and
// UnitPointer.Files) relative to the workspace root instead of as absolute
// paths, so the resulting CAS blobs and index do not embed root's absolute
// path at all — the property internal/server relies on to share one CAS and
// index across every git worktree of a repository.
func TestBuild_RelativePaths_StoresRootRelativeFileTable(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{RelativePaths: true}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	viewFacts(t, db, cas, pkgLeaf, func(v *store.View) {
		if v.FileCount() == 0 {
			t.Fatal("leaf's facts blob has no files")
		}
		path, err := v.FileAt(0)
		if err != nil {
			t.Fatalf("FileAt(0): %v", err)
		}
		if filepath.IsAbs(path) || strings.Contains(path, dir) {
			t.Errorf("facts blob file table entry = %q, want a root-relative path with no trace of %q", path, dir)
		}
		if path != filepath.Join("leaf", "leaf.go") {
			t.Errorf("facts blob file table entry = %q, want %q", path, filepath.Join("leaf", "leaf.go"))
		}
	})

	ptr, err := db.GetUnit(context.Background(), store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit: %v", err)
	}
	if len(ptr.Files) != 1 || filepath.IsAbs(ptr.Files[0].Path) {
		t.Errorf("UnitPointer.Files = %+v, want exactly one root-relative entry", ptr.Files)
	}
}

// TestRevalidate_RelativePaths_UnchangedAcrossRoots is the core worktree-
// sharing guarantee at the internal/index layer, without needing a real git
// worktree: a database built with Options.RelativePaths for one root must
// report no changes when revalidated against a byte-identical copy of the
// same module checked out under a completely different root — proving both
// that stored paths were made root-relative (so they resolve under the new
// root) and that the content hash no longer bakes in the writing root's
// absolute path (see contentHash's doc).
func TestRevalidate_RelativePaths_UnchangedAcrossRoots(t *testing.T) {
	rootA := mutableTestModule(t)
	snapA := loadSnapshot(t, rootA)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snapA, db, cas, &Options{RelativePaths: true}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	rootB := mutableTestModule(t) // an independent, byte-identical copy under a different root
	snapB := loadSnapshot(t, rootB)

	changed, err := Revalidate(ctx, snapB, db, runtime.Version(), "", true)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if changed {
		t.Error("Revalidate() = true, want false: rootB is a byte-identical copy of rootA")
	}

	// An edit made only under rootB must still be detected.
	edited := []byte("package leaf\n\n// Greeting is a friendly greeting.\ntype Greeting struct{ Message string }\n\n// Hello returns a Greeting for name.\nfunc Hello(name string) Greeting { return Greeting{Message: \"hi \" + name} }\n")
	if err := os.WriteFile(filepath.Join(rootB, "leaf", "leaf.go"), edited, 0o600); err != nil {
		t.Fatalf("edit rootB leaf.go: %v", err)
	}

	changed, err = Revalidate(ctx, snapB, db, runtime.Version(), "", true)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if !changed {
		t.Error("Revalidate() = false, want true after editing rootB's leaf.go")
	}

	// rootA, untouched, must still report no changes of its own.
	changed, err = Revalidate(ctx, snapA, db, runtime.Version(), "", true)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if changed {
		t.Error("Revalidate() = true for untouched rootA, want false")
	}
}
