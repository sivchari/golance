package server

import (
	"context"
	"go/types"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// newRootExportTestServer builds a Server over testdata/rootexport (a
// imports root package b, which itself imports root package c — a two-hop
// root-to-root chain) with a REAL facts index (index.Build, exactly like
// the indexer subprocess produces), so the index's own persisted
// store.UnitBlob.Export for b and c is available for engineImporter.
// decodeRoot to resolve from.
func newRootExportTestServer(t *testing.T) (s *Server, aFile string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "rootexport"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	relative := RelativeIndexPaths(root)
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	if _, err := index.Build(context.Background(), snap, db, cas, &index.Options{RelativePaths: relative}); err != nil {
		t.Fatalf("index.Build: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s = New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, relative)})

	return s, filepath.Join(root, "a", "a.go")
}

// TestEngineImporter_RootToRootImport_DecodesViaFactsIndexExportData is the
// regression test for the gopls-v0.12-style export-data persistence fix: a
// warm root-to-root import (a -> b -> c) must resolve by decoding the facts
// index's own already-persisted export data (rootExportSource, via
// engineImporter.decodeRoot) rather than a full check.Engine.GetPackage
// recheck of b's (and transitively c's) own source — proven two ways: (1)
// s.depExportProviderVal.Checked() stays 0, exactly like the pre-existing
// root-to-root regression test, since decodeRoot never touches depexport
// either; (2) ws.depCache.cache.Decodes() — the decode-cache b's blob lands
// in — goes above zero, which only ever happens via engineImporter.
// decodeRoot for a root package (check.Engine.GetPackage never touches
// depCache's cache at all, as the pre-existing test's own identical
// Checked()==0 assertion already established for the getRoot-only path).
// b.Value().Field resolving with no type errors, to the correct int type,
// confirms c's own Item.Field survived decode through b's export data
// without c's blob ever needing separate resolution.
func TestEngineImporter_RootToRootImport_DecodesViaFactsIndexExportData(t *testing.T) {
	s, aFile := newRootExportTestServer(t)
	ws := s.workspace()

	cp, err := ws.engine.Get(context.Background(), aFile)
	if err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	for _, d := range check.Diagnostics(cp, s.overlay) {
		t.Errorf("unexpected diagnostic checking a: %v", d)
	}

	if got := s.depExportProviderVal.Checked(); got != 0 {
		t.Errorf("depExportProviderVal.Checked() = %d, want 0 (a root-to-root import must never reach depexport.Cache's from-source check)", got)
	}
	if got := ws.depCache.cache.Decodes(); got == 0 {
		t.Error("depCache.cache.Decodes() = 0, want > 0 (b's export data must have been decoded via the facts index fast path)")
	}

	useB := cp.Package().Scope().Lookup("UseB")
	if useB == nil {
		t.Fatal("a's checked package has no UseB declaration")
	}
	sig, ok := useB.Type().(*types.Signature)
	if !ok {
		t.Fatalf("UseB's type = %T, want *types.Signature", useB.Type())
	}
	basic, ok := sig.Results().At(0).Type().(*types.Basic)
	if !ok || basic.Kind() != types.Int {
		t.Errorf("UseB's resolved return type = %v, want int (c.Item.Field must resolve through b's decoded export data)", sig.Results().At(0).Type())
	}
}

// TestEngineImporter_RootToRootImport_DirtyOverlayBypassesFactsIndexExportData
// is the invalidation-safety counterpart: b is open with an UNSAVED edit
// that changes Value's return type from c.Item to int (an intentionally
// breaking change relative to what the facts index was built against), so
// a's own reference to b.Value().Field is only a type error if a's
// type-check actually used the overlay's current content rather than the
// facts index's stale, on-disk-built export data. This is exactly the
// scenario rootExportSource.dirty exists to guard: reusing the persisted
// blob here would silently accept a's now-invalid field selector.
func TestEngineImporter_RootToRootImport_DirtyOverlayBypassesFactsIndexExportData(t *testing.T) {
	s, aFile := newRootExportTestServer(t)
	ws := s.workspace()
	root := filepath.Dir(filepath.Dir(aFile))
	bFile := filepath.Join(root, "b", "b.go")

	const editedB = `package b

// Value no longer returns c.Item: b is open with this unsaved edit.
func Value() int {
	return 1
}
`
	openDoc(t, s, bFile, editedB)

	cp, err := ws.engine.Get(context.Background(), aFile)
	if err != nil {
		t.Fatalf("Get(a): %v", err)
	}

	if got := ws.depCache.cache.Decodes(); got != 0 {
		t.Errorf("depCache.cache.Decodes() = %d, want 0 (an open, dirty b must never be resolved from the facts index's stale export data)", got)
	}

	if diags := check.Diagnostics(cp, s.overlay); len(diags) == 0 {
		t.Error("expected a diagnostic resolving b.Value().Field against b's edited (int-returning) overlay content, got none — the stale, pre-edit export data may have been reused instead")
	}
}
