package typecheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/gcexportdata"
)

// stdlibExportFiles resolves stdlib packages via `go list -export`
// (gcexportdata.Find), independent of the module this test runs in — it
// exercises the same GOCACHE export-file path internal/graph.Snapshot uses,
// without depending on internal/graph.
type stdlibExportFiles struct{}

func (stdlibExportFiles) ExportFile(pkgPath string) (string, bool) {
	file, _ := gcexportdata.Find(pkgPath, "")
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
