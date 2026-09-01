package golance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eDepDefLocs records the position TestE2E_DefinitionFromDependencyFile
// queries in its synthetic workspace file.
type e2eDepDefLocs struct {
	appFile string
	appPos  protocol.Position // fmt.Sprintf call site in appFile
}

func writeE2EDependencyDefModule(t *testing.T) (string, e2eDepDefLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eDepDefLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2edepdef\n\ngo 1.23\n")

	const appSrc = `package app

import "fmt"

func Describe(n int) string {
	return fmt.Sprintf("%d", n)
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)
	locs.appPos = mustPos(t, appSrc, `return fmt.Sprintf("%d", n)`, "Sprintf")

	return root, locs
}

// TestE2E_DefinitionFromDependencyFile covers "Go to Definition" invoked a
// SECOND time from INSIDE a dependency file already opened via a first
// stdlib jump — the scenario a real monorepo report traced through server
// logging to "definition at .../gorm.go:124:30: xref: read facts for
// gorm.io/gorm: store: not found": the facts index only ever covers root
// (workspace) packages (internal/index/scheduler.go's doc), so a query
// landing inside GOROOT or a module cache directory always misses there.
// Before internal/xref.Resolver.New started excluding non-root packages
// from its file/dir lookup tables, that miss surfaced as a wrapped
// "store: not found" error indistinguishable from a genuine index failure,
// even though internal/server.definitionFallback's ad-hoc CheckedPackage +
// SamePackageDefinition + DependencyDefinition chain already answered the
// query correctly regardless. This pins that chain end to end through the
// real server/indexer subprocess, for both a same-package target and a
// cross-package one, both queried from inside the dependency file itself.
func TestE2E_DefinitionFromDependencyFile(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EDependencyDefModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	// First hop: workspace -> stdlib, landing inside GOROOT's fmt/print.go.
	first := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
			Position:     locs.appPos,
		},
	}, e2eIndexBudget)
	if len(first) != 1 {
		t.Fatalf("definition(fmt.Sprintf) returned %d locations, want 1: %+v", len(first), first)
	}
	printGo := first[0].URI.FsPath()
	if !strings.HasSuffix(filepath.ToSlash(printGo), "fmt/print.go") {
		t.Fatalf("definition(fmt.Sprintf) = %s, want a path ending in fmt/print.go (inside GOROOT)", printGo)
	}

	printSrc, err := os.ReadFile(filepath.Clean(printGo))
	if err != nil {
		t.Fatalf("read %s: %v", printGo, err)
	}
	c.openFile(t, printGo)

	t.Run("same_package_target", func(t *testing.T) {
		pos := mustPos(t, string(printSrc), "p := newPrinter()", "newPrinter")
		result := definitionAt(t, c, printGo, pos)
		if len(result) != 1 {
			t.Fatalf("definition(newPrinter) = %+v, want exactly 1 location (same-package, inside fmt itself)", result)
		}
		if got := result[0].URI.FsPath(); !strings.HasSuffix(filepath.ToSlash(got), "fmt/print.go") {
			t.Errorf("definition(newPrinter) = %s, want it to stay inside fmt/print.go", got)
		}
	})

	t.Run("cross_package_target", func(t *testing.T) {
		pos := mustPos(t, string(printSrc), "strconv.AppendInt(b, int64(w)", "AppendInt")
		result := definitionAt(t, c, printGo, pos)
		if len(result) != 1 {
			t.Fatalf("definition(strconv.AppendInt) = %+v, want exactly 1 location (cross-package, into strconv)", result)
		}
		if got := result[0].URI.FsPath(); !strings.Contains(filepath.ToSlash(got), "strconv/") {
			t.Errorf("definition(strconv.AppendInt) = %s, want a path inside strconv (GOROOT)", got)
		}
	})
}
