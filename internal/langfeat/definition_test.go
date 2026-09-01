package langfeat_test

import (
	"context"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
)

// newCheckedPackageWithDepFset is newCheckedPackage plus the dependency
// importer's own *token.FileSet, which DependencyDefinition needs to
// resolve an imported object's export-data position (see
// internal/server's depCacheHolder.FileSet, the production equivalent of
// depFset here).
func newCheckedPackageWithDepFset(t *testing.T, reader overlay.FileReader, pkgDir, file string) (cp *check.CheckedPackage, path string, depFset *token.FileSet) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	src := check.NewGraphSource(snap, reader)
	depFset = token.NewFileSet()
	depCache := typecheck.NewCache()
	imp := func() types.ImporterFrom {
		return typecheck.NewImporter(depFset, nil, snap, depCache)
	}
	engine := check.New(src, reader, imp, check.Options{})

	path = filepath.Join(root, pkgDir, file)
	cp, err = engine.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get(%s): %v", path, err)
	}
	return cp, path, depFset
}

func TestDependencyDefinition_Stdlib(t *testing.T) {
	reader := overlay.New()
	cp, path, depFset := newCheckedPackageWithDepFset(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	tests := []struct {
		name        string
		substr      string
		wantPkgPath string
		wantSuffix  string
	}{
		{name: "strings.Builder", substr: "strings.Builder", wantPkgPath: "strings", wantSuffix: filepath.FromSlash("strings/builder.go")},
		{name: "fmt.Sprintf", substr: "fmt.Sprintf", wantPkgPath: "fmt", wantSuffix: filepath.FromSlash("fmt/print.go")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset := mustIndex(t, text, tt.substr) + len(strings.SplitN(tt.substr, ".", 2)[0]) + 1

			got, err := langfeat.DependencyDefinition(cp, depFset, path, offset)
			if err != nil {
				t.Fatalf("DependencyDefinition: %v", err)
			}
			if got == nil {
				t.Fatal("DependencyDefinition returned nil, want a result")
			}
			if got.PkgPath != tt.wantPkgPath {
				t.Errorf("PkgPath = %q, want %q", got.PkgPath, tt.wantPkgPath)
			}
			if !strings.HasSuffix(got.Filename, tt.wantSuffix) {
				t.Errorf("Filename = %q, want it to end with %q", got.Filename, tt.wantSuffix)
			}
			if got.Line <= 0 {
				t.Errorf("Line = %d, want > 0", got.Line)
			}
			if _, err := os.Stat(got.Filename); err != nil {
				t.Errorf("resolved file %s does not exist on disk: %v", got.Filename, err)
			}
		})
	}
}

// TestDependencyDefinition_EmbeddedStdlibField covers "Go to Definition" on
// an embedded struct field naming a standard-library type: the mirror image
// of TestSamePackageDefinition_EmbeddedField for the cross-package/export-
// data path. Before embeddedFieldTarget, ObjectOf(id) returned the implicit
// field var, whose Pkg() is the EMBEDDING package (not bytes) -- so
// DependencyDefinition declined outright (obj.Pkg() == cp.Package()), and
// definitionFallback's SamePackageDefinition call (tried first) resolved
// the same wrong object to itself instead, per
// TestSamePackageDefinition_EmbeddedField's doc.
func TestDependencyDefinition_EmbeddedStdlibField(t *testing.T) {
	reader := overlay.New()
	cp, path, depFset := newCheckedPackageWithDepFset(t, reader, "embed", "embedstruct_stdlib.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "bytes.Buffer\n") + len("bytes.")

	got, err := langfeat.DependencyDefinition(cp, depFset, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("DependencyDefinition returned nil, want bytes.Buffer's declaration")
	}
	if got.PkgPath != "bytes" {
		t.Errorf("PkgPath = %q, want %q", got.PkgPath, "bytes")
	}
	if !strings.HasSuffix(got.Filename, filepath.FromSlash("bytes/buffer.go")) {
		t.Errorf("Filename = %q, want it to end with bytes/buffer.go", got.Filename)
	}
}

// TestDependencyDefinition_EmbeddedStdlibInterface covers "Go to
// Definition" on an embedded interface naming a standard-library interface
// (io.Reader): unlike a struct field, an embedded interface element never
// declares an implicit types.Var, so ObjectOf(id) already resolved straight
// to Uses[id] before embeddedFieldTarget existed -- this pins that this
// case needed no fix and still passes through unaffected.
func TestDependencyDefinition_EmbeddedStdlibInterface(t *testing.T) {
	reader := overlay.New()
	cp, path, depFset := newCheckedPackageWithDepFset(t, reader, "embed", "embediface.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "io.Reader\n") + len("io.")

	got, err := langfeat.DependencyDefinition(cp, depFset, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("DependencyDefinition returned nil, want io.Reader's declaration")
	}
	if got.PkgPath != "io" {
		t.Errorf("PkgPath = %q, want %q", got.PkgPath, "io")
	}
}

func TestDependencyDefinition_SamePackageReturnsNil(t *testing.T) {
	reader := overlay.New()
	cp, path, depFset := newCheckedPackageWithDepFset(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// UseStdlib's own declaring identifier is in cp's own package: the
	// workspace facts index already has a better answer for this case, so
	// DependencyDefinition should decline rather than offer a
	// column-degraded substitute.
	offset := mustIndex(t, text, "func UseStdlib") + len("func ")

	got, err := langfeat.DependencyDefinition(cp, depFset, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("DependencyDefinition = %+v, want nil (declared in cp's own package)", got)
	}
}

// TestSamePackageDefinition_ResolvesLocalIdentifier verifies
// SamePackageDefinition's positive case: an identifier declared in cp's
// own package resolves to its exact declaring identifier, using only cp's
// own AST/types.Info/FileSet — the case DependencyDefinition declines (see
// TestDependencyDefinition_SamePackageReturnsNil).
func TestSamePackageDefinition_ResolvesLocalIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "hover", "hover.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// DefaultGreeting's initializer references Greeting, declared earlier
	// in the same file.
	offset := mustIndex(t, text, "= Greeting{") + len("= ")

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("SamePackageDefinition returned nil, want a result")
	}
	if got.File != path {
		t.Errorf("File = %q, want %q", got.File, path)
	}
	wantOffset := mustIndex(t, text, "type Greeting") + len("type ")
	if got.Range.StartOffset != wantOffset {
		t.Errorf("Range.StartOffset = %d, want %d (Greeting's declaring identifier)", got.Range.StartOffset, wantOffset)
	}
}

// TestSamePackageDefinition_CrossPackageReturnsNil verifies
// SamePackageDefinition declines an identifier declared outside cp's own
// package, the mirror image of TestDependencyDefinition_SamePackageReturnsNil.
func TestSamePackageDefinition_CrossPackageReturnsNil(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "strings.Builder") + len("strings.")

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("SamePackageDefinition = %+v, want nil (declared in a different package)", got)
	}
}

// TestSamePackageDefinition_EmbeddedField covers "Go to Definition" invoked
// on an embedded struct field's name, declared in cp's own package: per
// gopls (golang/go#42254), it must jump to the embedded TYPE's own
// declaration. Before embeddedFieldTarget existed, ObjectOf(id) returned
// the implicit field types.Var types.Info.Defs records at the SAME
// position as the identifier itself, so this resolved to itself instead of
// leaving the cursor's current position -- see embeddedFieldTarget's doc.
func TestSamePackageDefinition_EmbeddedField(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "embed", "embed.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\tBase\n") + 1

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("SamePackageDefinition returned nil, want Base's declaration")
	}
	wantOffset := mustIndex(t, text, "type Base") + len("type ")
	if got.Range.StartOffset != wantOffset {
		t.Errorf("Range.StartOffset = %d, want %d (Base's declaring identifier, not the embedded field itself)", got.Range.StartOffset, wantOffset)
	}
}

func TestSamePackageDefinition_NoIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "hover", "hover.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\n\n// Greeting")

	got, err := langfeat.SamePackageDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("SamePackageDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("SamePackageDefinition = %+v, want nil (no identifier at offset)", got)
	}
}

func TestDependencyDefinition_NoIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path, depFset := newCheckedPackageWithDepFset(t, reader, "depuse", "depuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\n\n// UseStdlib")

	got, err := langfeat.DependencyDefinition(cp, depFset, path, offset)
	if err != nil {
		t.Fatalf("DependencyDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("DependencyDefinition = %+v, want nil (no identifier at offset)", got)
	}
}
