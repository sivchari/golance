package index

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// mustCheckCacheGenSrc parses and type-checks src as pkgPath against a nil
// importer, failing the test on any error — src is expected to be
// self-contained (no imports).
func mustCheckCacheGenSrc(t *testing.T, fset *token.FileSet, pkgPath, src string) *types.Package {
	t.Helper()
	f, err := parser.ParseFile(fset, pkgPath+".go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", pkgPath, err)
	}
	conf := types.Config{}
	pkg, err := conf.Check(pkgPath, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("check %s: %v", pkgPath, err)
	}
	return pkg
}

// TestCacheGenerations_AcquireRotatesPastBudget confirms acquire returns the
// same generation while its cache stays within budget, and rotates to a
// fresh one — created via newGen, counted in count() — the moment a decode
// pushes it past budget.
func TestCacheGenerations_AcquireRotatesPastBudget(t *testing.T) {
	fset0 := token.NewFileSet()
	depPkg := mustCheckCacheGenSrc(t, fset0, "example.com/cachegen/dep", "package dep\n\ntype C struct{}\n")
	blob, err := typecheck.WriteExport(depPkg, fset0)
	if err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	var created []*cacheGeneration
	newGen := func() *cacheGeneration {
		fset := token.NewFileSet()
		cache := typecheck.NewCache()
		g := &cacheGeneration{fset: fset, cache: cache, imp: typecheck.NewImporter(fset, nil, nil, cache)}
		created = append(created, g)
		return g
	}

	gens := newCacheGenerations(0, newGen)
	if got := gens.count(); got != 1 {
		t.Fatalf("count() after construction = %d, want 1 (eager first generation)", got)
	}

	first := gens.acquire()
	if first != created[0] {
		t.Fatal("acquire() before any decode did not return the eagerly-created first generation")
	}

	if _, err := typecheck.ReadExport(blob, first.fset, depPkg.Path(), first.cache); err != nil {
		t.Fatalf("ReadExport: %v", err)
	}
	if first.cache.Bytes() <= 0 {
		t.Fatalf("cache.Bytes() = %d after decode, want > 0", first.cache.Bytes())
	}

	second := gens.acquire()
	if second == first {
		t.Fatal("acquire() returned the same generation after its cache exceeded budget, want rotation")
	}
	if got := gens.count(); got != 2 {
		t.Errorf("count() after rotation = %d, want 2", got)
	}

	third := gens.acquire()
	if third != second {
		t.Error("acquire() rotated again despite the new generation's cache still being empty (0 bytes, budget 0 not exceeded)")
	}
}

// generateDiamondModule writes a synthetic module modeling the real kiota
// shape (research-kiota-split.md): b is reached both directly (r2 imports
// it) and only indirectly, through a, by two separate roots (r and r3) —
// the exact combination that produced two non-identical *types.Package
// instances for one import path under the old per-entry eviction scheme.
func generateDiamondModule(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/diamond\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	pkgs := map[string]string{
		"b":  "package b\n\n// V returns 0.\nfunc V() int { return 0 }\n",
		"a":  "package a\n\nimport \"example.com/diamond/b\"\n\n// V returns b.V() + 1.\nfunc V() int { return b.V() + 1 }\n",
		"r":  "package r\n\nimport \"example.com/diamond/a\"\n\n// V returns a.V().\nfunc V() int { return a.V() }\n",
		"r2": "package r2\n\nimport \"example.com/diamond/b\"\n\n// V returns b.V().\nfunc V() int { return b.V() }\n",
		"r3": "package r3\n\nimport \"example.com/diamond/a\"\n\n// V returns a.V() + 1.\nfunc V() int { return a.V() + 1 }\n",
	}
	for name, src := range pkgs {
		pkgDir := filepath.Join(dir, name)
		if err := os.MkdirAll(pkgDir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", pkgDir, err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "p.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s/p.go: %v", name, err)
		}
	}
}

// TestBuild_GenerationRotationKeepsIdentityConsistent builds
// generateDiamondModule's fixture with DecodeCacheBudget set to force a
// generation rotation on essentially every decode, at both Parallelism 1
// (deterministic single-worker) and > 1 (real concurrent scheduling), and
// verifies every root's export round-trips cleanly — the regression this
// package's generational Cache design replaces per-entry eviction to fix
// (see research-kiota-split.md's DECISION section).
func TestBuild_GenerationRotationKeepsIdentityConsistent(t *testing.T) {
	roots := []string{
		"example.com/diamond/a",
		"example.com/diamond/b",
		"example.com/diamond/r",
		"example.com/diamond/r2",
		"example.com/diamond/r3",
	}

	for _, parallelism := range []int{1, 4} {
		t.Run(fmt.Sprintf("parallelism=%d", parallelism), func(t *testing.T) {
			dir := t.TempDir()
			generateDiamondModule(t, dir)

			snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
			if err != nil {
				t.Fatalf("graph.Load: %v", err)
			}
			db := openTestDB(t)
			cas := openTestCAS(t)

			opts := Options{
				Parallelism:       parallelism,
				DecodeCacheBudget: 1, // force rotation on virtually every decode
			}

			stats, err := Build(context.Background(), snap, db, cas, &opts)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if stats.Processed != len(roots) {
				t.Fatalf("Processed = %d, want %d", stats.Processed, len(roots))
			}
			if stats.Incomplete != 0 {
				t.Errorf("Incomplete = %d, want 0", stats.Incomplete)
			}
			if stats.CacheGenerations <= 1 {
				t.Errorf("CacheGenerations = %d, want > 1 (budget 1 byte should force rotation)", stats.CacheGenerations)
			}

			for _, root := range roots {
				assertExportRoundTrips(t, db, cas, root)
			}
		})
	}
}

// assertExportRoundTrips fetches root's persisted export blob from db/cas
// and confirms it decodes cleanly into a fresh, independent
// typecheck.Cache — the identity-split symptom (research-kiota-split.md)
// surfaces as a decode failure here, never as a build-time error.
func assertExportRoundTrips(t *testing.T, db *store.DB, cas *store.CAS, root string) {
	t.Helper()
	ptr, err := db.GetUnit(context.Background(), store.Hash(root))
	if err != nil {
		t.Fatalf("GetUnit(%s): %v", root, err)
	}
	blob, ok, err := cas.Get(context.Background(), ptr.BlobKey)
	if err != nil || !ok {
		t.Fatalf("cas.Get(%s): ok=%v err=%v", root, ok, err)
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		t.Fatalf("DecodeUnitBlob(%s): %v", root, err)
	}
	if len(u.Export) == 0 {
		t.Fatalf("%s has no export blob (was it withheld as unsafe to decode?)", root)
	}
	fresh := typecheck.NewCache()
	if _, err := typecheck.ReadExport(u.Export, token.NewFileSet(), root, fresh); err != nil {
		t.Errorf("%s export does not round-trip decode: %v", root, err)
	}
}
