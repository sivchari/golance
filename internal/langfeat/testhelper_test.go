package langfeat_test

import (
	"context"
	"go/token"
	"go/types"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/depexport"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
)

// newCheckedPackage loads testdata/module's pkgDir subpackage through a
// real check.Engine (graph.Load + typecheck) and returns its
// CheckedPackage along with the absolute path to file within pkgDir.
func newCheckedPackage(t *testing.T, reader overlay.FileReader, pkgDir, file string) (*check.CheckedPackage, string) {
	t.Helper()
	engine, root := newCheckEngine(t, reader)
	path := filepath.Join(root, pkgDir, file)
	cp, err := engine.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get(%s): %v", path, err)
	}
	return cp, path
}

// newCheckedPackageOverlay is newCheckedPackage's counterpart for a file
// that exists only in the editor overlay, never on disk -- mirroring an LSP
// didOpen for a brand-new, unsaved buffer inside a known package directory
// (pkgDir). content is opened via reader.DidOpen before the engine ever
// resolves pkgDir's file set, matching production's didOpen-before-Get
// ordering (see internal/server's handleDidOpen), so the overlay-only file
// is already visible the first time Get lists candidates.
func newCheckedPackageOverlay(t *testing.T, reader *overlay.Overlay, pkgDir, file, content string) (*check.CheckedPackage, string) {
	t.Helper()
	engine, root := newCheckEngine(t, reader)
	path := filepath.Join(root, pkgDir, file)
	reader.DidOpen(&protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:  uri.File(path),
			Text: content,
		},
	})
	cp, err := engine.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get(%s): %v", path, err)
	}
	return cp, path
}

// newCheckEngine builds the real check.Engine (graph.Load + typecheck)
// newCheckedPackage/newCheckedPackageOverlay share, and returns it along
// with testdata/module's absolute root.
func newCheckEngine(t *testing.T, reader overlay.FileReader) (*check.Engine, string) {
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
	depMeta := depcheck.NewGraphMetadataSource(snap)
	depExp := depexport.NewCache(nil, depMeta, depcheck.NewProvider(depMeta, depcheck.Options{}), depexport.Options{})
	imp := func(context.Context) types.ImporterFrom {
		return typecheck.NewImporter(depFset, nil, depExp, depCache)
	}
	return check.New(src, reader, imp, check.Options{}), root
}
