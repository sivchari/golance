package index

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
)

const (
	pkgLeaf = "example.com/idxmod/leaf"
	pkgMid  = "example.com/idxmod/mid"
	pkgTop  = "example.com/idxmod/top"
)

func loadTestSnapshot(t *testing.T) *graph.Snapshot {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	return snap
}

// mutableTestModule copies testdata/module into a fresh temp directory and
// returns its path, for tests that need to edit files, touch mtimes, or
// add/remove files on disk without mutating the checked-in fixture (which
// other tests read concurrently).
func mutableTestModule(t *testing.T) string {
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

func loadSnapshot(t *testing.T, dir string) *graph.Snapshot {
	t.Helper()
	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load(%s): %v", dir, err)
	}
	return snap
}

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	return db
}

func openTestCAS(t *testing.T) *store.CAS {
	t.Helper()
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	return cas
}

// viewFacts loads pkgPath's current facts blob (via db's UnitPointer and
// cas) and runs fn against a [store.View] over it, for test convenience.
func viewFacts(t *testing.T, db *store.DB, cas *store.CAS, pkgPath string, fn func(v *store.View)) {
	t.Helper()
	ptr, err := db.GetUnit(store.Hash(pkgPath))
	if err != nil {
		t.Fatalf("GetUnit(%s): %v", pkgPath, err)
	}
	blob, ok, err := cas.Get(ptr.BlobKey)
	if err != nil || !ok {
		t.Fatalf("CAS.Get(%s) = (%v, %v, %v)", pkgPath, blob != nil, ok, err)
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		t.Fatalf("DecodeUnitBlob(%s): %v", pkgPath, err)
	}
	v, err := store.NewView(u.Facts)
	if err != nil {
		t.Fatalf("NewView(%s): %v", pkgPath, err)
	}
	fn(v)
}

// findSymbolByName scans pkgPath's facts blob for a symbol named name and
// returns its IDHash.
func findSymbolByName(t *testing.T, db *store.DB, cas *store.CAS, pkgPath, name string) uint64 {
	t.Helper()
	var idHash uint64
	found := false
	viewFacts(t, db, cas, pkgPath, func(v *store.View) {
		for i := 0; i < v.SymbolCount(); i++ {
			sym, err := v.SymbolAt(i)
			if err != nil {
				t.Fatalf("SymbolAt(%d): %v", i, err)
			}
			if sym.Name() == name {
				idHash = sym.IDHash()
				found = true
				return
			}
		}
	})
	if !found {
		t.Fatalf("symbol %s not found in %s", name, pkgPath)
	}
	return idHash
}

// TestBuild_CrossPackageRefIdentity verifies that a reference in top to
// leaf.Hello resolves to the exact same SymbolID hash as leaf's own
// definition of Hello.
func TestBuild_CrossPackageRefIdentity(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Processed != 3 {
		t.Errorf("Processed = %d, want 3", stats.Processed)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	helloID := findSymbolByName(t, db, cas, pkgLeaf, "Hello")

	var found bool
	viewFacts(t, db, cas, pkgTop, func(v *store.View) {
		for _, r := range v.RefsTo(helloID) {
			if r.ToPkgHash() == store.Hash(pkgLeaf) {
				found = true
			}
		}
	})
	if !found {
		t.Error("top has no ref resolving to leaf.Hello's SymbolID; cross-package identity broken")
	}
}

// TestBuild_SecondRunSkipsAll verifies a second Build over unchanged
// sources reprocesses nothing.
func TestBuild_SecondRunSkipsAll(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	first, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	if first.Processed != 3 {
		t.Fatalf("first Build Processed = %d, want 3", first.Processed)
	}

	second, err := Build(ctx, snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if second.Processed != 0 {
		t.Errorf("second Build Processed = %d, want 0", second.Processed)
	}
	if second.Skipped != 3 {
		t.Errorf("second Build Skipped = %d, want 3", second.Skipped)
	}
}

// writeEmptyPackageModule writes a synthetic module with one ordinary
// package (pkgGood) and one root package that go/packages legitimately
// reports with zero GoFiles: a directory containing only an external
// "_test" package, which declares no non-test source at all.
func writeEmptyPackageModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/emptypkg\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	goodDir := filepath.Join(dir, "good")
	if err := os.MkdirAll(goodDir, 0o750); err != nil {
		t.Fatalf("mkdir good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "good.go"), []byte("package good\n\n// V returns 1.\nfunc V() int { return 1 }\n"), 0o600); err != nil {
		t.Fatalf("write good.go: %v", err)
	}
	emptyDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(emptyDir, 0o750); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	const testOnlySrc = `package empty_test

import "testing"

func TestNothing(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(emptyDir, "e_test.go"), []byte(testOnlySrc), 0o600); err != nil {
		t.Fatalf("write e_test.go: %v", err)
	}
	return dir
}

// TestBuild_EmptyPackageIsSkippedNotFatal verifies that a root package with
// no GoFiles (go/packages legitimately reports these, e.g. a directory
// containing only an external "_test" package) is counted as Skipped and
// does not turn Build's return error non-nil, while every other package
// still builds normally.
func TestBuild_EmptyPackageIsSkippedNotFatal(t *testing.T) {
	dir := writeEmptyPackageModule(t)
	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed = %d, want 1", stats.Processed)
	}
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", stats.Skipped)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	fp, err := db.BuildFingerprint()
	if err != nil {
		t.Fatalf("BuildFingerprint: %v", err)
	}
	if fp != runtime.Version() {
		t.Errorf("BuildFingerprint = %q, want %q", fp, runtime.Version())
	}
}
