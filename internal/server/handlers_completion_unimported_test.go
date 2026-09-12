package server

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/sivchari/golance/internal/langfeat"
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

// TestUnimportedMemberItems_ImportErrorIsLogged pins the M9 fix at
// handlers_completion_unimported.go's own candidate-resolution loop: an
// ImportFrom failure used to be indistinguishable from "this candidate's
// exported members just don't match the typed prefix" — both silently
// `continue`d, so a genuinely broken candidate (its export data
// undecodable, not merely lacking the wanted member) never left any trace
// once the loop ran out of candidates. It registers a package name whose
// only candidate import path resolves to nothing real, so ImportFrom fails
// for that reason rather than a missing member, and checks the failure
// reaches s.logger.
func TestUnimportedMemberItems_ImportErrorIsLogged(t *testing.T) {
	s, _, root := newTestServer(t)
	var logBuf bytes.Buffer
	s.logger = log.New(&logBuf, "", 0)

	ws := s.workspace()
	if ws == nil {
		t.Fatal("s.workspace() = nil, want a populated workspace")
	}
	const bogusSelector = "doesnotexistpkg"
	ws.pkgNameIndex[bogusSelector] = []string{"example.com/servermod/doesnotexistpkg"}

	path := filepath.Join(root, "greet", "greet.go")
	cf := s.checkedFile(context.Background(), uri.File(path), protocol.Position{})
	if !cf.ok {
		t.Fatalf("checkedFile(%s) ok=false, want a resolvable checked package", path)
	}

	uctx := langfeat.UnimportedContext{Selector: bogusSelector, Prefix: ""}
	items := s.unimportedMemberItems(ws, cf, uctx)
	if items != nil {
		t.Errorf("unimportedMemberItems = %+v, want nil (the only candidate import path does not exist)", items)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, bogusSelector) {
		t.Errorf("log output = %q, want it to mention selector %q", logged, bogusSelector)
	}
	if !strings.Contains(logged, "example.com/servermod/doesnotexistpkg") {
		t.Errorf("log output = %q, want it to name the failing candidate import path", logged)
	}
}
