package server

import (
	"context"
	"go/types"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
)

// TestEngine_RootToRootImport_DoesNotSourceCheckThroughDepexport is the
// regression test for the warm go-to-definition/hover hang: package a
// (testdata/rootimport/a) imports its sibling root package b — both loaded
// via "./..." from the same module, so graph.Load marks both Root. Before
// the fix, ws.engine's own dependency Importer resolved a root import
// through depCacheHolder -> depexport.Cache, which has no persistent CAS
// entry for a root (workspace) directory and so always ran a full,
// uncached, from-source check of b's own transitive closure
// (depexport.Cache.checkAndPersist, via s.depExportProviderVal) on every
// single query. After the fix, a root import with no facts-index data yet
// (as here — no index.Build ever ran) is resolved through rootAwareImporter's
// getRoot tier, ws.rootFallback's own small, SEPARATE Get/commit cache (see
// setWorkspace's own construction of it), so s.depExportProviderVal is never
// asked to check anything. The facts index is marked ready (s.idx.Store)
// because engineImporter only applies this routing once it is — see its own
// doc: during a cold build, a root import stays unresolved instead, exactly
// as it did before this fix (see the "NoIndex_OtherWorkspacePackage" family
// of tests, which pin that unchanged cold-build behavior).
func TestEngine_RootToRootImport_DoesNotSourceCheckThroughDepexport(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "rootimport"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	s.idx.Store(&indexState{})

	ws := s.workspace()
	aFile := filepath.Join(root, "a", "a.go")
	ctx := context.Background()

	cp, err := ws.engine.Get(ctx, aFile)
	if err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	if got := s.depExportProviderVal.Checked(); got != 0 {
		t.Errorf("depExportProviderVal.Checked() = %d, want 0 (a root-to-root import must never reach depexport.Cache's from-source check)", got)
	}

	var bImport *types.Package
	for _, imp := range cp.Package().Imports() {
		if imp.Path() == "example.com/rootimport/b" {
			bImport = imp
		}
	}
	if bImport == nil {
		t.Fatal("a's checked package has no b import recorded")
	}
	if bImport.Scope().Lookup("Value") == nil {
		t.Error("resolved b package's scope has no Value declaration")
	}

	// A direct GetPackage for b against ws.rootFallback — the same Engine
	// rootAwareImporter's getRoot tier resolved b through above — must reuse
	// the exact same cache entry, not trigger a second recheck: identity is
	// shared WITHIN rootFallback's own cache (see rootAwareImporter's own
	// doc), which is what this pins; it is NOT shared with ws.engine's own
	// cache (a separate Engine entirely — see setWorkspace's own doc for
	// why), so this deliberately checks rootFallback, not engine, for b.
	bCP, err := ws.rootFallback.GetPackage(ctx, "example.com/rootimport/b")
	if err != nil {
		t.Fatalf("rootFallback.GetPackage(b): %v", err)
	}
	if bCP.Package() != bImport {
		t.Error("rootFallback.GetPackage(b) returned a *types.Package different from the one a's own import resolved to — identity broken within rootFallback's own cache")
	}
	if got := s.depExportProviderVal.Checked(); got != 0 {
		t.Errorf("depExportProviderVal.Checked() = %d after GetPackage(b), want 0", got)
	}
}

// TestEngine_RootToRootImport_ColdBuildLeavesImportUnresolved is
// engineImporter's cold-build counterpart: while the facts index is not yet
// ready (s.idx is never stored), a root-to-root import must stay unresolved
// exactly as it did before rootAwareImporter existed — see
// TestHandleDefinition_NoIndex_OtherWorkspacePackage and its sibling tests,
// which pin the same cold-build behavior at the handler level. Resolving it
// eagerly here instead would reintroduce, via decodeRoot/rootFallback.
// GetPackage recursion, the same kind of server-side memory pressure the
// cold-index-build gate exists to avoid while the indexer subprocess is
// already checking the same packages.
func TestEngine_RootToRootImport_ColdBuildLeavesImportUnresolved(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "rootimport"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)

	ws := s.workspace()
	aFile := filepath.Join(root, "a", "a.go")
	cp, err := ws.engine.Get(context.Background(), aFile)
	if err != nil {
		t.Fatalf("Get(a): %v", err)
	}

	// go/types still adds a placeholder *types.Package to Imports() for an
	// import ImportFrom failed to resolve (an empty-scope stand-in, not the
	// real b) — so "left unresolved" is checked by b's declarations being
	// absent, not by b's own entry being absent from Imports() at all.
	for _, imp := range cp.Package().Imports() {
		if imp.Path() == "example.com/rootimport/b" && imp.Scope().Lookup("Value") != nil {
			t.Error("a's checked package fully resolved b (Value found) during a cold build, want it left unresolved")
		}
	}
	if got := s.depExportProviderVal.Checked(); got != 0 {
		t.Errorf("depExportProviderVal.Checked() = %d during a cold build, want 0", got)
	}
}
