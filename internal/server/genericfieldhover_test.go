package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/rpc"
)

// TestHandleHover_GenericWrapperFieldThroughExportProduction drives a real
// Server (setWorkspace, check.Engine, depCacheHolder — the identical
// production wiring internal/server.ensureDepProvider installs) over a
// fixture shaped exactly like the field that silently broke under the
// pre-fix corruption: connectrpc.com/connect's Request[T].Msg. testdata's
// box.Box[T]{Msg *T} is instantiated with payload.Data and referenced from a
// consumer package; box and payload are non-root, transitive dependencies
// of consumer (loaded via "./consumer/..." — the only pattern, so consumer
// is the sole Root package), so they resolve exactly the way a real GOROOT
// or module-cache dependency does: through depCacheHolder's
// typecheck.Importer with depexport.Cache as its ExportSource — see
// ensureDepProvider's own doc for why that path used to corrupt this exact
// shape. textDocument/hover on the Msg field access must return real
// content, not nil.
func TestHandleHover_GenericWrapperFieldThroughExportProduction(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "genericclosure"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./consumer/...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	rpcServer := rpc.NewServer(rpc.WithLogger(newTestLogger(t)))
	s := New(rpcServer, Options{Logger: newTestLogger(t)})
	s.setWorkspace(root, snap)
	stopWorkspaceEngineOnCleanup(t, s)

	consumerFile := filepath.Join(root, "consumer", "consumer.go")
	data, err := os.ReadFile(consumerFile)
	if err != nil {
		t.Fatalf("read consumer.go: %v", err)
	}
	pos := identPositionIn(t, consumerFile, data, "Msg", 1) // r.Msg's use in ExtractField (box.Box's own Msg field decl lives in box.go, a different file)

	result, err := s.handleHover(context.Background(), mustMarshal(t, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(consumerFile)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleHover(r.Msg): %v", err)
	}
	hov, ok := result.(*protocol.Hover)
	if !ok || hov == nil {
		t.Fatalf("handleHover(r.Msg) = %#v, want a non-nil *protocol.Hover (the field resolving to types.Invalid under the pre-fix corruption made this nil)", result)
	}
	t.Logf("hover contents: %+v", hov.Contents)
}
