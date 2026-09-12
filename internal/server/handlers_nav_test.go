package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// completionWithDataItemsAt mirrors completionItemsAt (handlers_completion_
// unimported_test.go) but drives handleCompletionWithData, the handler
// actually registered for textDocument/completion, so each returned item
// carries the Data field completionItem/resolve needs.
func completionWithDataItemsAt(t *testing.T, s *Server, path string, text []byte, prefixThroughCursor string) protocol.CompletionItemSlice {
	t.Helper()
	pos := completionPosition(t, text, prefixThroughCursor)
	result, err := s.handleCompletionWithData(context.Background(), mustMarshal(t, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleCompletionWithData: %v", err)
	}
	items, ok := result.(protocol.CompletionItemSlice)
	if !ok {
		t.Fatalf("handleCompletionWithData result = %#v, want protocol.CompletionItemSlice", result)
	}
	return items
}

// TestHandleCompletionResolve_UnimportedSelectorDoc is a regression test for
// Finding L3's completiondoc.go half: completionItem/resolve for an
// unimported-package-member candidate (here "fmt.Sprintf", with fmt not yet
// imported by testdata/module/unimported/unimported.go — see
// TestHandleCompletion_UnimportedMemberSelector) used to resolve no
// Documentation at all, since ResolveCompletionDoc's ordinary object
// resolution can never find fmt (this package has no graph access to know
// it as an import path). unimportedCompletionDoc now redoes that lookup at
// the server layer, where the graph is available.
func TestHandleCompletionResolve_UnimportedSelectorDoc(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "unimported", "unimported.go")
	text, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	items := completionWithDataItemsAt(t, s, path, text, "fmt.Sp")
	idx := -1
	for i := range items {
		if items[i].Label == "Sprintf" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("completion results missing \"Sprintf\"; got %d item(s): %+v", len(items), items)
	}
	if len(items[idx].Data) == 0 {
		t.Fatal("Sprintf item has no Data; completionItem/resolve cannot look up its doc")
	}

	resolved, err := s.handleCompletionResolve(context.Background(), mustMarshal(t, items[idx]))
	if err != nil {
		t.Fatalf("handleCompletionResolve: %v", err)
	}
	got, ok := resolved.(*protocol.CompletionItem)
	if !ok {
		t.Fatalf("handleCompletionResolve result = %#v, want *protocol.CompletionItem", resolved)
	}
	if got.Documentation == nil {
		t.Fatal("resolved Sprintf item has no Documentation, want fmt.Sprintf's doc comment")
	}
	doc, ok := got.Documentation.(protocol.String)
	if !ok || !strings.Contains(string(doc), "Sprintf formats") {
		t.Errorf("Documentation = %+v, want it to contain fmt.Sprintf's doc comment (\"Sprintf formats...\")", got.Documentation)
	}
}
