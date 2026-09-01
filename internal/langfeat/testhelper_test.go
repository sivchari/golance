package langfeat_test

import (
	"context"
	"go/token"
	"go/types"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
)

// newCheckedPackage loads testdata/module's pkgDir subpackage through a
// real check.Engine (graph.Load + typecheck) and returns its
// CheckedPackage along with the absolute path to file within pkgDir.
func newCheckedPackage(t *testing.T, reader overlay.FileReader, pkgDir, file string) (*check.CheckedPackage, string) {
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
	depFset := token.NewFileSet()
	depCache := typecheck.NewCache()
	imp := func() types.ImporterFrom {
		return typecheck.NewImporter(depFset, nil, snap, depCache)
	}
	engine := check.New(src, reader, imp, check.Options{})

	path := filepath.Join(root, pkgDir, file)
	cp, err := engine.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get(%s): %v", path, err)
	}
	return cp, path
}
