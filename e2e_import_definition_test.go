package golance_test

import (
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eImportDefLocs records the exact 0-based positions
// TestE2E_DefinitionOnImportPath queries, captured while the synthetic
// module is written so nothing is re-parsed at query time.
type e2eImportDefLocs struct {
	boxFile string // lib/box/box.go
	boxSrc  string

	appFile string // app/app.go

	workspaceImportPos protocol.Position // inside "example.com/e2eimportdef/lib/box"
	stdlibImportPos    protocol.Position // inside "fmt"
	brokenImportPos    protocol.Position // inside "example.com/e2eimportdef/doesnotexist"
}

func writeE2EImportDefModule(t *testing.T) (string, e2eImportDefLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eImportDefLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2eimportdef\n\ngo 1.23\n")

	const boxSrc = `package box

// Box holds a size.
type Box struct {
	Size int
}
`
	locs.boxFile = writeE2EFile(t, root, "lib/box/box.go", boxSrc)
	locs.boxSrc = boxSrc

	const appSrc = `package app

import (
	"fmt"

	"example.com/e2eimportdef/lib/box"
	_ "example.com/e2eimportdef/doesnotexist"
)

// Describe formats b using fmt.
func Describe(b box.Box) string {
	return fmt.Sprintf("%d", b.Size)
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)
	locs.workspaceImportPos = mustPos(t, appSrc, `"example.com/e2eimportdef/lib/box"`, "box")
	locs.stdlibImportPos = mustPos(t, appSrc, `"fmt"`, "fmt")
	locs.brokenImportPos = mustPos(t, appSrc, "doesnotexist", "doesnotexist")

	return root, locs
}

// TestE2E_DefinitionOnImportPath covers "Go to Definition" invoked on an
// import spec's path string itself (as opposed to an identifier
// referencing something from the imported package): facts extraction never
// indexes an *ast.ImportSpec, so this always resolves through
// definitionFallback's importDefinition step (internal/server/
// handlers_xref.go), backed directly by internal/graph's Snapshot rather
// than the facts index or export data. gopls jumps into the imported
// package's own file(s) for this; the third subtest pins that an import
// path the graph could not resolve at all (e.g. a typo, or a genuinely
// missing dependency) degrades to an empty result instead of erroring.
func TestE2E_DefinitionOnImportPath(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EImportDefModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	t.Run("workspace_package", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
				Position:     locs.workspaceImportPos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("definition(import box) returned %d locations, want 1: %+v", len(result), result)
		}
		if gotPath := result[0].URI.FsPath(); gotPath != locs.boxFile {
			t.Errorf("definition(import box) = %s, want %s (box's own package file)", gotPath, locs.boxFile)
		}
		wantDecl := mustPos(t, locs.boxSrc, "package box", "box")
		if result[0].Range.Start.Line != wantDecl.Line {
			t.Errorf("definition(import box) line = %d, want %d (box's package clause)", result[0].Range.Start.Line, wantDecl.Line)
		}
	})

	t.Run("stdlib_package", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
				Position:     locs.stdlibImportPos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("definition(import fmt) returned %d locations, want 1: %+v", len(result), result)
		}
		gotPath := result[0].URI.FsPath()
		if !strings.HasSuffix(filepath.ToSlash(gotPath), "fmt/doc.go") {
			t.Errorf("definition(import fmt) = %s, want a path ending in fmt/doc.go (inside GOROOT)", gotPath)
		}
	})

	t.Run("unresolvable_import_degrades_empty", func(t *testing.T) {
		resp := c.call(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
				Position:     locs.brokenImportPos,
			},
		}, e2eRequestBudget)
		if len(resp.Error) > 0 {
			t.Fatalf("definition(import doesnotexist) failed: %s", resp.Error)
		}
		var result protocol.LocationSlice
		if err := protocol.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("unmarshal definition result: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("definition(import doesnotexist) = %+v, want an empty result (unresolvable import path)", result)
		}
	})
}
