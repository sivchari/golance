package golance_test

import (
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eExternalTestLocs records the fixture path, source, and positions
// TestE2EExternalTestPackage's subtests query, mirroring e2eAdhocLocs's role
// for TestE2EAdhocPackage.
type e2eExternalTestLocs struct {
	extFile    string            // lib/util/util_external_test.go
	extSrc     string            // its source, for the full-document inlay hint request
	sumCallPos protocol.Position // "Sum" in "util.Sum(2, 3)"
	helperPos  protocol.Position // "sumExternal" in "return sumExternal()"
	declPos    protocol.Position // "sumExternal" in "func sumExternal() int {"
}

// TestE2EExternalTestPackage drives a real golance binary over stdio and
// verifies Phase 2 of design-adhoc-packages.md: a "package util_test" file
// (the external "_test"-suffixed test package for lib/util, which already
// has util.go and, from writeE2EModule, an in-package util_test.go) gets
// hover, completion, diagnostics, inlay hints, and same-package definition
// resolved against its own checked unit — including util's exported
// declarations, through the ordinary dependency importer, exactly like any
// other cross-package import. Cross-reference queries (references)
// deliberately keep degrading to an empty result, per
// design-adhoc-packages.md's scope guard: an external test package is
// never indexed, the same as an ad-hoc package (see e2e_adhoc_test.go).
//
// This lives in its own file, mirroring e2e_adhoc_test.go's rationale,
// rather than adding to e2e_test.go / e2e_repo_test.go.
func TestE2EExternalTestPackage(t *testing.T) {
	skipUnlessE2E(t)

	root, _ := writeE2EModule(t)
	locs := writeE2EExternalTestFixture(t, root)

	c := startClient(t, root)
	c.initialize(t, root)
	c.openFile(t, locs.extFile)
	c.waitForIndexReady(t)

	t.Run("hover_base_package_exported_symbol", func(t *testing.T) {
		checkE2EExternalTestHoverBaseExported(t, c, &locs)
	})
	t.Run("completion_base_package_exported_members", func(t *testing.T) {
		checkE2EExternalTestCompletionBaseExported(t, c, &locs)
	})
	t.Run("diagnostics_on_open", func(t *testing.T) {
		checkE2EExternalTestDiagnostics(t, c, &locs)
	})
	t.Run("inlay_hints_non_empty", func(t *testing.T) {
		checkE2EExternalTestInlayHintsNonEmpty(t, c, &locs)
	})
	t.Run("same_package_definition", func(t *testing.T) {
		checkE2EExternalTestSamePackageDefinition(t, c, &locs)
	})
	t.Run("references_degrades_to_empty_not_error", func(t *testing.T) {
		checkE2EExternalTestReferencesDegradeToEmpty(t, c, &locs)
	})
}

// writeE2EExternalTestFixture writes lib/util/util_external_test.go — the
// external "package util_test" test package for lib/util (writeE2EModule
// already gives lib/util a base util.go with Sum, plus an in-package
// util_test.go) — and returns the positions TestE2EExternalTestPackage's
// subtests query.
func writeE2EExternalTestFixture(t *testing.T, root string) e2eExternalTestLocs {
	t.Helper()

	const extSrc = `package util_test

import "example.com/e2e/lib/util"

// sumExternal calls Sum, lib/util's exported declaration, from outside the
// package: the external test package's own checked unit resolving a base
// package import exactly like any other cross-package import.
func sumExternal() int {
	total := util.Sum(2, 3)
	return total
}

// localHelper calls sumExternal, declared above in this same external test
// file, exercising same-package definition within the external unit.
func localHelper() int {
	return sumExternal()
}

// brokenDiag has a type error: a string literal cannot be returned as int.
func brokenDiag() int {
	return "not an int"
}
`
	var locs e2eExternalTestLocs
	locs.extFile = writeE2EFile(t, root, "lib/util/util_external_test.go", extSrc)
	locs.extSrc = extSrc
	locs.sumCallPos = mustPos(t, extSrc, "util.Sum(2, 3)", "Sum")
	locs.helperPos = mustPos(t, extSrc, "return sumExternal()", "sumExternal")
	locs.declPos = mustPos(t, extSrc, "func sumExternal() int {", "sumExternal")
	return locs
}

func checkE2EExternalTestHoverBaseExported(t *testing.T, c *lspClient, locs *e2eExternalTestLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.extFile)},
			Position:     locs.sumCallPos,
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
	if !strings.Contains(md.Value, "Sum") {
		t.Errorf("hover on util.Sum (the base package's exported declaration) = %q, want it to mention Sum", md.Value)
	}
}

func checkE2EExternalTestCompletionBaseExported(t *testing.T, c *lspClient, locs *e2eExternalTestLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentCompletion, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.extFile)},
			Position:     locs.sumCallPos,
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
		if items[i].Label == "Sum" {
			return
		}
	}
	t.Fatalf("completion for the util selector is missing %q; got %d item(s): %+v", "Sum", len(items), items)
}

func checkE2EExternalTestDiagnostics(t *testing.T, c *lspClient, locs *e2eExternalTestLocs) {
	t.Helper()
	diags := c.waitForDiagnostics(t, locs.extFile)
	if len(diags) == 0 {
		t.Fatal("want at least one diagnostic for brokenDiag's type error, got none")
	}
}

func checkE2EExternalTestInlayHintsNonEmpty(t *testing.T, c *lspClient, locs *e2eExternalTestLocs) {
	t.Helper()
	hints := requestInlayHints(t, c, locs.extFile, protocol.Range{End: endOfDocument(locs.extSrc)})
	if len(hints) == 0 {
		t.Fatal("want at least one inlay hint inside the external test package, got none")
	}
}

func checkE2EExternalTestSamePackageDefinition(t *testing.T, c *lspClient, locs *e2eExternalTestLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.extFile)},
			Position:     locs.helperPos,
		},
	}, e2eRequestBudget)
	if len(got) != 1 {
		t.Fatalf("definition from inside the external test package returned %d locations, want 1: %+v", len(got), got)
	}
	if gotPath := got[0].URI.FsPath(); gotPath != locs.extFile || got[0].Range.Start != locs.declPos {
		t.Fatalf("definition = %s:%+v, want %s:%+v", gotPath, got[0].Range.Start, locs.extFile, locs.declPos)
	}
}

func checkE2EExternalTestReferencesDegradeToEmpty(t *testing.T, c *lspClient, locs *e2eExternalTestLocs) {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.extFile)},
			Position:     locs.helperPos,
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
		t.Errorf("references inside an external test package = %+v, want empty (external test packages are never indexed)", got)
	}
}
