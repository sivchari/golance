package server

import (
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/overlay"
)

// writeTempFile writes content to dir/name and returns its absolute path.
func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// newTestOverlay returns an empty *overlay.Overlay for tests that only
// need a Server's overlay field populated.
func newTestOverlay() *overlay.Overlay {
	return overlay.New()
}

// openDoc simulates a textDocument/didOpen notification for path with the
// given content.
func openDoc(t *testing.T, s *Server, path, text string) {
	t.Helper()
	s.overlay.DidOpen(&protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:     uri.File(path),
			Version: 1,
			Text:    text,
		},
	})
}

// changeDoc simulates a textDocument/didChange notification for path that
// replaces the whole document with text.
func changeDoc(t *testing.T, s *Server, path string, version int32, text string) {
	t.Helper()
	err := s.overlay.DidChange(&protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Version:                version,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			&protocol.TextDocumentContentChangeWholeDocument{Text: text},
		},
	})
	if err != nil {
		t.Fatalf("changeDoc: %v", err)
	}
}
