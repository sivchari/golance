package typecheck

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/graph"
)

// stdlibExportSource resolves stdlib packages the same way production now
// does (see internal/depexport's package doc): declaration-only
// source-type-checking them via internal/depcheck, never a GOCACHE-generated
// `go list -export` file. Kept as its own tiny ExportSource here, rather
// than depending on internal/depexport directly, so this package's own
// tests stay focused on Importer/Cache's two-tier resolution logic, not on
// depexport's separate persistence behavior (covered by its own tests).
type stdlibExportSource struct {
	provider *depcheck.Provider
}

// newStdlibExportSource loads this test module's own real *graph.Snapshot
// (testdata/module — a genuine go.mod, so graph.Load resolves real stdlib
// packages exactly as production does) and wraps a fresh depcheck.Provider
// over it.
func newStdlibExportSource(t *testing.T) stdlibExportSource {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	meta := depcheck.NewGraphMetadataSource(snap)
	return stdlibExportSource{provider: depcheck.NewProvider(meta, depcheck.Options{})}
}

// ExportData implements ExportSource by declaration-only source-checking
// pkgPath via s.provider and re-encoding the result as a self-contained
// blob (WriteExport) — the same shape a real fallback ExportSource
// (internal/depexport.Cache) produces. A check failure propagates as an
// error rather than a soft ok=false miss: unlike "pkgPath is not part of
// this module's import graph at all" (the genuine soft-miss case every
// other ExportSource in this file models), a failed check for a pkgPath
// the test itself asked for is a real, unexpected failure that should fail
// the test loudly instead of silently falling through to "no export data".
func (s stdlibExportSource) ExportData(pkgPath string) ([]byte, bool, error) {
	cp, err := s.provider.Package(context.Background(), pkgPath)
	if err != nil {
		return nil, false, err
	}
	blob, err := WriteExport(cp.Types(), s.provider.FileSet())
	if err != nil {
		return nil, false, err
	}
	return blob, true, nil
}

type blobSource struct {
	blobs map[string][]byte
}

func (s blobSource) ExportData(pkgPath string) ([]byte, bool, error) {
	b, ok := s.blobs[pkgPath]
	return b, ok, nil
}

func parseTestdata(t *testing.T, fset *token.FileSet, rel string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(fset, filepath.Join("testdata", "module", rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return f
}

// TestCheckPackage_StdlibExportFile checks a leaf package whose only import
// is stdlib "fmt", resolved via the fallback ExportSource tier
// (stdlibExportSource).
func TestCheckPackage_StdlibExportFile(t *testing.T) {
	fset := token.NewFileSet()
	f := parseTestdata(t, fset, "dep/dep.go")

	cache := NewCache()
	imp := NewImporter(fset, nil, newStdlibExportSource(t), cache)

	pkg, info, errs := CheckPackage(fset, []*ast.File{f}, "example.com/tcmod/dep", imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors: %v", errs)
	}
	if pkg == nil || !pkg.Complete() {
		t.Fatalf("expected a complete package, got %v", pkg)
	}
	if len(info.Defs) == 0 {
		t.Error("info.Defs is empty, want Greet's declaration recorded")
	}
}

// TestCheckPackage_ExportSourceDependency checks a package whose workspace
// dependency is resolved from a self-authored WriteExport blob via the
// primary ExportSource, and whose stdlib dependency still resolves via the
// fallback tier (stdlibExportSource).
func TestCheckPackage_ExportSourceDependency(t *testing.T) {
	fset := token.NewFileSet()

	depFile := parseTestdata(t, fset, "dep/dep.go")
	cache := NewCache()
	depImp := NewImporter(fset, nil, newStdlibExportSource(t), cache)
	depPkg, _, errs := CheckPackage(fset, []*ast.File{depFile}, "example.com/tcmod/dep", depImp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors checking dep: %v", errs)
	}

	blob, err := WriteExport(depPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	userFile := parseTestdata(t, fset, "user/user.go")
	src := blobSource{blobs: map[string][]byte{"example.com/tcmod/dep": blob}}
	userImp := NewImporter(fset, src, newStdlibExportSource(t), cache)
	userPkg, _, errs := CheckPackage(fset, []*ast.File{userFile}, "example.com/tcmod/user", userImp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors checking user: %v", errs)
	}
	if userPkg == nil || !userPkg.Complete() {
		t.Fatalf("expected a complete package, got %v", userPkg)
	}

	msg := userPkg.Scope().Lookup("Message")
	if msg == nil {
		t.Fatal("user.Message not found in package scope")
	}
}

// TestReadExport round-trips WriteExport's output through ReadExport
// directly, without going through an Importer.
func TestReadExport(t *testing.T) {
	fset := token.NewFileSet()
	depFile := parseTestdata(t, fset, "dep/dep.go")
	cache := NewCache()
	imp := NewImporter(fset, nil, newStdlibExportSource(t), cache)
	depPkg, _, errs := CheckPackage(fset, []*ast.File{depFile}, "example.com/tcmod/dep", imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors: %v", errs)
	}

	blob, err := WriteExport(depPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	readFset := token.NewFileSet()
	readCache := NewCache()
	got, err := ReadExport(blob, readFset, "example.com/tcmod/dep", readCache)
	if err != nil {
		t.Fatalf("ReadExport: %v", err)
	}
	if got.Scope().Lookup("Greet") == nil {
		t.Error("decoded package missing Greet")
	}
}

// TestReadExport_CachesSuccessWithoutRedecoding confirms a second
// ReadExport call for the same pkgPath against the same cache does not
// re-run gcexportdata.Read: Cache.Decodes() must stay at 1 after the
// second call.
func TestReadExport_CachesSuccessWithoutRedecoding(t *testing.T) {
	fset := token.NewFileSet()
	depFile := parseTestdata(t, fset, "dep/dep.go")
	writeCache := NewCache()
	imp := NewImporter(fset, nil, newStdlibExportSource(t), writeCache)
	depPkg, _, errs := CheckPackage(fset, []*ast.File{depFile}, "example.com/tcmod/dep", imp)
	if len(errs) != 0 {
		t.Fatalf("unexpected type errors: %v", errs)
	}
	blob, err := WriteExport(depPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	readFset := token.NewFileSet()
	readCache := NewCache()
	if _, err := ReadExport(blob, readFset, "example.com/tcmod/dep", readCache); err != nil {
		t.Fatalf("first ReadExport: %v", err)
	}
	if got := readCache.Decodes(); got != 1 {
		t.Fatalf("Decodes() after first ReadExport = %d, want 1", got)
	}
	if _, err := ReadExport(blob, readFset, "example.com/tcmod/dep", readCache); err != nil {
		t.Fatalf("second ReadExport: %v", err)
	}
	if got := readCache.Decodes(); got != 1 {
		t.Errorf("Decodes() after second ReadExport = %d, want still 1 (repeat call must be served from cache.pkgs)", got)
	}
}

// TestReadExport_CachesFailure confirms a pkgPath whose export data fails
// to decode has that failure cached: a second call for the same pkgPath
// must return the SAME error without attempting gcexportdata.Read again,
// closing the field symptom of a ~1s decode cost repeating on every query
// for a package that can never successfully decode.
func TestReadExport_CachesFailure(t *testing.T) {
	fset := token.NewFileSet()
	cache := NewCache()
	badData := []byte("not export data")

	_, err1 := ReadExport(badData, fset, "example.com/broken", cache)
	if err1 == nil {
		t.Fatal("ReadExport with malformed data: got nil error, want a decode error")
	}
	if got := cache.FailedLen(); got != 1 {
		t.Fatalf("FailedLen() after first failed ReadExport = %d, want 1", got)
	}

	_, err2 := ReadExport(badData, fset, "example.com/broken", cache)
	if err2 == nil || err2.Error() != err1.Error() {
		t.Errorf("second ReadExport error = %v, want the identical cached error %v", err2, err1)
	}
	if got := cache.FailedLen(); got != 1 {
		t.Errorf("FailedLen() after second failed ReadExport = %d, want still 1", got)
	}
}

// TestCache_DeleteClearsFailure confirms Delete drops a cached decode
// failure too, not just a cached success, so a package reindexed after an
// earlier decode failure gets a genuine retry instead of the stale error
// forever (mirroring Resolver.Invalidate's existing contract for a
// successful decode).
func TestCache_DeleteClearsFailure(t *testing.T) {
	fset := token.NewFileSet()
	cache := NewCache()
	if _, err := ReadExport([]byte("not export data"), fset, "example.com/broken", cache); err == nil {
		t.Fatal("expected a decode error")
	}
	if got := cache.FailedLen(); got != 1 {
		t.Fatalf("FailedLen() before Delete = %d, want 1", got)
	}

	cache.Delete("example.com/broken")

	if got := cache.FailedLen(); got != 0 {
		t.Errorf("FailedLen() after Delete = %d, want 0", got)
	}
}

// TestCheckPackage_CollectsErrors verifies every type error is collected,
// not just the first.
func TestCheckPackage_CollectsErrors(t *testing.T) {
	fset := token.NewFileSet()
	f := parseTestdata(t, fset, "broken/broken.go")
	cache := NewCache()
	imp := NewImporter(fset, nil, newStdlibExportSource(t), cache)

	pkg, _, errs := CheckPackage(fset, []*ast.File{f}, "example.com/tcmod/broken", imp)
	if len(errs) == 0 {
		t.Fatal("expected at least one type error")
	}
	if pkg == nil {
		t.Fatal("expected a non-nil package even with type errors")
	}
}
