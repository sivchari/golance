package golance_test

import (
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eAdhocLocs records the fixture paths and positions
// TestE2EAdhocPackage's subtests query, mirroring e2eLocs's role for
// TestE2E.
type e2eAdhocLocs struct {
	fixtureFile      string            // testdata/fixture/fixture.go
	helperFile       string            // testdata/fixture/fixture_helper.go, same ad-hoc package
	fixtureHelperSrc string            // helperFile's source, for the full-document inlay hint request
	declPos          protocol.Position // "AdhocOnlySymbol" in "func AdhocOnlySymbol(x, y int) int"
	callPos          protocol.Position // "AdhocOnlySymbol" in the AdhocOnlySymbol(1, 2) call inside helperFile
	fieldAccessPos   protocol.Position // "Name" in "w.Name" inside helperFile, for selector completion
}

// TestE2EAdhocPackage drives a real golance binary over stdio and verifies
// Phase 3 of design-adhoc-packages.md: a file under testdata/, a directory
// the go tool always excludes from ./... patterns and so no known package
// ever covers, still gets hover, completion, inlay hints, and same-package
// definition — synthesized as an "ad-hoc" package from its own package
// clause and same-clause siblings, rather than the silent no-op it got
// before Phase 3. Cross-reference queries (references) and the facts
// index (workspace/symbol) deliberately keep degrading to an empty result
// for it, per design-adhoc-packages.md's 非目標: ad-hoc packages are never
// indexed.
//
// This lives in its own file, mirroring e2e_inlayhint_test.go's rationale,
// rather than adding to e2e_test.go / e2e_repo_test.go.
func TestE2EAdhocPackage(t *testing.T) {
	skipUnlessE2E(t)

	root, _ := writeE2EModule(t)
	locs := writeE2EAdhocFixture(t, root)

	c := startClient(t, root)
	c.initialize(t, root)
	c.openFile(t, locs.helperFile)
	c.waitForIndexReady(t)

	t.Run("hover_local_symbol", func(t *testing.T) {
		checkE2EAdhocHoverLocalSymbol(t, c, &locs)
	})
	t.Run("completion_local_field", func(t *testing.T) {
		checkE2EAdhocCompletionLocalField(t, c, &locs)
	})
	t.Run("inlay_hints_non_empty", func(t *testing.T) {
		checkE2EAdhocInlayHintsNonEmpty(t, c, &locs)
	})
	t.Run("same_package_definition_exact_position", func(t *testing.T) {
		checkE2EAdhocSamePackageDefinition(t, c, &locs)
	})
	t.Run("references_degrades_to_empty_not_error", func(t *testing.T) {
		checkE2EAdhocReferencesDegradeToEmpty(t, c, &locs)
	})
	t.Run("facts_index_excludes_adhoc_symbol", func(t *testing.T) {
		checkE2EAdhocFactsIndexExcludesSymbol(t, c)
	})
}

// writeE2EAdhocFixture writes fixture.go and fixture_helper.go into
// testdata/fixture under root, a directory the go tool always excludes
// from "./..." patterns, and returns the paths and positions the
// TestE2EAdhocPackage subtests query.
func writeE2EAdhocFixture(t *testing.T, root string) e2eAdhocLocs {
	t.Helper()

	// AdhocOnlySymbol only exists inside this fixture, so a workspace/symbol
	// hit for it (checked by checkE2EAdhocFactsIndexExcludesSymbol) can only
	// mean the facts index wrongly picked up an ad-hoc directory.
	const fixtureSrc = `package fixture

// AdhocOnlySymbol is declared only inside this testdata fixture, a
// directory no known package covers (the go tool skips any directory
// named "testdata" when resolving ./... patterns).
func AdhocOnlySymbol(x, y int) int {
	return x + y
}

// Widget is a small local type, for selector completion coverage.
type Widget struct {
	Name string
}
`
	const fixtureHelperSrc = `package fixture

// UseAdhocOnlySymbol calls AdhocOnlySymbol, defined in fixture.go:
// cross-file resolution inside an ad-hoc package (no known package covers
// this directory).
func UseAdhocOnlySymbol() int {
	total := AdhocOnlySymbol(1, 2)
	return total
}

// FieldAccess exercises selector completion on Widget's field.
func FieldAccess(w Widget) string {
	return w.Name
}
`
	var locs e2eAdhocLocs
	locs.fixtureFile = writeE2EFile(t, root, "testdata/fixture/fixture.go", fixtureSrc)
	locs.helperFile = writeE2EFile(t, root, "testdata/fixture/fixture_helper.go", fixtureHelperSrc)
	locs.fixtureHelperSrc = fixtureHelperSrc
	locs.declPos = mustPos(t, fixtureSrc, "func AdhocOnlySymbol", "AdhocOnlySymbol")
	locs.callPos = mustPos(t, fixtureHelperSrc, "AdhocOnlySymbol(1, 2)", "AdhocOnlySymbol")
	locs.fieldAccessPos = mustPos(t, fixtureHelperSrc, "w.Name", "Name")
	return locs
}

func checkE2EAdhocHoverLocalSymbol(t *testing.T, c *lspClient, locs *e2eAdhocLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.helperFile)},
			Position:     locs.callPos,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("hover failed: %s", resp.Error)
	}
	var hover protocol.Hover
	if err := protocol.Unmarshal(resp.Result, &hover); err != nil {
		t.Fatalf("unmarshal hover result: %v", err)
	}
	md, ok := hover.Contents.(*protocol.MarkupContent)
	if !ok {
		t.Fatalf("hover contents type = %T, want *protocol.MarkupContent", hover.Contents)
	}
	if !strings.Contains(md.Value, "AdhocOnlySymbol") {
		t.Errorf("hover on AdhocOnlySymbol (defined in fixture.go, another file of the same ad-hoc package) = %q, want it to mention AdhocOnlySymbol", md.Value)
	}
}

func checkE2EAdhocCompletionLocalField(t *testing.T, c *lspClient, locs *e2eAdhocLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentCompletion, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.helperFile)},
			Position:     locs.fieldAccessPos,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("completion failed: %s", resp.Error)
	}
	var items protocol.CompletionItemSlice
	if err := protocol.Unmarshal(resp.Result, &items); err != nil {
		t.Fatalf("unmarshal completion result: %v", err)
	}
	for i := range items {
		if items[i].Label == "Name" {
			return
		}
	}
	t.Fatalf("completion for the Widget selector is missing %q; got %d item(s): %+v", "Name", len(items), items)
}

func checkE2EAdhocInlayHintsNonEmpty(t *testing.T, c *lspClient, locs *e2eAdhocLocs) {
	t.Helper()
	hints := requestInlayHints(t, c, locs.helperFile, protocol.Range{End: endOfDocument(locs.fixtureHelperSrc)})
	if len(hints) == 0 {
		t.Fatal("want at least one inlay hint inside the ad-hoc package, got none")
	}
}

func checkE2EAdhocSamePackageDefinition(t *testing.T, c *lspClient, locs *e2eAdhocLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.helperFile)},
			Position:     locs.callPos,
		},
	}, e2eRequestBudget)
	if len(got) != 1 {
		t.Fatalf("definition from inside the ad-hoc package returned %d locations, want 1: %+v", len(got), got)
	}
	if gotPath := got[0].URI.FsPath(); gotPath != locs.fixtureFile || got[0].Range.Start != locs.declPos {
		t.Fatalf("definition = %s:%+v, want %s:%+v", gotPath, got[0].Range.Start, locs.fixtureFile, locs.declPos)
	}
}

func checkE2EAdhocReferencesDegradeToEmpty(t *testing.T, c *lspClient, locs *e2eAdhocLocs) {
	t.Helper()
	// callRetryIndexUnavailable rides out the short, pre-existing race
	// window right after waitForIndexReady (see its own doc): this subtest
	// asserts on the facts index's steady-state answer for an ad-hoc
	// package (always empty, by design), not on whether the index has
	// finished installing yet.
	resp := c.callRetryIndexUnavailable(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.helperFile)},
			Position:     locs.callPos,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("references returned a protocol error, want a graceful empty result: %s", resp.Error)
	}
	var got protocol.LocationSlice
	if err := protocol.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal references result: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("references inside an ad-hoc package = %+v, want empty (ad-hoc packages are never indexed)", got)
	}
}

func checkE2EAdhocFactsIndexExcludesSymbol(t *testing.T, c *lspClient) {
	t.Helper()
	// See checkE2EAdhocReferencesDegradeToEmpty's comment on
	// callRetryIndexUnavailable.
	resp := c.callRetryIndexUnavailable(t, protocol.MethodWorkspaceSymbol, &protocol.WorkspaceSymbolParams{Query: "AdhocOnlySymbol"}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("workspace/symbol failed: %s", resp.Error)
	}
	var infos protocol.SymbolInformationSlice
	if err := protocol.Unmarshal(resp.Result, &infos); err != nil {
		t.Fatalf("unmarshal workspace/symbol result: %v", err)
	}
	for _, info := range infos {
		if info.Name == "AdhocOnlySymbol" {
			t.Fatalf("workspace/symbol found AdhocOnlySymbol at %+v, want it excluded (ad-hoc packages are never indexed)", info.Location)
		}
	}
}
