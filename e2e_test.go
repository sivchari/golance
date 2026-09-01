package golance_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Budgets for the E2E requests. e2eIndexBudget covers the indexer subprocess
// building the facts index for the synthetic workspace; e2eRequestBudget
// covers one ordinary LSP round trip.
const (
	e2eIndexBudget   = 60 * time.Second
	e2eRequestBudget = 10 * time.Second
)

// skipUnlessE2E skips the suite in -short mode and unless GOLANCE_E2E=1 is
// set, since it builds and runs a real golance binary.
func skipUnlessE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short set; skipping e2e")
	}
	if os.Getenv("GOLANCE_E2E") != "1" {
		t.Skip("GOLANCE_E2E=1 not set; skipping e2e")
	}
}

// TestE2E drives a real golance binary over stdio against a synthetic
// workspace. Subtests share one session and run sequentially; each
// reproduces one user-visible flow, implemented as its own top-level
// checkE2E* function to keep this driver itself simple.
func TestE2E(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EModule(t)
	c := startClient(t, root)
	result := c.initialize(t, root)

	t.Run("initialize_capabilities", func(t *testing.T) {
		checkE2EInitializeCapabilities(t, result)
	})

	t.Run("diagnostics_on_open", func(t *testing.T) {
		checkE2EDiagnosticsOnOpen(t, c, &locs)
	})

	// diagnostics_on_open_clean_file covers a file that never had any
	// diagnostics at all: before golance publishes diagnostics for every
	// open file in a checked package (not just files with something to
	// report), a client had no way to learn such a file is clean.
	t.Run("diagnostics_on_open_clean_file", func(t *testing.T) {
		checkE2EDiagnosticsOnOpenCleanFile(t, c, &locs)
	})

	// Cross-reference queries need the facts index; opening a file starts
	// the indexer subprocess (see internal/server.handleInitialize), so wait
	// for its completion signal before relying on definition/references.
	// The synthetic module also includes an "empty" package with zero
	// GoFiles (see writeE2EModule): the indexer must still exit 0 and
	// build a usable index despite it, or everything below would fail.
	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	t.Run("definition_cross_package", func(t *testing.T) {
		checkE2EDefinitionCrossPackage(t, c, &locs)
	})

	t.Run("references_cross_file", func(t *testing.T) {
		checkE2EReferencesCrossFile(t, c, &locs)
	})

	t.Run("references_cross_package_method", func(t *testing.T) {
		checkE2EReferencesCrossPackageMethod(t, c, &locs)
	})

	t.Run("completion_selector", func(t *testing.T) {
		checkE2ECompletionSelector(t, c, &locs)
	})

	// hover_reflects_unsaved_edit sends a didChange without a following
	// didSave and asserts hover picks up the overlay content. There is no
	// acknowledgment that the notification has been applied (LSP has none
	// either), so this polls a bounded number of times instead of relying on
	// a single racy request right after the notification.
	t.Run("hover_reflects_unsaved_edit", func(t *testing.T) {
		checkE2EHoverReflectsUnsavedEdit(t, c, &locs)
	})

	// concurrent_edits_no_error_responses is a regression test for a bug
	// where a request-driven type check (hover/completion) shared per-dir
	// supersede cancellation with debounce-triggered background rechecks:
	// while a burst of edits kept re-firing the debounce for lib/util, an
	// in-flight hover or completion request's type check could be canceled
	// by it, surfacing an error response for a request that was still
	// alive. It sends a burst of textDocument/didChange while issuing
	// hover and completion requests concurrently and asserts none of them
	// ever come back as an error response.
	t.Run("concurrent_edits_no_error_responses", func(t *testing.T) {
		checkE2EConcurrentEditsNoErrorResponses(t, c, &locs)
	})

	// unsaved_new_file_joins_package covers Phase 1 of the ad-hoc package
	// design (design-adhoc-packages.md): a brand-new .go file created in an
	// editor, never written to disk, still gets full language features in
	// its directory's known package — hover resolves a symbol declared in
	// another file of that package, and diagnostics for the new file's own
	// type error are published.
	t.Run("unsaved_new_file_joins_package", func(t *testing.T) {
		checkE2EUnsavedNewFileJoinsPackage(t, c, &locs)
	})

	// hover_and_inlay_hint_in_package_test_file covers the same directory
	// fallback for the other symptom it fixes: an on-disk in-package
	// "_test.go" file, which packages.Load's non-Tests GoFiles list omits
	// entirely — hover resolves a symbol declared in another file of the
	// package, and inlay hints (which share the same engine.Get + FileText
	// path) are returned too.
	t.Run("hover_and_inlay_hint_in_package_test_file", func(t *testing.T) {
		checkE2EHoverAndInlayHintInPackageTestFile(t, c, &locs)
	})

	// references_includes_in_package_test_file_call_site and
	// definition_from_inside_test_file cover the facts-index gap fixed
	// alongside hover/inlay hints above: an in-package "_test.go" file
	// contributed nothing to the facts index at all (see
	// internal/index.testFilesInPackage), so references on a workspace
	// symbol silently omitted its own test file's call sites, and
	// definition/references invoked from a position inside a test file
	// found nothing.
	t.Run("references_includes_in_package_test_file_call_site", func(t *testing.T) {
		checkE2EReferencesIncludesTestFileCallSite(t, c, &locs)
	})

	t.Run("definition_from_inside_test_file", func(t *testing.T) {
		checkE2EDefinitionFromInsideTestFile(t, c, &locs)
	})

	// definition_from_inside_test_file_cross_package and
	// references_from_inside_test_file_cross_package close the specific gap
	// PR #36's same-package test file coverage above did not exercise: a
	// query issued from a _test.go position, targeting a symbol in a
	// *different* workspace package (see
	// checkE2EDefinitionFromInsideTestFileCrossPackage's doc for why a
	// same-package target could pass even before internal/xref.Resolver
	// grew its own directory fallback).
	t.Run("definition_from_inside_test_file_cross_package", func(t *testing.T) {
		checkE2EDefinitionFromInsideTestFileCrossPackage(t, c, &locs)
	})

	t.Run("references_from_inside_test_file_cross_package", func(t *testing.T) {
		checkE2EReferencesFromInsideTestFileCrossPackage(t, c, &locs)
	})

	// didsave_test_file_adds_new_test_only_symbol verifies the write path:
	// saving an edited in-package test file reindexes it (internal/server's
	// handleDidSave -> reindex -> index.Reindex), and a symbol newly added
	// only in that test file becomes findable afterward.
	t.Run("didsave_test_file_adds_new_test_only_symbol", func(t *testing.T) {
		checkE2EDidSaveTestFileAddsNewSymbol(t, c, &locs)
	})
}

func checkE2EUnsavedNewFileJoinsPackage(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()

	const newSrc = `package extra

// NewFunc calls Double, defined in extra.go: cross-file resolution for an
// unsaved new file that never touched disk.
func NewFunc() int {
	return Double(3)
}

// Broken has a type error, so diagnostics for the new file itself can be
// asserted too.
func Broken() int {
	return "not an int"
}
`
	newFile := filepath.Join(filepath.Dir(locs.extraFile), "new_unsaved.go")
	doubleCallPos := mustPos(t, newSrc, "Double(3)", "Double")

	c.openNewFile(t, newFile, newSrc)

	diags := c.waitForDiagnostics(t, newFile)
	if len(diags) == 0 {
		t.Fatal("want at least one diagnostic for the unsaved new file's own type error, got none")
	}

	resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(newFile)},
			Position:     doubleCallPos,
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
	if !strings.Contains(md.Value, "Double") {
		t.Errorf("hover on Double (defined in extra.go, another file of the same package) = %q, want it to mention Double", md.Value)
	}
}

func checkE2EHoverAndInlayHintInPackageTestFile(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	c.openFile(t, locs.utilTestFile)

	resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilTestFile)},
			Position:     locs.sumCallInUtilTest,
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
		t.Errorf("hover on Sum (defined in util.go, another file of the same package) inside the in-package test file = %q, want it to mention Sum", md.Value)
	}

	hints := requestInlayHints(t, c, locs.utilTestFile, protocol.Range{End: endOfDocument(locs.utilTestSrc)})
	if len(hints) == 0 {
		t.Fatal("want at least one inlay hint inside the in-package test file, got none")
	}
}

// checkE2EReferencesIncludesTestFileCallSite verifies that references on
// Sum's declaration (in util.go) includes util_test.go's own call site
// (sumTotal's Sum(4, 5)), alongside the cross-package call sites
// checkE2EReferencesCrossFile already covers.
func checkE2EReferencesIncludesTestFileCallSite(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilFile)},
			Position:     locs.sumDecl,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}, e2eRequestBudget)
	var found bool
	for _, l := range got {
		if l.URI.FsPath() == locs.utilTestFile && l.Range.Start.Line == locs.sumCallInUtilTest.Line {
			found = true
		}
	}
	if !found {
		t.Errorf("references on Sum missing its call site in the in-package test file %s; got %d location(s): %+v", locs.utilTestFile, len(got), got)
	}
}

// checkE2EDefinitionFromInsideTestFile verifies that a definition query
// issued from a position inside an in-package test file (the Sum(4, 5) call
// in util_test.go) resolves to Sum's declaration in util.go.
func checkE2EDefinitionFromInsideTestFile(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilTestFile)},
			Position:     locs.sumCallInUtilTest,
		},
	}, e2eRequestBudget)
	if len(got) != 1 {
		t.Fatalf("definition from inside the test file returned %d locations, want 1: %+v", len(got), got)
	}
	if gotPath := got[0].URI.FsPath(); gotPath != locs.utilFile || got[0].Range.Start.Line != locs.sumDecl.Line {
		t.Fatalf("definition = %s:%d, want %s:%d", gotPath, got[0].Range.Start.Line, locs.utilFile, locs.sumDecl.Line)
	}
}

// checkE2EDefinitionFromInsideTestFileCrossPackage verifies that a
// definition query issued from a position inside an in-package test file,
// targeting a symbol in a *different* workspace package (store.Store's use
// in util_test.go), resolves to Store's exact declaration in lib/store.
// checkE2EDefinitionFromInsideTestFile's Sum target cannot exercise this: a
// same-package miss falls back to langfeat.SamePackageDefinition (see
// definitionFallback), which masks whether the facts index itself ever
// resolved the query position at all — only a target in another workspace
// package forces the answer through internal/xref.Resolver's own directory
// fallback (see resolveAt's doc), since dependencyDefinition's root-package
// guard refuses to answer this case from definitionFallback either.
func checkE2EDefinitionFromInsideTestFileCrossPackage(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilTestFile)},
			Position:     locs.storeRefInUtilTest,
		},
	}, e2eRequestBudget)
	if len(got) != 1 {
		t.Fatalf("definition from inside the test file (cross-package target) returned %d locations, want 1: %+v", len(got), got)
	}
	if gotPath := got[0].URI.FsPath(); gotPath != locs.storeFile || got[0].Range.Start != locs.storeDecl {
		t.Fatalf("definition = %s:%+v, want %s:%+v", gotPath, got[0].Range.Start, locs.storeFile, locs.storeDecl)
	}
}

// checkE2EReferencesFromInsideTestFileCrossPackage verifies that a
// references query issued from that same cross-package position (Store's
// use inside util_test.go) returns Store's declaration: unlike Definition,
// References has no same-package/dependency fallback chain at all (see
// handleReferences), so this exercises the facts-index path directly.
func checkE2EReferencesFromInsideTestFileCrossPackage(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilTestFile)},
			Position:     locs.storeRefInUtilTest,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: true},
	}, e2eRequestBudget)
	var found bool
	for _, l := range got {
		if l.URI.FsPath() == locs.storeFile && l.Range.Start == locs.storeDecl {
			found = true
		}
	}
	if !found {
		t.Errorf("references from inside the test file (cross-package target) missing Store's declaration in %s; got %d location(s): %+v", locs.storeFile, len(got), got)
	}
}

// checkE2EDidSaveTestFileAddsNewSymbol edits util_test.go to add a new
// exported, test-only function (Helper, declared nowhere else in the
// package) and saves it, then polls workspace/symbol until Helper is
// findable — verifying handleDidSave's reindex path picks up a test file's
// own edit end to end, not just Build's initial pass.
func checkE2EDidSaveTestFileAddsNewSymbol(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	updated := locs.utilTestSrc + "\n// Helper is declared only in this in-package test file.\nfunc Helper() int {\n\treturn 42\n}\n"
	c.saveFile(t, locs.utilTestFile, updated)

	deadline := time.Now().Add(e2eIndexBudget)
	for {
		resp := c.call(t, protocol.MethodWorkspaceSymbol, &protocol.WorkspaceSymbolParams{Query: "Helper"}, e2eRequestBudget)
		if len(resp.Error) > 0 {
			t.Fatalf("workspace/symbol failed: %s", resp.Error)
		}
		var infos protocol.SymbolInformationSlice
		if err := protocol.Unmarshal(resp.Result, &infos); err != nil {
			t.Fatalf("unmarshal workspace/symbol result: %v", err)
		}
		for _, info := range infos {
			if info.Name == "Helper" && info.Location.URI.FsPath() == locs.utilTestFile {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace/symbol never found Helper (added to %s and saved) within %s; got %+v", locs.utilTestFile, e2eIndexBudget, infos)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func checkE2EInitializeCapabilities(t *testing.T, result *protocol.InitializeResult) {
	t.Helper()
	if result.ServerInfo.Name != "golance" {
		t.Errorf("ServerInfo.Name = %q, want golance", result.ServerInfo.Name)
	}
	caps := result.Capabilities
	if caps.CompletionProvider == nil {
		t.Error("CompletionProvider is nil")
	}
	if caps.HoverProvider == nil {
		t.Error("HoverProvider is nil")
	}
	if caps.DefinitionProvider == nil {
		t.Error("DefinitionProvider is nil")
	}
	if caps.ReferencesProvider == nil {
		t.Error("ReferencesProvider is nil")
	}
	if caps.ImplementationProvider == nil {
		t.Error("ImplementationProvider is nil")
	}
}

func checkE2EDiagnosticsOnOpen(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	c.openFile(t, locs.brokenFile)
	diags := c.waitForDiagnostics(t, locs.brokenFile)
	if len(diags) == 0 {
		t.Fatal("want at least one diagnostic for broken.go, got none")
	}
	if diags[0].Severity != protocol.DiagnosticSeverityError {
		t.Errorf("diagnostic severity = %v, want DiagnosticSeverityError", diags[0].Severity)
	}
}

func checkE2EDiagnosticsOnOpenCleanFile(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	c.openFile(t, locs.extraFile)
	diags := c.waitForDiagnostics(t, locs.extraFile)
	if len(diags) != 0 {
		t.Fatalf("want zero diagnostics for a clean file, got %d: %+v", len(diags), diags)
	}
}

func checkE2EDefinitionCrossPackage(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.sumCallInApp,
		},
	}, e2eRequestBudget)
	if len(got) != 1 {
		t.Fatalf("want exactly 1 definition location, got %d: %+v", len(got), got)
	}
	if gotPath := got[0].URI.FsPath(); gotPath != locs.utilFile || got[0].Range.Start.Line != locs.sumDecl.Line {
		t.Fatalf("definition = %s:%d, want %s:%d", gotPath, got[0].Range.Start.Line, locs.utilFile, locs.sumDecl.Line)
	}
}

func checkE2EReferencesCrossFile(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilFile)},
			Position:     locs.sumDecl,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}, e2eRequestBudget)
	files := make(map[string]bool, len(got))
	for _, l := range got {
		files[l.URI.FsPath()] = true
	}
	for _, want := range []string{locs.appFile, locs.extraFile} {
		if !files[want] {
			t.Errorf("references missing a location in %s; got %d location(s): %+v", want, len(got), got)
		}
	}
}

// checkE2EReferencesCrossPackageMethod verifies that textDocument/references
// on a method's declaration (lib/store.Store.Get) finds its cross-package
// call sites in both usepkg and app — coverage checkE2EReferencesCrossFile
// does not give, since that subtest targets a plain function. Store's
// method set is declared in an order (Get, Zulu, Alpha, Mike) that differs
// from alphabetical order, the same ordering mismatch
// TestBuild_CrossPackageMethodRefIdentity guards against at the index layer.
func checkE2EReferencesCrossPackageMethod(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	got := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentReferences, &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.storeFile)},
			Position:     locs.storeGetDecl,
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	}, e2eRequestBudget)
	files := make(map[string]bool, len(got))
	for _, l := range got {
		files[l.URI.FsPath()] = true
	}
	for _, want := range []string{locs.usepkgFile, locs.appFile} {
		if !files[want] {
			t.Errorf("references missing a location in %s; got %d location(s): %+v", want, len(got), got)
		}
	}
}

func checkE2ECompletionSelector(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	c.openFile(t, locs.usepkgFile)
	resp := c.call(t, protocol.MethodTextDocumentCompletion, &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.usepkgFile)},
			Position:     locs.selectorPos,
		},
	}, e2eRequestBudget)
	if len(resp.Error) > 0 {
		t.Fatalf("completion failed: %s", resp.Error)
	}
	var items protocol.CompletionItemSlice
	if err := protocol.Unmarshal(resp.Result, &items); err != nil {
		t.Fatalf("unmarshal completion result: %v", err)
	}
	found := false
	for i := range items {
		if items[i].Label == "Get" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("completion for the store.Store selector is missing \"Get\"; got %d item(s): %+v", len(items), items)
	}
}

func checkE2EHoverReflectsUnsavedEdit(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()
	c.openFile(t, locs.utilFile)
	newText := strings.Replace(locs.utilSrc, "adds two ints", "adds two ints, edited", 1)
	c.changeFile(t, locs.utilFile, 2, newText)

	deadline := time.Now().Add(e2eRequestBudget)
	var lastDoc string
	for {
		resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilFile)},
				Position:     locs.sumDecl,
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
		lastDoc = md.Value
		if strings.Contains(lastDoc, "edited") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hover did not reflect the unsaved edit within %s: %s", e2eRequestBudget, lastDoc)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkE2EConcurrentEditsNoErrorResponses sends a burst of
// textDocument/didChange for locs.utilFile while concurrently issuing
// hover requests against it and completion requests against
// locs.usepkgFile (already open from the completion_selector subtest), and
// asserts none of those requests ever comes back as an error response.
// Before request-driven checks stopped sharing per-dir cancellation with
// debounce-triggered background rechecks, a request's in-flight type check
// could be canceled by the next debounce firing for the same directory.
func checkE2EConcurrentEditsNoErrorResponses(t *testing.T, c *lspClient, locs *e2eLocs) {
	t.Helper()

	stop := make(chan struct{})
	var editWG sync.WaitGroup
	editWG.Add(1)
	go func() {
		defer editWG.Done()
		version := int32(100)
		for {
			select {
			case <-stop:
				return
			default:
			}
			version++
			newText := fmt.Sprintf("%s\n// edit %d\n", locs.utilSrc, version)
			c.changeFile(t, locs.utilFile, version, newText)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var mu sync.Mutex
	var errs []string
	record := func(label string, resp *message) {
		if resp != nil && len(resp.Error) > 0 {
			mu.Lock()
			errs = append(errs, fmt.Sprintf("%s: %s", label, resp.Error))
			mu.Unlock()
		}
	}

	const rounds = 30
	var reqWG sync.WaitGroup
	for range rounds {
		reqWG.Add(2)
		go func() {
			defer reqWG.Done()
			resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.utilFile)},
					Position:     locs.sumDecl,
				},
			}, e2eRequestBudget)
			record("hover", resp)
		}()
		go func() {
			defer reqWG.Done()
			resp := c.call(t, protocol.MethodTextDocumentCompletion, &protocol.CompletionParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.usepkgFile)},
					Position:     locs.selectorPos,
				},
			}, e2eRequestBudget)
			record("completion", resp)
		}()
		time.Sleep(3 * time.Millisecond)
	}
	reqWG.Wait()

	close(stop)
	editWG.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Fatalf("got %d error response(s) during concurrent edits, want 0:\n%s", len(errs), strings.Join(errs, "\n"))
	}
}
