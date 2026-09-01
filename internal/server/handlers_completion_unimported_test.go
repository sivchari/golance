package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/overlay"
)

// completionPosition returns the LSP Position right after prefixThroughCursor
// (the file's original content up to and including the cursor) ends in
// text, the same "type this much, then ask for completion" shape the
// unimported-completion fixture (testdata/module/unimported/unimported.go)
// is written for.
func completionPosition(t *testing.T, text []byte, prefixThroughCursor string) protocol.Position {
	t.Helper()
	i := bytes.Index(text, []byte(prefixThroughCursor))
	if i < 0 {
		t.Fatalf("substring %q not found in fixture", prefixThroughCursor)
	}
	offset := i + len(prefixThroughCursor)
	pos, ok := overlay.UTF16PositionForByteOffset(text, offset)
	if !ok {
		t.Fatalf("UTF16PositionForByteOffset(%d) failed", offset)
	}
	return pos
}

func completionItemsAt(t *testing.T, s *Server, path string, text []byte, prefixThroughCursor string) protocol.CompletionItemSlice {
	t.Helper()
	pos := completionPosition(t, text, prefixThroughCursor)
	result, err := s.handleCompletion(context.Background(), mustMarshal(t, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleCompletion: %v", err)
	}
	items, ok := result.(protocol.CompletionItemSlice)
	if !ok {
		t.Fatalf("handleCompletion result = %#v, want protocol.CompletionItemSlice", result)
	}
	return items
}

// TestHandleCompletion_UnimportedPackagePrefix covers shape 1: typing a
// package name itself ("gre" for the workspace package "greet", not yet
// imported by testdata/module/unimported/unimported.go) surfaces an
// unimported candidate whose AdditionalTextEdits import it.
func TestHandleCompletion_UnimportedPackagePrefix(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "unimported", "unimported.go")
	text, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	items := completionItemsAt(t, s, path, text, "var _ = gre")

	idx := -1
	for i := range items {
		if items[i].Label == "greet" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("completion results missing \"greet\"; got %d item(s): %+v", len(items), items)
	}
	got := items[idx]
	if got.Kind != protocol.CompletionItemKindModule {
		t.Errorf("greet item Kind = %v, want CompletionItemKindModule", got.Kind)
	}
	if len(got.AdditionalTextEdits) != 1 {
		t.Fatalf("greet item AdditionalTextEdits = %+v, want exactly one edit", got.AdditionalTextEdits)
	}
	if !strings.Contains(got.AdditionalTextEdits[0].NewText, `"example.com/servermod/greet"`) {
		t.Errorf("AdditionalTextEdits[0].NewText = %q, want it to import example.com/servermod/greet", got.AdditionalTextEdits[0].NewText)
	}
}

// TestHandleCompletion_UnimportedMemberSelector covers shape 2: typing
// "fmt.Sp" where fmt is not imported surfaces fmt's exported Sp-prefixed
// members (Sprintf, ...), each carrying the edit that imports "fmt".
func TestHandleCompletion_UnimportedMemberSelector(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "unimported", "unimported.go")
	text, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	items := completionItemsAt(t, s, path, text, "fmt.Sp")

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
	got := items[idx]
	if len(got.AdditionalTextEdits) != 1 {
		t.Fatalf("Sprintf item AdditionalTextEdits = %+v, want exactly one edit", got.AdditionalTextEdits)
	}
	if !strings.Contains(got.AdditionalTextEdits[0].NewText, `"fmt"`) {
		t.Errorf("AdditionalTextEdits[0].NewText = %q, want it to import fmt", got.AdditionalTextEdits[0].NewText)
	}
	for i := range items {
		if items[i].Label == "Println" {
			t.Errorf("completion results contain \"Println\", want it filtered out (does not match prefix Sp)")
		}
	}
}

// TestHandleCompletion_NoUnimportedContextIsNoOp checks that a cursor
// position ordinary (in-scope) completion already fully handles — a
// selector on an already-imported package — is untouched by
// appendUnimportedCompletions: no duplicate "greet" package item, no
// AdditionalTextEdits attached to anything, and no error.
func TestHandleCompletion_NoUnimportedContextIsNoOp(t *testing.T) {
	s, _, root := newTestServer(t)
	path := filepath.Join(root, "depuse", "depuse.go")
	text, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// depuse.go already imports "strings"; completing "strings.Bui" is an
	// entirely ordinary, already-imported selector completion.
	items := completionItemsAt(t, s, path, text, "strings.Bui")

	found := false
	for i := range items {
		if items[i].Label == "Builder" {
			found = true
			if len(items[i].AdditionalTextEdits) != 0 {
				t.Errorf("Builder item AdditionalTextEdits = %+v, want none (already imported)", items[i].AdditionalTextEdits)
			}
		}
	}
	if !found {
		t.Fatalf("completion results missing \"Builder\"; got %d item(s): %+v", len(items), items)
	}
}
