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

// TestHandleDefinition_FromDependencyFile covers "Go to Definition"
// invoked a SECOND time from INSIDE a dependency file already reached via a
// first stdlib jump (TestHandleDefinition_Stdlib covers only that first
// hop) — the scenario a real monorepo report traced to "definition at
// .../gorm.go:124:30: xref: read facts for gorm.io/gorm: store: not
// found": the facts index only ever covers root (workspace) packages, so a
// query landing inside GOROOT or a module cache directory always misses
// there. Before internal/xref.Resolver.New started excluding non-root
// packages from its own file/dir lookup tables, that miss surfaced as a
// wrapped "store: not found" error indistinguishable from a genuine index
// failure; internal/server.definitionFallback's ad-hoc CheckedPackage +
// SamePackageDefinition + DependencyDefinition chain already answered the
// query correctly regardless (this test pins that it still does), for both
// a same-package target inside the dependency file and a cross-package
// one.
func TestHandleDefinition_FromDependencyFile(t *testing.T) {
	s, snap, _ := newTestServer(t)
	pkg, ok := snap.Packages["example.com/servermod/depuse"]
	if !ok || len(pkg.GoFiles) == 0 {
		t.Fatal("depuse package not found in test workspace")
	}
	file := pkg.GoFiles[0]
	pos := identPositionIn(t, file, mustReadFile(t, file), "Sprintf", 1)

	// First hop: workspace -> stdlib, landing inside GOROOT's fmt/print.go
	// (already covered end to end by TestHandleDefinition_Stdlib; repeated
	// minimally here only to obtain the file this test's own subject
	// queries from).
	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(fmt.Sprintf): %v", err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok || len(locs) != 1 {
		t.Fatalf("handleDefinition(fmt.Sprintf): result = %#v, want a single location", result)
	}
	printGo := locs[0].URI.FsPath()
	if !strings.HasSuffix(filepath.ToSlash(printGo), "fmt/print.go") {
		t.Fatalf("definition file = %q, want it to end with fmt/print.go (inside GOROOT)", printGo)
	}
	printSrc := mustReadFile(t, printGo)

	t.Run("same_package_target", func(t *testing.T) {
		pos := identPositionIn(t, printGo, printSrc, "newPrinter", 1)
		locs := definitionAtFile(t, s, printGo, pos)
		if len(locs) != 1 {
			t.Fatalf("handleDefinition(newPrinter) = %+v, want exactly 1 location (same-package, inside fmt itself)", locs)
		}
		if got := locs[0].URI.FsPath(); !strings.HasSuffix(filepath.ToSlash(got), "fmt/print.go") {
			t.Errorf("handleDefinition(newPrinter) = %s, want it to stay inside fmt/print.go", got)
		}
	})

	t.Run("cross_package_target", func(t *testing.T) {
		pos := identPositionIn(t, printGo, printSrc, "AppendInt", 1)
		locs := definitionAtFile(t, s, printGo, pos)
		if len(locs) != 1 {
			t.Fatalf("handleDefinition(AppendInt) = %+v, want exactly 1 location (cross-package, into strconv)", locs)
		}
		if got := locs[0].URI.FsPath(); !strings.Contains(filepath.ToSlash(got), "strconv/") {
			t.Errorf("handleDefinition(AppendInt) = %s, want a path inside strconv (GOROOT)", got)
		}
	})
}

// definitionAtFile sends a single textDocument/definition request for
// (path, pos) directly through s.handleDefinition (bypassing the LSP
// transport, like every other test in this file) and returns its
// unmarshaled result.
func definitionAtFile(t *testing.T, s *Server, path string, pos protocol.Position) protocol.LocationSlice {
	t.Helper()
	result, err := s.handleDefinition(context.Background(), mustMarshal(t, &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(path)},
			Position:     pos,
		},
	}))
	if err != nil {
		t.Fatalf("handleDefinition(%s): %v", path, err)
	}
	locs, ok := result.(protocol.LocationSlice)
	if !ok {
		t.Fatalf("handleDefinition(%s): result = %#v, want protocol.LocationSlice", path, result)
	}
	return locs
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
