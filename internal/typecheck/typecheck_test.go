package typecheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"
)

// stdlibExportFiles resolves stdlib packages via a scoped go/packages.Load
// (the same non-deprecated mechanism internal/graph.Snapshot's own
// reloadExportFile uses), independent of the module this test runs in — it
// exercises the same GOCACHE export-file path internal/graph.Snapshot uses,
// without depending on internal/graph.
type stdlibExportFiles struct{}

func (stdlibExportFiles) ExportFile(pkgPath string) (string, bool) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedExportFile}
	pkgs, err := packages.Load(cfg, pkgPath)
	if err != nil || len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
		return "", false
	}
	file := pkgs[0].ExportFile
	return file, file != ""
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
// is stdlib "fmt", resolved via ExportFileSource (GOCACHE export file).
func TestCheckPackage_StdlibExportFile(t *testing.T) {
	fset := token.NewFileSet()
	f := parseTestdata(t, fset, "dep/dep.go")

	cache := NewCache()
	imp := NewImporter(fset, nil, stdlibExportFiles{}, cache)

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
// dependency is resolved from a self-authored WriteExport blob via
// ExportSource, and whose stdlib dependency still resolves via
// ExportFileSource.
func TestCheckPackage_ExportSourceDependency(t *testing.T) {
	fset := token.NewFileSet()

	depFile := parseTestdata(t, fset, "dep/dep.go")
	cache := NewCache()
	depImp := NewImporter(fset, nil, stdlibExportFiles{}, cache)
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
	userImp := NewImporter(fset, src, stdlibExportFiles{}, cache)
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
	imp := NewImporter(fset, nil, stdlibExportFiles{}, cache)
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
	imp := NewImporter(fset, nil, stdlibExportFiles{}, writeCache)
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
	imp := NewImporter(fset, nil, stdlibExportFiles{}, cache)

	pkg, _, errs := CheckPackage(fset, []*ast.File{f}, "example.com/tcmod/broken", imp)
	if len(errs) == 0 {
		t.Fatal("expected at least one type error")
	}
	if pkg == nil {
		t.Fatal("expected a non-nil package even with type errors")
	}
}
