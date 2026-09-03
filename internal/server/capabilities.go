package server

import (
	"go.lsp.dev/protocol"

	"github.com/sivchari/golance/internal/langfeat"
)

// capabilities returns the ServerCapabilities golance advertises in its
// InitializeResult.
func (s *Server) capabilities() protocol.ServerCapabilities {
	openClose := true
	change := protocol.TextDocumentSyncKindIncremental
	resolveProvider := true
	prepareRenameProvider := true
	return protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: &openClose,
			Change:    &change,
			Save:      protocol.Boolean(true),
		},
		CompletionProvider:              &protocol.CompletionOptions{TriggerCharacters: []string{"."}, ResolveProvider: &resolveProvider},
		HoverProvider:                   protocol.Boolean(true),
		SignatureHelpProvider:           &protocol.SignatureHelpOptions{TriggerCharacters: []string{"(", ","}},
		DefinitionProvider:              protocol.Boolean(true),
		TypeDefinitionProvider:          protocol.Boolean(true),
		DeclarationProvider:             protocol.Boolean(true),
		ReferencesProvider:              protocol.Boolean(true),
		ImplementationProvider:          protocol.Boolean(true),
		DocumentSymbolProvider:          protocol.Boolean(true),
		DocumentHighlightProvider:       protocol.Boolean(true),
		WorkspaceSymbolProvider:         protocol.Boolean(true),
		RenameProvider:                  &protocol.RenameOptions{PrepareProvider: &prepareRenameProvider},
		DocumentFormattingProvider:      protocol.Boolean(true),
		DocumentRangeFormattingProvider: protocol.Boolean(true),
		DocumentLinkProvider:            &protocol.DocumentLinkOptions{},
		FoldingRangeProvider:            protocol.Boolean(true),
		SelectionRangeProvider:          protocol.Boolean(true),
		InlayHintProvider:               protocol.Boolean(true),
		CodeActionProvider: &protocol.CodeActionOptions{
			CodeActionKinds: []protocol.CodeActionKind{
				protocol.CodeActionKindQuickFix,
				protocol.CodeActionKindSourceOrganizeImports,
			},
		},
		SemanticTokensProvider: &protocol.SemanticTokensOptions{
			Legend: protocol.SemanticTokensLegend{
				TokenTypes:     langfeat.TokenKindNames,
				TokenModifiers: langfeat.TokenModifierNames,
			},
			Full:  protocol.Boolean(true),
			Range: protocol.Boolean(true),
		},
		CallHierarchyProvider: protocol.Boolean(true),
		TypeHierarchyProvider: protocol.Boolean(true),

		// CodeLensProvider must be non-nil to enable the capability at all
		// (matching gopls's own general.go comment to the same effect).
		// ResolveProvider is deliberately left unset: gopls's own
		// CodeLensOptions is `&protocol.CodeLensOptions{}` too — every lens
		// gopls emits already carries its full Command, so codeLens/resolve
		// is never needed, and golance's handleCodeLens matches that (see
		// handlers_codelens.go).
		CodeLensProvider: &protocol.CodeLensOptions{},
		ExecuteCommandProvider: protocol.ExecuteCommandOptions{
			Commands: []string{commandGenerate, commandRegenerateCgo, commandRunTests},
		},
	}
}
