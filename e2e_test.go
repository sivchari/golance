package golance_test

import (
	"os"
	"strings"
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
	c.waitForIndexReady(t, e2eIndexBudget)

	t.Run("definition_cross_package", func(t *testing.T) {
		checkE2EDefinitionCrossPackage(t, c, &locs)
	})

	t.Run("references_cross_file", func(t *testing.T) {
		checkE2EReferencesCrossFile(t, c, &locs)
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
	diags := c.waitForDiagnostics(t, locs.brokenFile, e2eRequestBudget)
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
	diags := c.waitForDiagnostics(t, locs.extraFile, e2eRequestBudget)
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
