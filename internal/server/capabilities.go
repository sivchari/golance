package server

import "go.lsp.dev/protocol"

// capabilities returns the ServerCapabilities golance advertises in its
// InitializeResult.
func (s *Server) capabilities() protocol.ServerCapabilities {
	openClose := true
	change := protocol.TextDocumentSyncKindIncremental
	return protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: &openClose,
			Change:    &change,
			Save:      protocol.Boolean(true),
		},
		CompletionProvider:         &protocol.CompletionOptions{TriggerCharacters: []string{"."}},
		HoverProvider:              protocol.Boolean(true),
		SignatureHelpProvider:      &protocol.SignatureHelpOptions{TriggerCharacters: []string{"(", ","}},
		DefinitionProvider:         protocol.Boolean(true),
		ReferencesProvider:         protocol.Boolean(true),
		ImplementationProvider:     protocol.Boolean(true),
		DocumentSymbolProvider:     protocol.Boolean(true),
		WorkspaceSymbolProvider:    protocol.Boolean(true),
		RenameProvider:             protocol.Boolean(true),
		DocumentFormattingProvider: protocol.Boolean(true),
		InlayHintProvider:          protocol.Boolean(true),
	}
}
