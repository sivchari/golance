package xref

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
)

// copyTestModule copies testdata/module into a fresh temp directory,
// returning its path. Used to simulate two different git worktrees
// (byte-identical content, different absolute roots) without needing a real
// git repository.
func copyTestModule(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copy testdata module: %v", err)
	}
	return dst
}

// TestResolver_RelativePaths_RoundTripsAcrossRoots verifies the API
// boundary internal/server relies on for worktree sharing: a database built
// with Options.RelativePaths under one root, when opened by a Resolver for
// a different root (a byte-identical copy of the same module, standing in
// for a second git worktree), still returns absolute Location.File values —
// and specifically, absolute paths under the *new* root, not the one the
// database was built from.
func TestResolver_RelativePaths_RoundTripsAcrossRoots(t *testing.T) {
	rootA := copyTestModule(t)
	snapA, err := graph.Load(graph.Options{Dir: rootA}, "./...")
	if err != nil {
		t.Fatalf("graph.Load(rootA): %v", err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	// A single CAS, shared across rootA and rootB (see the package doc):
	// blobs are root-agnostic (paths stored relative), exactly the property
	// that lets every worktree of a repository share one CAS directory.
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if _, err := index.Build(context.Background(), snapA, db, cas, index.Options{RelativePaths: true}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	rootB := copyTestModule(t)
	snapB, err := graph.Load(graph.Options{Dir: rootB}, "./...")
	if err != nil {
		t.Fatalf("graph.Load(rootB): %v", err)
	}
	r := New(db, cas, snapB, true)

	userFile := goFile(t, snapB, pkgUser, "user.go")
	line, col := identOccurrence(t, userFile, "Person", 1) // "impl.Person" return type in Declare

	locs, err := r.Definition(userFile, line, col)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Definition() = %d locations, want 1", len(locs))
	}

	wantFile := goFile(t, snapB, pkgImpl, "impl.go")
	if locs[0].File != wantFile {
		t.Errorf("Definition().File = %s, want %s (rootB's own absolute path, not rootA's)", locs[0].File, wantFile)
	}
	if filepath.IsAbs(locs[0].File) != true {
		t.Errorf("Definition().File = %s, want an absolute path", locs[0].File)
	}
}
