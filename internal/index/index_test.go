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
	ptr, err := db.GetUnit(context.Background(), store.Hash(pkgPath))
	if err != nil {
		t.Fatalf("GetUnit(%s): %v", pkgPath, err)
	}
	blob, ok, err := cas.Get(context.Background(), ptr.BlobKey)
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

// TestBuild_CrossPackageMethodRefIdentity verifies that a cross-package call
// to a method resolves to the exact same SymbolID hash as its own
// definition. The defining type's method set is declared in an order
// (Zulu, Alpha, Mike, GetLabel) that differs from alphabetical order: a
// SymbolID built from a method's objectpath encodes its index within the
// method's defining *types.Named, so this guards against that index
// disagreeing between the definer's source-checked type (the definition
// side) and the caller's export-data-decoded import of it (the reference
// side) — the same gap that left cross-package method references untested
// even though TestBuild_CrossPackageRefIdentity covers a plain function.
// Uses its own synthetic module (rather than testdata/module) so it does
// not have to share leaf/mid/top's fixed file layout with every other test
// in this package.
func TestBuild_CrossPackageMethodRefIdentity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/methodxref\n\ngo 1.23\n")
	writeFile(t, dir, "defpkg/defpkg.go", `package defpkg

type T struct{}

func (t *T) Zulu() string     { return "z" }
func (t *T) Alpha() string    { return "a" }
func (t *T) Mike() string     { return "m" }
func (t *T) GetLabel() string { return "label" }
`)
	writeFile(t, dir, "callerpkg/callerpkg.go", `package callerpkg

import "example.com/methodxref/defpkg"

func Use(t *defpkg.T) string {
	return t.Zulu() + t.Alpha() + t.Mike() + t.GetLabel()
}
`)

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	const defPkgPath = "example.com/methodxref/defpkg"
	const callerPkgPath = "example.com/methodxref/callerpkg"

	for _, method := range []string{"Zulu", "Alpha", "Mike", "GetLabel"} {
		idHash := findSymbolByName(t, db, cas, defPkgPath, method)

		var found bool
		viewFacts(t, db, cas, callerPkgPath, func(v *store.View) {
			for _, r := range v.RefsTo(idHash) {
				if r.ToPkgHash() == store.Hash(defPkgPath) {
					found = true
				}
			}
		})
		if !found {
			t.Errorf("caller has no ref resolving to defpkg.T.%s's SymbolID; cross-package method ref identity broken", method)
		}
	}
}

// writeFile writes content to rel under dir, creating parent directories as
// needed.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
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

// findFileIdx returns the index into pkgPath's facts blob file table of the
// file whose base name is base, failing t if none matches.
func findFileIdx(t *testing.T, db *store.DB, cas *store.CAS, pkgPath, base string) uint32 {
	t.Helper()
	var idx uint32
	found := false
	viewFacts(t, db, cas, pkgPath, func(v *store.View) {
		for i := 0; i < v.FileCount(); i++ {
			f, err := v.FileAt(i)
			if err != nil {
				t.Fatalf("FileAt(%d): %v", i, err)
			}
			if filepath.Base(f) == base {
				idx = uint32(i)
				found = true
				return
			}
		}
	})
	if !found {
		t.Fatalf("no file named %s in %s's facts blob file table", base, pkgPath)
	}
	return idx
}

// TestBuild_InPackageTestFileFactsIndexed verifies that an in-package
// _test.go file contributes to the facts index: its own definitions are
// indexed (helperGreeting, declared only in greet_test.go), and a
// definition elsewhere in the package (Hello) gets a recorded reference for
// its usage inside the test file.
func TestBuild_InPackageTestFileFactsIndexed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/testfacts\n\ngo 1.23\n")
	writeFile(t, dir, "greet/greet.go", `package greet

// Hello returns a greeting for name.
func Hello(name string) string {
	return "hello " + name
}
`)
	writeFile(t, dir, "greet/greet_test.go", `package greet

import "testing"

// helperGreeting is declared only in this in-package test file.
func helperGreeting() string {
	return Hello("test")
}

func TestHello(t *testing.T) {
	if helperGreeting() == "" {
		t.Fatal("empty greeting")
	}
}
`)

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)

	if _, err := Build(context.Background(), snap, db, cas, Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	const pkgGreet = "example.com/testfacts/greet"

	// helperGreeting is declared only in greet_test.go: finding it at all
	// confirms the test file's own definitions are indexed.
	_ = findSymbolByName(t, db, cas, pkgGreet, "helperGreeting")

	// Hello's own facts must include a reference from inside greet_test.go
	// (helperGreeting's call to it), the only call site in this package.
	helloID := findSymbolByName(t, db, cas, pkgGreet, "Hello")
	testFileIdx := findFileIdx(t, db, cas, pkgGreet, "greet_test.go")
	var foundRefFromTestFile bool
	viewFacts(t, db, cas, pkgGreet, func(v *store.View) {
		for _, r := range v.RefsTo(helloID) {
			if r.FileIdx() == testFileIdx {
				foundRefFromTestFile = true
			}
		}
	})
	if !foundRefFromTestFile {
		t.Error("Hello has no recorded reference from inside greet_test.go; in-package test file references are not indexed")
	}
}

// TestBuild_ExternalTestPackageContributesNothing verifies that a directory
// containing both an ordinary package and an external "_test"-suffixed test
// package indexes only the ordinary package's own files: the external test
// package's symbols must never leak into the facts index.
func TestBuild_ExternalTestPackageContributesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/exttest\n\ngo 1.23\n")
	writeFile(t, dir, "mixed/mixed.go", `package mixed

// V returns 1.
func V() int { return 1 }
`)
	writeFile(t, dir, "mixed/mixed_test.go", `package mixed_test

import (
	"testing"

	"example.com/exttest/mixed"
)

// externalOnlyHelper exists only in the external "_test" package.
func externalOnlyHelper() int { return mixed.V() }

func TestV(t *testing.T) {
	if externalOnlyHelper() != 1 {
		t.Fatal("bad")
	}
}
`)

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)

	if _, err := Build(context.Background(), snap, db, cas, Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	const pkgMixed = "example.com/exttest/mixed"
	viewFacts(t, db, cas, pkgMixed, func(v *store.View) {
		if v.FileCount() != 1 {
			t.Errorf("FileCount() = %d, want 1 (mixed.go only; the external \"_test\" package's file must be excluded)", v.FileCount())
		}
		for i := 0; i < v.SymbolCount(); i++ {
			sym, err := v.SymbolAt(i)
			if err != nil {
				t.Fatalf("SymbolAt(%d): %v", i, err)
			}
			if sym.Name() == "externalOnlyHelper" {
				t.Error("externalOnlyHelper (declared in the external \"_test\" package) leaked into mixed's facts index")
			}
		}
	})
}
