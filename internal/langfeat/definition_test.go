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
	src := check.NewGraphSource(snap)
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
