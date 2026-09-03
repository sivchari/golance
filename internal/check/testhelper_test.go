package check

import (
	"go/token"
	"go/types"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/depexport"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/overlay"
	"github.com/sivchari/golance/internal/typecheck"
)

// newTestEngine loads the real testdata/module graph and returns an Engine
// over it, plus the module's absolute root directory. reader is typically
// overlay.New() or a wrapper around one.
func newTestEngine(t *testing.T, reader overlay.FileReader, opts Options) (*Engine, string) {
	t.Helper()
	return newTestEngineWithImporterHook(t, reader, opts, nil)
}

// newTestEngineWithImporterHook is like newTestEngine but, if hook is
// non-nil, wraps the Importer factory with it before building the Engine —
// e.g. to count or gate individual recheck calls (Importer's doc: Engine
// calls it exactly once per recheck, so wrapping it is a precise hook for
// "how many rechecks actually ran").
func newTestEngineWithImporterHook(t *testing.T, reader overlay.FileReader, opts Options, hook func(Importer) Importer) (*Engine, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	src := NewGraphSource(snap, reader)
	depFset := token.NewFileSet()
	depCache := typecheck.NewCache()
	depMeta := depcheck.NewGraphMetadataSource(snap)
	depExp := depexport.NewCache(nil, depMeta, depcheck.NewProvider(depMeta, depcheck.Options{}), depexport.Options{})
	imp := Importer(func() types.ImporterFrom {
		return typecheck.NewImporter(depFset, nil, depExp, depCache)
	})
	if hook != nil {
		imp = hook(imp)
	}
	return New(src, reader, imp, opts), root
}
