package golance_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eNavLocs records the exact 0-based positions the TestE2E_Nav subtests
// query, captured while the synthetic module is written so nothing is
// re-parsed at query time. This module is independent of writeE2EModule's
// (e2e_repo_test.go), so nav-specific fixtures never collide with it.
type e2eNavLocs struct {
	boxFile string // lib/box/box.go
	boxSrc  string

	appFile         string // app/app.go
	appSrc          string
	boxTypeRefInApp protocol.Position // "Box" in "B box.Box" (cross-package type reference)
	boxPkgNameInApp protocol.Position // "box" in "B box.Box" (the package name)
	totalDecl       protocol.Position // "total" in "total := 0"
	binaryOperandA  protocol.Position // "a" in "total += a"
	widgetSelector  protocol.Position // just after "w.B." in Describe, for completion/resolve
}

func writeE2ENavModule(t *testing.T) (string, e2eNavLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eNavLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2enav\n\ngo 1.23\n")

	const boxSrc = `package box

// Box holds a size.
type Box struct {
	// Size is the box's size.
	Size int
}
`
	locs.boxFile = writeE2EFile(t, root, "lib/box/box.go", boxSrc)
	locs.boxSrc = boxSrc

	const appSrc = `package app

import (
	"fmt"

	"example.com/e2enav/lib/box"
)

// Widget wraps a Box.
type Widget struct {
	B box.Box
}

// Describe returns the wrapped Box's size.
func Describe(w Widget) int {
	return w.B.Size
}

// Total adds a and b, then formats the result.
func Total(a, b int) string {
	total := 0
	total += a
	total += b
	return fmt.Sprintf("%d", total)
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)
	locs.appSrc = appSrc
	locs.boxTypeRefInApp = mustPos(t, appSrc, "B box.Box", "Box")
	locs.boxPkgNameInApp = mustPos(t, appSrc, "B box.Box", "box")
	locs.totalDecl = mustPos(t, appSrc, "total := 0", "total")
	locs.binaryOperandA = mustPos(t, appSrc, "total += a", "a")
	locs.widgetSelector = mustPos(t, appSrc, "return w.B.Size", "Size")

	return root, locs
}

// TestE2E_Nav drives a real golance binary over stdio, covering the
// navigation/editing-support methods implemented in
// internal/server/handlers_nav.go: typeDefinition, declaration,
// documentHighlight, prepareRename, foldingRange, selectionRange,
// rangeFormatting, documentLink, and completionItem/resolve.
func TestE2E_Nav(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2ENavModule(t)
	c := startClient(t, root)
	result := c.initialize(t, root)

	t.Run("capabilities", func(t *testing.T) {
		checkE2ENavCapabilities(t, result)
	})

	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	t.Run("type_definition_cross_package", func(t *testing.T) {
		checkE2ETypeDefinitionCrossPackage(t, c, &locs)
	})

	t.Run("declaration_same_as_definition", func(t *testing.T) {
		checkE2EDeclarationSameAsDefinition(t, c, &locs)
	})

	t.Run("document_highlight_read_write", func(t *testing.T) {
		checkE2EDocumentHighlightReadWrite(t, c, &locs)
	})

	t.Run("prepare_rename_rejects_package_name", func(t *testing.T) {
		checkE2EPrepareRenameRejectsPackageName(t, c, &locs)
	})

	t.Run("prepare_rename_accepts_local_var", func(t *testing.T) {
		checkE2EPrepareRenameAcceptsLocalVar(t, c, &locs)
	})

	t.Run("folding_range", func(t *testing.T) {
		checkE2EFoldingRange(t, c, &locs)
	})

	t.Run("selection_range_hierarchy", func(t *testing.T) {
		checkE2ESelectionRangeHierarchy(t, c, &locs)
	})

	t.Run("document_link_local_and_external", func(t *testing.T) {
		checkE2EDocumentLink(t, c, &locs)
	})

	t.Run("completion_resolve_fills_documentation", func(t *testing.T) {
		checkE2ECompletionResolve(t, c, &locs)
	})

	t.Run("range_formatting", func(t *testing.T) {
		checkE2ERangeFormatting(t, c, &locs)
	})
}

func checkE2ENavCapabilities(t *testing.T, result *protocol.InitializeResult) {
	t.Helper()
	caps := result.Capabilities
	if caps.TypeDefinitionProvider == nil {
		t.Error("TypeDefinitionProvider is nil")
	}
	if caps.DeclarationProvider == nil {
		t.Error("DeclarationProvider is nil")
	}
	if caps.DocumentHighlightProvider == nil {
		t.Error("DocumentHighlightProvider is nil")
	}
	if caps.RenameProvider == nil {
		t.Error("RenameProvider is nil")
	}
	if caps.FoldingRangeProvider == nil {
		t.Error("FoldingRangeProvider is nil")
	}
	if caps.SelectionRangeProvider == nil {
		t.Error("SelectionRangeProvider is nil")
	}
	if caps.DocumentRangeFormattingProvider == nil {
		t.Error("DocumentRangeFormattingProvider is nil")
	}
	if caps.DocumentLinkProvider == nil {
		t.Error("DocumentLinkProvider is nil")
	}
	if caps.CompletionProvider == nil {
		t.Fatal("CompletionProvider is nil")
	}
}

// checkE2ETypeDefinitionCrossPackage is the first cross-reference-dependent
// subtest to run, so it polls (via waitForNonEmptyLocations) rather than
// issuing a single request: waitForIndexReady only signals the indexer
// subprocess exited, not that the server has finished opening the resulting
// facts database and rebuilding its Resolver (see waitForNonEmptyLocations's
// own doc).
func checkE2ETypeDefinitionCrossPackage(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentTypeDefinition, &protocol.TypeDefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.boxTypeRefInApp,
		},
	}, e2eIndexBudget)
	if len(result) != 1 {
		t.Fatalf("typeDefinition returned %d locations, want 1: %+v", len(result), result)
	}
	boxDecl := mustPos(t, locs.boxSrc, "type Box struct", "Box")
	if gotPath := result[0].URI.FsPath(); gotPath != locs.boxFile || result[0].Range.Start.Line != boxDecl.Line {
		t.Errorf("typeDefinition = %s:%d, want %s:%d", gotPath, result[0].Range.Start.Line, locs.boxFile, boxDecl.Line)
	}
}

func checkE2EDeclarationSameAsDefinition(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentDeclaration, &protocol.DeclarationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.boxTypeRefInApp,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("declaration failed: %s", resp.Error)
	}
	var result protocol.LocationSlice
	if err := protocol.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal declaration result: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("declaration returned %d locations, want 1: %+v", len(result), result)
	}
	boxDecl := mustPos(t, locs.boxSrc, "type Box struct", "Box")
	if gotPath := result[0].URI.FsPath(); gotPath != locs.boxFile || result[0].Range.Start.Line != boxDecl.Line {
		t.Errorf("declaration = %s:%d, want %s:%d (same as definition)", gotPath, result[0].Range.Start.Line, locs.boxFile, boxDecl.Line)
	}
}

func checkE2EDocumentHighlightReadWrite(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentDocumentHighlight, &protocol.DocumentHighlightParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.totalDecl,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("documentHighlight failed: %s", resp.Error)
	}
	var result []protocol.DocumentHighlight
	if err := protocol.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal documentHighlight result: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("documentHighlight returned %d occurrences, want 4: %+v", len(result), result)
	}
	var reads, writes int
	for _, h := range result {
		switch h.Kind {
		case protocol.DocumentHighlightKindRead:
			reads++
		case protocol.DocumentHighlightKindWrite:
			writes++
		}
	}
	if reads != 1 || writes != 3 {
		t.Errorf("documentHighlight reads=%d writes=%d, want reads=1 writes=3 (decl + 2 compound assigns are writes, the Sprintf arg is a read): %+v", reads, writes, result)
	}
}

func checkE2EPrepareRenameRejectsPackageName(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentPrepareRename, &protocol.PrepareRenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.boxPkgNameInApp,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("prepareRename failed: %s", resp.Error)
	}
	if string(resp.Result) != "" && string(resp.Result) != "null" {
		t.Errorf("prepareRename on a package name = %s, want null", resp.Result)
	}
}

func checkE2EPrepareRenameAcceptsLocalVar(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentPrepareRename, &protocol.PrepareRenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.totalDecl,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("prepareRename failed: %s", resp.Error)
	}
	if string(resp.Result) == "" || string(resp.Result) == "null" {
		t.Errorf("prepareRename on a local var = %s, want a range", resp.Result)
	}
}

func checkE2EFoldingRange(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentFoldingRange, &protocol.FoldingRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("foldingRange failed: %s", resp.Error)
	}
	var result []protocol.FoldingRange
	if err := protocol.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal foldingRange result: %v", err)
	}
	var hasImports, hasRegion bool
	for _, fr := range result {
		switch fr.Kind {
		case protocol.FoldingRangeKindImports:
			hasImports = true
		case "":
			hasRegion = true
		}
	}
	if !hasImports {
		t.Errorf("foldingRange = %+v, want an import-block range", result)
	}
	if !hasRegion {
		t.Errorf("foldingRange = %+v, want at least one block-body range", result)
	}
}

func checkE2ESelectionRangeHierarchy(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentSelectionRange, &protocol.SelectionRangeParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
		Positions:    []protocol.Position{locs.binaryOperandA},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("selectionRange failed: %s", resp.Error)
	}
	var result []protocol.SelectionRange
	if err := protocol.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal selectionRange result: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("selectionRange returned %d entries, want 1 (one per requested position): %+v", len(result), result)
	}
	depth := 0
	for n := &result[0]; n != nil; n = n.Parent {
		depth++
	}
	if depth < 2 {
		t.Errorf("selectionRange chain depth = %d, want at least 2 (the identifier plus an enclosing node)", depth)
	}
}

func checkE2EDocumentLink(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentDocumentLink, &protocol.DocumentLinkParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("documentLink failed: %s", resp.Error)
	}
	var result []protocol.DocumentLink
	if err := protocol.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal documentLink result: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("documentLink returned %d links, want 2: %+v", len(result), result)
	}
	var sawLocal, sawExternal bool
	for _, l := range result {
		if l.Target == nil {
			continue
		}
		target := string(*l.Target)
		if target == string(uri.File(locs.boxFile)) {
			sawLocal = true
		}
		if strings.HasPrefix(target, "https://pkg.go.dev/fmt") {
			sawExternal = true
		}
	}
	if !sawLocal {
		t.Errorf("documentLink = %+v, want a local file link to %s", result, locs.boxFile)
	}
	if !sawExternal {
		t.Errorf("documentLink = %+v, want an external pkg.go.dev link for fmt", result)
	}
}

func checkE2ECompletionResolve(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentCompletion, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.widgetSelector,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("completion failed: %s", resp.Error)
	}
	var items protocol.CompletionItemSlice
	if err := protocol.Unmarshal(resp.Result, &items); err != nil {
		t.Fatalf("unmarshal completion result: %v", err)
	}
	var sizeItem *protocol.CompletionItem
	for i := range items {
		if items[i].Label == "Size" {
			sizeItem = &items[i]
			break
		}
	}
	if sizeItem == nil {
		t.Fatalf("completion for w.B. is missing \"Size\"; got %d item(s): %+v", len(items), items)
	}
	if len(sizeItem.Data) == 0 {
		t.Fatal("completion item \"Size\" has no Data, want a resolve key")
	}

	itemJSON, err := protocol.Marshal(sizeItem)
	if err != nil {
		t.Fatalf("marshal completion item: %v", err)
	}
	resolveResp := c.call(t, protocol.MethodCompletionItemResolve, json.RawMessage(itemJSON), e2eRequestBudget)
	if len(resolveResp.Error) > 0 {
		t.Fatalf("completionItem/resolve failed: %s", resolveResp.Error)
	}
	var resolved protocol.CompletionItem
	if err := protocol.Unmarshal(resolveResp.Result, &resolved); err != nil {
		t.Fatalf("unmarshal completionItem/resolve result: %v", err)
	}
	md, ok := resolved.Documentation.(*protocol.MarkupContent)
	var doc string
	if ok {
		doc = md.Value
	} else if s, ok := resolved.Documentation.(protocol.String); ok {
		doc = string(s)
	}
	if !strings.Contains(doc, "Size is the box's size.") {
		t.Errorf("resolved Documentation = %q, want it to contain Size's doc comment", doc)
	}
}

// checkE2ERangeFormatting sends a misformatted didChange without a following
// didSave, then polls rangeFormatting (mirroring
// checkE2EHoverReflectsUnsavedEdit's pattern) since there is no
// acknowledgment that the notification has been applied.
func checkE2ERangeFormatting(t *testing.T, c *lspClient, locs *e2eNavLocs) {
	t.Helper()
	messy := strings.Replace(locs.appSrc, "total += a\n\ttotal += b", "total  +=  a\n\ttotal  +=  b", 1)
	c.changeFile(t, locs.appFile, 2, messy)

	rng := protocol.Range{
		Start: protocol.Position{Line: locs.binaryOperandA.Line, Character: 0},
		End:   protocol.Position{Line: locs.binaryOperandA.Line + 1, Character: 0},
	}
	deadline := time.Now().Add(e2eRequestBudget)
	for {
		resp := c.call(t, protocol.MethodTextDocumentRangeFormatting, &protocol.DocumentRangeFormattingParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Range:        rng,
		}, e2eRequestBudget)
		if len(resp.Error) > 0 {
			t.Fatalf("rangeFormatting failed: %s", resp.Error)
		}
		var edits []protocol.TextEdit
		if err := protocol.Unmarshal(resp.Result, &edits); err != nil {
			t.Fatalf("unmarshal rangeFormatting result: %v", err)
		}
		if len(edits) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("rangeFormatting returned no edits for a misformatted range within the budget")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
