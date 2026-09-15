package check

import (
	"context"
	"go/types"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sivchari/golance/internal/overlay"
)

// recursiveRootImporter wraps a base Importer with an ImportFrom that
// resolves target by recursively calling back into the very Engine it is
// serving (via engine.GetPackage) instead of falling through to base — the
// same shape internal/server's rootAwareImporter uses in production to
// route a ROOT-package import through Engine's own cache. engine is set
// once, after newTestEngineWithImporterHook returns the Engine this
// importer feeds (a two-phase construction unavoidable here, mirroring
// internal/server's own: the hook must exist before New returns an Engine
// to recurse into).
type recursiveRootImporter struct {
	base   Importer
	engine *Engine
	target string
	calls  *int64
}

func (r *recursiveRootImporter) importer(ctx context.Context) types.ImporterFrom {
	return recursiveRootImporterFrom{ctx: ctx, base: r.base(ctx), engine: r.engine, target: r.target, calls: r.calls}
}

type recursiveRootImporterFrom struct {
	ctx    context.Context
	base   types.ImporterFrom
	engine *Engine
	target string
	calls  *int64
}

func (r recursiveRootImporterFrom) Import(path string) (*types.Package, error) {
	return r.ImportFrom(path, "", 0)
}

func (r recursiveRootImporterFrom) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path == r.target {
		atomic.AddInt64(r.calls, 1)
		cp, err := r.engine.GetPackage(r.ctx, path)
		if err != nil {
			return nil, err
		}
		return cp.Package(), nil
	}
	return r.base.ImportFrom(path, dir, mode)
}

// TestEngine_Get_RecursiveGetPackageResolvesRootImport covers GetPackage's
// intended use: an Importer that, while running INSIDE runRecheck for one
// unit (rootuser), recursively calls back into the same Engine via
// GetPackage to resolve a different unit (basic) that unit imports. Before
// GetPackage existed, a caller in this position had no way to resolve a
// root-to-root import through Engine's own cache at all — every prior
// caller of e.newImporter always resolved dependencies through a separate,
// non-recursive path (typecheck.Importer/depexport.Cache). This pins two
// things at once: the recursive call does not deadlock (Get/GetPackage
// never hold e.mu across their own blocking wait — see getUnit), and the
// *types.Package it returns is pointer-identical to what a direct Get for
// the same package produces, proving both routes share the same cache
// entry.
func TestEngine_Get_RecursiveGetPackageResolvesRootImport(t *testing.T) {
	var basicResolveCalls int64
	ri := &recursiveRootImporter{target: "example.com/checkmod/basic", calls: &basicResolveCalls}
	e, root := newTestEngineWithImporterHook(t, overlay.New(), Options{}, func(imp Importer) Importer {
		ri.base = imp
		return ri.importer
	})
	ri.engine = e

	ctx := context.Background()
	rootuserFile := filepath.Join(root, "rootuser", "rootuser.go")
	cp, err := e.Get(ctx, rootuserFile)
	if err != nil {
		t.Fatalf("Get(rootuser): %v", err)
	}
	if got := atomic.LoadInt64(&basicResolveCalls); got != 1 {
		t.Errorf("recursive ImportFrom(basic) called %d times, want exactly 1", got)
	}

	basicFile := filepath.Join(root, "basic", "basic.go")
	basicCP, err := e.Get(ctx, basicFile)
	if err != nil {
		t.Fatalf("Get(basic): %v", err)
	}

	var basicImport *types.Package
	for _, imp := range cp.Package().Imports() {
		if imp.Path() == "example.com/checkmod/basic" {
			basicImport = imp
		}
	}
	if basicImport == nil {
		t.Fatal("rootuser's checked package has no basic import recorded")
	}
	if basicImport != basicCP.Package() {
		t.Error("recursive GetPackage resolved a *types.Package instance different from a direct Get for the same package — identity broken")
	}
	if got := basicImport.Scope().Lookup("Add"); got == nil {
		t.Error("resolved basic package's scope has no Add declaration")
	}
}

// thrashModulePrefix identifies every package under testdata/module/thrash,
// the fixture TestEngine_Get_RootImportCacheDoesNotThrashUnderDefaultMaxLRU
// uses.
const thrashModulePrefix = "example.com/checkmod/thrash/"

// thrashRootImporter is recursiveRootImporter's counterpart for that test:
// instead of routing a single named target through GetPackage, it routes
// EVERY import under thrashModulePrefix through it — the fixture's own
// closure is what exercises eviction, not the importer's own selectivity.
// pkgcountCalls counts ImportFrom("example.com/checkmod/thrash/pkgcount")
// calls specifically: pkgcount is shared's only import, so a call into it
// only ever happens when shared itself is actually (re)checked, never when
// shared is served from Engine's cache (a cache hit returns its
// already-resolved *types.Package without re-walking its own imports at
// all). This is the test's precise "was shared rechecked" signal.
type thrashRootImporter struct {
	base          Importer
	engine        *Engine
	pkgcountCalls *int64
}

func (t *thrashRootImporter) importer(ctx context.Context) types.ImporterFrom {
	return thrashRootImporterFrom{ctx: ctx, base: t.base(ctx), engine: t.engine, pkgcountCalls: t.pkgcountCalls}
}

type thrashRootImporterFrom struct {
	ctx           context.Context
	base          types.ImporterFrom
	engine        *Engine
	pkgcountCalls *int64
}

func (t thrashRootImporterFrom) Import(path string) (*types.Package, error) {
	return t.ImportFrom(path, "", 0)
}

func (t thrashRootImporterFrom) ImportFrom(path, dir string, mode types.ImportMode) (*types.Package, error) {
	if path == thrashModulePrefix+"pkgcount" {
		atomic.AddInt64(t.pkgcountCalls, 1)
	}
	if strings.HasPrefix(path, thrashModulePrefix) {
		cp, err := t.engine.GetPackage(t.ctx, path)
		if err != nil {
			return nil, err
		}
		return cp.Package(), nil
	}
	return t.base.ImportFrom(path, dir, mode)
}

// TestEngine_Get_RootImportCacheThrashingIsACacheSizingProblem
// characterizes the thrashing GetPackage's routing through Engine's own
// e.cache produces once a single import closure exceeds the cache's own
// entry-count cap: testdata/module/thrash/top imports leafa (which resolves
// shared first), then seven filler packages, then shared again directly —
// nine distinct units besides top itself, one more than defaultMaxLRU (6).
// The seven filler packages committed between shared's two uses evict
// shared's own cache entry once the cache is too small to hold all nine,
// forcing top's own later direct import to recheck it — and shared's
// recheck re-walks its own import of pkgcount, exactly what pkgcountCalls
// counts. Engine's own content-hash cache-hit path (getUnit) never
// re-invokes an Importer at all, so a warm hit can never itself call
// ImportFrom(pkgcount) a second time; only an actual recheck can.
//
// This exercises exactly the failure mode depcheck.RecommendedCap's own doc
// describes for the identical reason (a single dependency closure larger
// than a small, entry-count LRU thrashes it, turning "check every distinct
// package once" into "recheck a widely-shared package every time a new
// importer reaches it again"): the "cap too small" case below pins that this
// is a real, reproducible property of Engine's own LRU once GetPackage
// routes a root closure through it, and the "cap large enough" case pins
// the inverse: the same closure, given a cap that actually fits it, checks
// every distinct unit exactly once.
//
// internal/server's own rootFallback Engine (see setWorkspace's own
// construction) deliberately accepts the "cap too small" case's tradeoff
// rather than the "cap large enough" one: it is left at check.Engine's own
// small, editor-session default rather than sized to the workspace's own
// root-package count, so worst-case memory stays bounded independent of
// workspace size (the alternative measured 10-25GB server RSS on a large
// monorepo — see internal/server's own cold-start RSS investigation notes)
// at the cost of some thrashing under a closure with many simultaneously
// stale root packages at once. This test exists to make that accepted
// tradeoff, and Engine's own underlying LRU behavior it rests on, explicit
// and regression-tested, not to dictate a specific production cap.
func TestEngine_Get_RootImportCacheThrashingIsACacheSizingProblem(t *testing.T) {
	tests := []struct {
		name        string
		maxLRU      int
		wantThrash  bool
		description string
	}{
		{name: "cap too small for the nine-unit closure", maxLRU: defaultMaxLRU, wantThrash: true},
		{name: "cap large enough for the nine-unit closure", maxLRU: 32, wantThrash: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pkgcountCalls int64
			ri := &thrashRootImporter{pkgcountCalls: &pkgcountCalls}
			e, root := newTestEngineWithImporterHook(t, overlay.New(), Options{MaxLRU: tt.maxLRU}, func(imp Importer) Importer {
				ri.base = imp
				return ri.importer
			})
			ri.engine = e

			topFile := filepath.Join(root, "thrash", "top", "top.go")
			if _, err := e.Get(context.Background(), topFile); err != nil {
				t.Fatalf("Get(top): %v", err)
			}
			got := atomic.LoadInt64(&pkgcountCalls)
			if tt.wantThrash && got <= 1 {
				t.Errorf("pkgcount resolved via shared %d times with MaxLRU=%d, want more than 1 (this cap is too small to hold the closure, so this case should reproduce the thrash — if it no longer does, MaxLRU=%d silently grew large enough to hide the failure mode this test pins, or Engine's eviction algorithm changed)", got, tt.maxLRU, tt.maxLRU)
			}
			if !tt.wantThrash && got != 1 {
				t.Errorf("pkgcount resolved via shared %d times with MaxLRU=%d, want exactly 1 (shared must stay cached across top's whole closure, not be evicted and rechecked)", got, tt.maxLRU)
			}
		})
	}
}
