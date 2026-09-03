package graph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
)

const (
	modA = "example.com/simple/a"
	modB = "example.com/simple/b"
	modC = "example.com/simple/c"
)

// TestLoadMode_NeverRequestsExportFile is a regression test for the
// removed `go list -export` path (see internal/depexport's package doc):
// graph.Load is the ONLY go/packages.Load call site left anywhere in
// golance (every other package resolves dependency export data via
// internal/depexport's declaration-only source-checking instead), so
// asserting loadMode itself never sets packages.NeedExportFile is a
// complete, load-bearing guarantee that golance never again asks the Go
// toolchain to compile a package's export data — not merely a check of
// this one call site among several.
func TestLoadMode_NeverRequestsExportFile(t *testing.T) {
	if loadMode&packages.NeedExportFile != 0 {
		t.Error("loadMode requests packages.NeedExportFile; this forces `go list -export`, compiling every matched package with the Go toolchain — see internal/depexport's package doc for why that must never happen again")
	}
}

func loadTestdata(t *testing.T) *Snapshot {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "simple"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := Load(Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return snap
}

func TestLoad_TopoOrder(t *testing.T) {
	snap := loadTestdata(t)

	for _, path := range []string{modA, modB, modC} {
		if _, ok := snap.Package(path); !ok {
			t.Errorf("Packages missing %s", path)
		}
	}

	idx := make(map[string]int, len(snap.Order))
	for i, path := range snap.Order {
		idx[path] = i
	}
	if idx[modA] >= idx[modB] {
		t.Errorf("topo order: want a before b, got order %v", snap.Order)
	}
	if idx[modB] >= idx[modC] {
		t.Errorf("topo order: want b before c, got order %v", snap.Order)
	}
}

// TestLoad_PackageName checks that Package.Name is populated straight from
// go/packages' NeedName result (see loadMode) — the candidate source
// internal/server's unimported-completion index relies on to avoid ever
// having to open and parse a package's files itself just to learn its
// declared name.
func TestLoad_PackageName(t *testing.T) {
	snap := loadTestdata(t)

	for path, want := range map[string]string{modA: "a", modB: "b", modC: "c"} {
		pkg, ok := snap.Package(path)
		if !ok {
			t.Fatalf("Package(%s) missing", path)
		}
		if pkg.Name != want {
			t.Errorf("Package(%s).Name = %q, want %q", path, pkg.Name, want)
		}
	}
}

// TestLoad_TestOnlyImports checks the core Phase 1 fix: loading with
// Config.Tests: true (see Load's doc) makes "testing" — imported only by
// a's in-package _test.go file, never by any production file in this
// module — a real graph node with usable Dir/Name/GoFiles, and records it
// on modA's TestImports, not Imports (see Package.TestImports's doc).
func TestLoad_TestOnlyImports(t *testing.T) {
	snap := loadTestdata(t)

	testingPkg, ok := snap.Package("testing")
	if !ok {
		t.Fatal(`Package("testing") missing: test-only imports were not loaded`)
	}
	if testingPkg.Name != "testing" {
		t.Errorf(`Package("testing").Name = %q, want "testing"`, testingPkg.Name)
	}
	if testingPkg.Dir == "" || len(testingPkg.GoFiles) == 0 {
		t.Errorf(`Package("testing") has Dir=%q GoFiles=%v, want both populated`, testingPkg.Dir, testingPkg.GoFiles)
	}
	if testingPkg.Root {
		t.Error(`Package("testing").Root = true, want false: it never matched a Load pattern directly`)
	}

	a, ok := snap.Package(modA)
	if !ok {
		t.Fatalf("Package(%s) missing", modA)
	}
	if slices.Contains(a.Imports, "testing") {
		t.Errorf("Package(%s).Imports contains %q, want it only in TestImports", modA, "testing")
	}
	if !slices.Contains(a.TestImports, "testing") {
		t.Errorf("Package(%s).TestImports = %v, want it to contain %q", modA, a.TestImports, "testing")
	}
}

// TestLoad_ExternalTestPackage checks that b's external "_test" test
// package ("example.com/simple/b_test", declared in b/b_test.go) gets its
// own canonical, non-Root graph node distinct from b's, with its own real
// Imports — including c, which (in production) imports b itself. That
// "test imports something that imports the package under test" shape is
// only legal for an EXTERNAL test package (a distinct PkgPath from b's own;
// an in-package test attempting it is a real import cycle go list itself
// rejects) — see fromPackages's external-variant branch and its doc for why
// b_test staying its own node, never merged into b, is what keeps this
// import-cycle-safe: nothing imports b_test in turn, so it can never
// participate in a topoOrder cycle regardless of what it itself imports.
func TestLoad_ExternalTestPackage(t *testing.T) {
	snap := loadTestdata(t)

	const extPath = modB + "_test"
	ext, ok := snap.Package(extPath)
	if !ok {
		t.Fatalf("Package(%s) missing: external test package was not loaded", extPath)
	}
	if ext.Root {
		t.Errorf("Package(%s).Root = true, want false", extPath)
	}
	for _, want := range []string{modB, modC, "testing"} {
		if !slices.Contains(ext.Imports, want) {
			t.Errorf("Package(%s).Imports = %v, want it to contain %q", extPath, ext.Imports, want)
		}
	}

	// b itself must stay unaffected: its own Imports/TestImports come from
	// b.go alone (no in-package test file), not from the separate b_test
	// package that happens to live in the same directory.
	b, ok := snap.Package(modB)
	if !ok {
		t.Fatalf("Package(%s) missing", modB)
	}
	if len(b.TestImports) != 0 {
		t.Errorf("Package(%s).TestImports = %v, want empty (b has no in-package test file)", modB, b.TestImports)
	}
	if slices.Contains(b.Imports, modC) {
		t.Errorf("Package(%s).Imports = %v, want it unaffected by b_test's own import of c", modB, b.Imports)
	}

	// Order must still place a before b before c: b_test's own edge into c
	// (topologically "after" b) never feeds back into anything topoOrder
	// computes over, since nothing imports b_test.
	idx := make(map[string]int, len(snap.Order))
	for i, path := range snap.Order {
		idx[path] = i
	}
	if idx[modA] >= idx[modB] {
		t.Errorf("topo order: want a before b, got order %v", snap.Order)
	}
	if idx[modB] >= idx[modC] {
		t.Errorf("topo order: want b before c, got order %v", snap.Order)
	}
}

// TestSnapshot_ClosureUnits also covers b_test (b's external test package,
// see TestLoad_ExternalTestPackage) correctly appearing in a's and c's
// closures: b_test genuinely imports c, which imports b, which imports a,
// so it is a real (if non-Root, non-indexed — see index/scheduler.go's
// Root-only fan-in) transitive importer of both.
func TestSnapshot_ClosureUnits(t *testing.T) {
	snap := loadTestdata(t)

	got := snap.ClosureUnits(modA)
	want := []string{modA, modB, modC, modB + "_test"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("ClosureUnits(%s) = %v, want %v", modA, got, want)
	}

	got = snap.ClosureUnits(modC)
	want = []string{modC, modB + "_test"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("ClosureUnits(%s) = %v, want %v", modC, got, want)
	}
}

func TestCache_RoundTrip(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "simple"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	snap := loadTestdata(t)
	patterns := []string{"./..."}
	if err := SaveCache(root, patterns, nil, snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	loaded, ok := LoadCache(root, patterns, nil)
	if !ok {
		t.Fatal("LoadCache: ok=false after SaveCache")
	}
	if len(loaded.Packages) != len(snap.Packages) {
		t.Errorf("LoadCache package count = %d, want %d", len(loaded.Packages), len(snap.Packages))
	}
	if !slices.Equal(loaded.Order, snap.Order) {
		t.Errorf("LoadCache order = %v, want %v", loaded.Order, snap.Order)
	}

	if _, ok := LoadCache(root, []string{"./other"}, nil); ok {
		t.Error("LoadCache should miss for a different patterns key")
	}

	if Stale(root) {
		t.Error("Stale should be false right after SaveCache with no module file changes")
	}
}

// TestLoadCache_RejectsOldVersion checks that a diskCache written before
// cacheVersion existed (or by an older mismatched version) is treated as a
// miss rather than being served with its packages' Name fields silently
// empty — see cacheVersion's doc for why this matters specifically for
// unimported completion: Stale alone (go.mod/go.sum/go.work mtimes) would
// never catch this, since none of those changed.
func TestLoadCache_RejectsOldVersion(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "simple"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	patterns := []string{"./..."}

	old := diskCache{Version: cacheVersion - 1, Patterns: patterns, Packages: map[string]*Package{}}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal old diskCache: %v", err)
	}
	file := CacheFile(root)
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatalf("write old cache file: %v", err)
	}

	if _, ok := LoadCache(root, patterns, nil); ok {
		t.Error("LoadCache should miss for a cache written with an old cacheVersion")
	}
}

// TestStale_DeletedTrackedFile verifies that deleting a go.work that
// existed when the cache was saved is itself detected as staleness, not
// just a modification to one that still exists (see Stale's doc).
func TestStale_DeletedTrackedFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	goWork := filepath.Join(root, "go.work")
	if err := os.WriteFile(goWork, []byte("go 1.23\n"), 0o600); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	// Back-date go.work so its ModTime is unambiguously older than the
	// cache file SaveCache is about to write, regardless of filesystem
	// mtime resolution.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(goWork, past, past); err != nil {
		t.Fatalf("chtimes go.work: %v", err)
	}

	snap := &Snapshot{Packages: map[string]*Package{}}
	if err := SaveCache(root, []string{"./..."}, nil, snap); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	if Stale(root) {
		t.Fatal("Stale should be false right after SaveCache with go.work present and unchanged")
	}

	if err := os.Remove(goWork); err != nil {
		t.Fatalf("remove go.work: %v", err)
	}
	if !Stale(root) {
		t.Error("Stale should be true after a go.work that existed at cache-save time was deleted")
	}
}
