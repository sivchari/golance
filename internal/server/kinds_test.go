package server

// This file pins a gopls-parity gap found while auditing golance's
// textDocument/documentSymbol against a real gopls v0.23.0 (see
// ./audit-informational.md at the repo root): unlike workspaceSymbolKind,
// which distinguishes index.KindInterface from index.KindType (see the
// facts-index-backed workspace/symbol case below), documentSymbolKind has
// no interface bucket at all — every langfeat.SymbolType (a struct, an
// interface, or a plain defined type like `type Level int`) maps to
// protocol.SymbolKindStruct. gopls's own `symbols` CLI reports "Struct",
// "Interface", and "Class" respectively for those same three shapes.

import (
	"testing"

	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/langfeat"
)

func TestDocumentSymbolKind_NoInterfaceDistinction(t *testing.T) {
	got := documentSymbolKind(langfeat.SymbolType)
	if got != protocol.SymbolKindStruct {
		t.Fatalf("documentSymbolKind(SymbolType) = %v, want SymbolKindStruct (this test's own doc explains why an interface declaration gets the same kind)", got)
	}
}

// TestWorkspaceSymbolKind_DoesDistinguishInterface is
// TestDocumentSymbolKind_NoInterfaceDistinction's contrast case: the
// facts-index-backed workspace/symbol path already carries an Interface
// kind (see internal/index's KindInterface), and workspaceSymbolKind maps
// it to protocol.SymbolKindInterface correctly — so the very same "Reader"
// interface declaration would render with different icons depending on
// whether the client asked textDocument/documentSymbol (SymbolKindStruct)
// or workspace/symbol (SymbolKindInterface).
func TestWorkspaceSymbolKind_DoesDistinguishInterface(t *testing.T) {
	got := workspaceSymbolKind(index.KindInterface)
	if got != protocol.SymbolKindInterface {
		t.Fatalf("workspaceSymbolKind(KindInterface) = %v, want SymbolKindInterface", got)
	}
}
