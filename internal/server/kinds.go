package server

import (
	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/langfeat"
)

// completionItemKind maps a langfeat.CompletionKind to its LSP equivalent.
func completionItemKind(k langfeat.CompletionKind) protocol.CompletionItemKind {
	switch k {
	case langfeat.KindFunc:
		return protocol.CompletionItemKindFunction
	case langfeat.KindMethod:
		return protocol.CompletionItemKindMethod
	case langfeat.KindField:
		return protocol.CompletionItemKindField
	case langfeat.KindVar:
		return protocol.CompletionItemKindVariable
	case langfeat.KindConst:
		return protocol.CompletionItemKindConstant
	case langfeat.KindType:
		return protocol.CompletionItemKindStruct
	case langfeat.KindPackage:
		return protocol.CompletionItemKindModule
	default:
		return protocol.CompletionItemKindText
	}
}

// documentSymbolKind maps a langfeat.SymbolKind to its LSP equivalent.
func documentSymbolKind(k langfeat.SymbolKind) protocol.SymbolKind {
	switch k {
	case langfeat.SymbolFunc:
		return protocol.SymbolKindFunction
	case langfeat.SymbolMethod:
		return protocol.SymbolKindMethod
	case langfeat.SymbolType:
		return protocol.SymbolKindStruct
	case langfeat.SymbolVar:
		return protocol.SymbolKindVariable
	case langfeat.SymbolConst:
		return protocol.SymbolKindConstant
	default:
		return protocol.SymbolKindVariable
	}
}

// workspaceSymbolKind maps one of the index.Kind* constants recorded in the
// facts blob (see xref.SymbolInfo.Kind) to its LSP equivalent.
func workspaceSymbolKind(k uint8) protocol.SymbolKind {
	switch k {
	case index.KindFunc:
		return protocol.SymbolKindFunction
	case index.KindMethod:
		return protocol.SymbolKindMethod
	case index.KindType:
		return protocol.SymbolKindStruct
	case index.KindInterface:
		return protocol.SymbolKindInterface
	case index.KindVar:
		return protocol.SymbolKindVariable
	case index.KindConst:
		return protocol.SymbolKindConstant
	case index.KindField:
		return protocol.SymbolKindField
	default:
		return protocol.SymbolKindVariable
	}
}
