package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
)

// TestHandleInitialize_ReturnsBeforeGraphLoadCompletes verifies the
// structural fix for golance's cold-worktree-open timeout: handleInitialize
// must return InitializeResult immediately, without waiting for the import
// graph load loadWorkspaceAsync runs in the background (see its own doc).
// graphLoad is substituted with a fake that blocks until the test releases
// it, standing in for a slow `go list` against a large monorepo without
// needing an actual synthetic module of that size.
//
// A facts index matching testdata/module is pre-built at the exact
// (HOME-sandboxed) location tryWarmOpen/revalidateIndex resolve for this
// root, so that once graphLoad is unblocked and produces the same
// snapshot, revalidateIndex finds nothing changed and never falls through
// to buildIndex — which would otherwise launch this test binary itself as
// a subprocess (os.Executable() resolves to it under `go test`), the same
// hazard TestRevalidateIndex_UnchangedKeepsWarmOpenHandle guards against.
func TestHandleInitialize_ReturnsBeforeGraphLoadCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		root, err := filepath.Abs(filepath.Join("testdata", "module"))
		if err != nil {
			t.Fatalf("abs testdata root: %v", err)
		}

		snap, err := graph.Load(graph.Options{Dir: root}, "./...")
		if err != nil {
			t.Fatalf("graph.Load: %v", err)
		}
		dbPath := indexDBFile(root)
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
			t.Fatalf("mkdir index dir: %v", err)
		}
		cas, err := store.OpenCAS(casDir(root))
		if err != nil {
			t.Fatalf("store.OpenCAS: %v", err)
		}
		buildTestIndexDB(t, snap, dbPath, cas)

		unblock := make(chan struct{})
		started := make(chan struct{})
		var once sync.Once
		orig := graphLoad
		t.Cleanup(func() { graphLoad = orig })
		graphLoad = func(opts graph.Options, patterns ...string) (*graph.Snapshot, error) {
			once.Do(func() { close(started) })
			<-unblock
			return orig(opts, patterns...)
		}

		s := New(rpc.NewServer(rpc.WithLogger(newTestLogger(t))), Options{Logger: newTestLogger(t)})
		t.Cleanup(func() {
			if idx := s.idx.Load(); idx != nil {
				_ = idx.db.Close()
			}
		})

		params, err := protocol.Marshal(&protocol.InitializeParams{
			WorkspaceFoldersInitializeParams: protocol.WorkspaceFoldersInitializeParams{
				WorkspaceFolders: protocol.NewNullable([]protocol.WorkspaceFolder{{URI: uri.File(root), Name: "module"}}),
			},
		})
		if err != nil {
			t.Fatalf("marshal InitializeParams: %v", err)
		}

		res, err := s.handleInitialize(context.Background(), params)
		if err != nil {
			t.Fatalf("handleInitialize: %v", err)
		}
		if _, ok := res.(*protocol.InitializeResult); !ok {
			t.Fatalf("handleInitialize result type = %T, want *protocol.InitializeResult", res)
		}
		// The real proof handleInitialize returned before the graph load
		// finished, rather than a wall-clock bound (meaningless under the fake
		// clock): the workspace must still be unset right after it returns.
		if ws := s.workspace(); ws != nil {
			t.Fatal("workspace already populated by the time handleInitialize returned; want it to return before the load completes")
		}

		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("loadWorkspaceAsync never called graphLoad")
		}
		if ws := s.workspace(); ws != nil {
			t.Fatal("workspace already populated while graph load is still blocked")
		}

		close(unblock)

		synctest.Wait()
		if s.workspace() == nil {
			t.Fatal("workspace never became ready after graph load was unblocked")
		}
	})
}

// TestLoadWorkspaceAsync_ColdBuildRepairsPackageDroppedByBuild is a
// regression test for the cold-build path (tryWarmOpen finds no database,
// so buildIndex runs): loadWorkspaceAsync must run the same revalidateIndex
// pass afterward that the warm-open branch already ran, since per
// internal/index.Build's own contract a package's parse/type-check failure
// never changes the indexer's exit code — that package simply never gets a
// UnitPointer written, silently, with the rest of the build reporting
// success (see loadWorkspaceAsync's own doc and openIndexAfterBuild's
// stderr/errors=N surfacing).
//
// This does not drive buildIndex (and therefore loadWorkspaceAsync) itself:
// that launches a real indexer subprocess via os.Executable(), which under
// `go test` resolves to this very test binary — at best a hang, at worst a
// recursive re-run of this whole suite (the same hazard
// TestRevalidateIndex_LargeStaleSetFallsBackToFullRebuild documents).
// Instead it reproduces exactly what buildIndex/openIndexAfterBuild leave
// behind after a cold build that silently dropped one package — a database
// with a build fingerprint (index.Build's own PutBuildFingerprint) and
// every other root package present, but no UnitPointer at all for the
// dropped one — installs it via s.idx.Store exactly as openIndexAfterBuild
// does, and then calls s.revalidateIndex with the same ctx loadWorkspaceAsync
// now always calls it with, regardless of which branch installed idx.
func TestLoadWorkspaceAsync_ColdBuildRepairsPackageDroppedByBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeTempFile(t, dir, "go.mod", "module example.com/coldbuildtest\n\ngo 1.26\n")
	aDir := filepath.Join(dir, "pkga")
	if err := os.MkdirAll(aDir, 0o750); err != nil {
		t.Fatalf("mkdir pkga: %v", err)
	}
	writeTempFile(t, aDir, "pkga.go", "package pkga\n\n// V returns 1.\nfunc V() int { return 1 }\n")

	// Loaded before pkgb exists, so this snapshot never even lists it — the
	// same shape a real per-package build failure leaves behind (see
	// TestRevalidateIndex_TargetedRepairFixesMissingPackageWithoutFullRebuild):
	// buildIndex's own index.Build call simply never writes a UnitPointer
	// for a package it fails to process, indistinguishable at the database
	// level from a package the snapshot it built against never knew about.
	snapWithoutB, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (before pkgb exists): %v", err)
	}

	bDir := filepath.Join(dir, "pkgb")
	if err := os.MkdirAll(bDir, 0o750); err != nil {
		t.Fatalf("mkdir pkgb: %v", err)
	}
	writeTempFile(t, bDir, "pkgb.go", "package pkgb\n\n// W returns 2.\nfunc W() int { return 2 }\n")

	fullSnap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load (with pkgb): %v", err)
	}
	if _, ok := fullSnap.Packages["example.com/coldbuildtest/pkgb"]; !ok {
		t.Fatal("pkgb missing from fullSnap; test setup is wrong")
	}

	dbPath := indexDBFile(dir)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir index dir: %v", err)
	}
	cas, err := store.OpenCAS(casDir(dir))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	buildTestIndexDB(t, snapWithoutB, dbPath, cas)

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(dir, fullSnap)

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	idx := &indexState{db: db, cas: cas, resolver: s.newResolver(db, cas, fullSnap, RelativeIndexPaths(dir))}
	s.idx.Store(idx)
	t.Cleanup(func() { _ = db.Close() })

	// The exact call loadWorkspaceAsync now makes unconditionally after
	// either the warm-open or the cold-build branch installs idx.
	s.revalidateIndex(context.Background(), dir)

	got := s.idx.Load()
	if got != idx {
		t.Errorf("s.idx after revalidateIndex = %p, want the same %p (a targeted repair must not close-and-rebuild)", got, idx)
	}
	if _, err := got.db.GetUnit(context.Background(), store.Hash("example.com/coldbuildtest/pkgb")); err != nil {
		t.Fatalf("GetUnit(pkgb) after revalidateIndex: %v (want the build-dropped package repaired in place)", err)
	}
}

// TestHandleInitialized_SetsClientInitialized covers the ordering guarantee
// workspaceReadyRefreshes relies on: s.clientInitialized only becomes true
// once handleInitialized runs, i.e. once the client's own "initialized"
// notification has arrived — never before, since the LSP spec forbids any
// server-initiated request earlier than that.
func TestHandleInitialized_SetsClientInitialized(t *testing.T) {
	s := New(rpc.NewServer(), Options{Logger: newTestLogger(t)})
	if s.clientInitialized.Load() {
		t.Fatalf("clientInitialized = true before handleInitialized ran, want false")
	}
	if err := s.handleInitialized(context.Background(), nil); err != nil {
		t.Fatalf("handleInitialized: %v", err)
	}
	if !s.clientInitialized.Load() {
		t.Fatalf("clientInitialized = false after handleInitialized ran, want true")
	}
}

// TestClientSupportsInlayHintRefresh covers clientSupportsInlayHintRefresh's
// three "no" cases (missing workspace capabilities, missing inlayHint
// capabilities, and refreshSupport explicitly false) plus the "yes" case —
// the gate refreshInlayHints relies on to avoid sending a client a request
// it never declared support for.
func TestClientSupportsInlayHintRefresh(t *testing.T) {
	trueVal, falseVal := true, false

	tests := []struct {
		name string
		p    *protocol.InitializeParams
		want bool
	}{
		{"no workspace capabilities", &protocol.InitializeParams{}, false},
		{
			"workspace capabilities without inlayHint",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{}}},
			false,
		},
		{
			"refreshSupport explicitly false",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				InlayHint: &protocol.InlayHintWorkspaceClientCapabilities{RefreshSupport: &falseVal},
			}}},
			false,
		},
		{
			"refreshSupport true",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				InlayHint: &protocol.InlayHintWorkspaceClientCapabilities{RefreshSupport: &trueVal},
			}}},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientSupportsInlayHintRefresh(tt.p); got != tt.want {
				t.Errorf("clientSupportsInlayHintRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClientSupportsSemanticTokensRefresh mirrors
// TestClientSupportsInlayHintRefresh for
// clientSupportsSemanticTokensRefresh — the gate refreshSemanticTokens
// relies on to avoid sending a client a request it never declared support
// for.
func TestClientSupportsSemanticTokensRefresh(t *testing.T) {
	trueVal, falseVal := true, false

	tests := []struct {
		name string
		p    *protocol.InitializeParams
		want bool
	}{
		{"no workspace capabilities", &protocol.InitializeParams{}, false},
		{
			"workspace capabilities without semanticTokens",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{}}},
			false,
		},
		{
			"refreshSupport explicitly false",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				SemanticTokens: &protocol.SemanticTokensWorkspaceClientCapabilities{RefreshSupport: &falseVal},
			}}},
			false,
		},
		{
			"refreshSupport true",
			&protocol.InitializeParams{Capabilities: protocol.ClientCapabilities{Workspace: &protocol.WorkspaceClientCapabilities{
				SemanticTokens: &protocol.SemanticTokensWorkspaceClientCapabilities{RefreshSupport: &trueVal},
			}}},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientSupportsSemanticTokensRefresh(tt.p); got != tt.want {
				t.Errorf("clientSupportsSemanticTokensRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}
