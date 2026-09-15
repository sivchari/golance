package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/rpc"
	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/xref"
)

// TestHandleDefinitionAndHover_GenericFieldWithRootTypeArgument is the
// regression test for a generic wrapper field (mirroring
// connectrpc.com/connect's Request[T].Msg) instantiated with a ROOT
// (workspace) type argument: testdata/rootgenericfield/wrapper.Box[T] is a
// NON-root dependency of the ROOT consumer package, instantiated with
// payload.Data — also a ROOT package, unlike testdata/genericclosure's
// TestHandleHover_GenericWrapperFieldThroughExportProduction, where both the
// wrapper AND its type argument are non-root. That fixture alone left
// engineImporter's root-import tiers (decodeRoot/getRoot — see
// rootimporter.go) entirely unexercised for a generic instantiation's own
// type argument, the exact combination a real monorepo's
// connect.Request[T]-shaped RPC handlers hit (T is always a generated
// protobuf message type, always a ROOT package) once engineImporter began
// routing root imports through the facts index's persisted export data
// instead of check.Engine's own decode-only importer.
func TestHandleDefinitionAndHover_GenericFieldWithRootTypeArgument(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "rootgenericfield"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	// wrapper is deliberately NOT listed as a Load pattern: it is reached
	// only transitively (as consumer's own dependency), the same technique
	// rootexport_test.go's newRootExportTestServer uses to keep b/c Root
	// while wrapper here stays non-Root.
	snap, err := graph.Load(graph.Options{Dir: root}, "./consumer/...", "./payload/...")
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
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)
	s.idx.Store(&indexState{db: db, cas: cas, resolver: xref.New(db, cas, snap, relative)})

	ws := s.workspace()
	consumerFile := filepath.Join(root, "consumer", "consumer.go")

	cp, err := ws.engine.Get(context.Background(), consumerFile)
	if err != nil {
		t.Fatalf("Get(consumer): %v", err)
	}
	for _, d := range check.Diagnostics(cp, s.overlay) {
		t.Errorf("unexpected diagnostic checking consumer: %v", d)
	}
	// Unlike the root-to-root regression tests (rootimporter_test.go,
	// rootexport_test.go), depExportProviderVal.Checked() is not asserted
	// here: wrapper is a genuine non-root dependency, resolved through
	// depCache's ordinary (non-root) path, which may legitimately produce
	// wrapper's own export data on first sight — only payload's ROOT import
	// must avoid a from-source recheck, which depCache.cache.Decodes()
	// below confirms.
	if got := ws.depCache.cache.Decodes(); got == 0 {
		t.Error("depCache.cache.Decodes() = 0, want > 0 (payload's export data must have been decoded via the facts index fast path)")
	}

	data, err := os.ReadFile(consumerFile)
	if err != nil {
		t.Fatalf("read consumer.go: %v", err)
	}
	pos := identPositionIn(t, consumerFile, data, "Msg", 1) // r.Msg's use in ExtractField

	defResult, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(consumerFile)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(r.Msg): %v", err)
	}
	locs, ok := defResult.(protocol.LocationSlice)
	if !ok || len(locs) == 0 {
		t.Fatalf("handleDefinition(r.Msg) = %#v, want a non-empty LocationSlice pointing into wrapper.go", defResult)
	}
	if got := locs[0].URI.FsPath(); filepath.Base(got) != "wrapper.go" {
		t.Errorf("handleDefinition(r.Msg) resolved to %s, want wrapper/wrapper.go", got)
	}

	hovResult, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(consumerFile)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleHover(r.Msg): %v", err)
	}
	if hov, ok := hovResult.(*protocol.Hover); !ok || hov == nil {
		t.Fatalf("handleHover(r.Msg) = %#v, want a non-nil *protocol.Hover", hovResult)
	}
}
