package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
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

	start := time.Now()
	res, err := s.handleInitialize(context.Background(), params)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("handleInitialize: %v", err)
	}
	if _, ok := res.(*protocol.InitializeResult); !ok {
		t.Fatalf("handleInitialize result type = %T, want *protocol.InitializeResult", res)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("handleInitialize took %v while graph load was blocked; want it to return before the load completes", elapsed)
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

	deadline := time.Now().Add(5 * time.Second)
	for s.workspace() == nil {
		if time.Now().After(deadline) {
			t.Fatal("workspace never became ready after graph load was unblocked")
		}
		time.Sleep(10 * time.Millisecond)
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
