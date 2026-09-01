package golance_test

import (
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// e2eEmbeddedDefLocs records the exact 0-based positions
// TestE2E_DefinitionOnEmbeddedName queries, captured while the synthetic
// module is written so nothing is re-parsed at query time.
type e2eEmbeddedDefLocs struct {
	baseFile string // base/base.go
	baseSrc  string

	appFile string // app/app.go
	appSrc  string

	localFieldPos protocol.Position // "Local" as Holder's own-package embedded field
	baseFieldPos  protocol.Position // "Base" as Holder's cross-package embedded field
	readerPos     protocol.Position // "Reader" as Reader's stdlib embedded interface
}

func writeE2EEmbeddedDefModule(t *testing.T) (string, e2eEmbeddedDefLocs) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}

	var locs e2eEmbeddedDefLocs
	writeE2EFile(t, root, "go.mod", "module example.com/e2eembeddef\n\ngo 1.23\n")

	const baseSrc = `package base

// Base is embedded by app.Holder across packages.
type Base struct{}
`
	locs.baseFile = writeE2EFile(t, root, "base/base.go", baseSrc)
	locs.baseSrc = baseSrc

	const appSrc = `package app

import (
	"io"

	"example.com/e2eembeddef/base"
)

// Local is embedded by Holder within the same package.
type Local struct{}

// Holder embeds Local (same package) and base.Base (cross package).
type Holder struct {
	Local
	base.Base
}

// Reader embeds io.Reader (standard library).
type Reader interface {
	io.Reader
}
`
	locs.appFile = writeE2EFile(t, root, "app/app.go", appSrc)
	locs.appSrc = appSrc
	locs.localFieldPos = mustPos(t, appSrc, "\tLocal", "Local")
	locs.baseFieldPos = mustPos(t, appSrc, "\tbase.Base", "Base")
	locs.readerPos = mustPos(t, appSrc, "\tio.Reader", "Reader")

	return root, locs
}

// TestE2E_DefinitionOnEmbeddedName covers "Go to Definition" invoked on an
// embedded field/interface name itself, per gopls's own behavior
// (golang/go#42254): the cursor must jump to the embedded TYPE's own
// declaration, not resolve to the struct's implicit field (which sits at
// the identical source position and, unfixed, makes the query resolve to
// itself). This exercises all three resolution paths: same-package
// (workspace facts index, own package), cross-package workspace (facts
// index, reverse-dependency), and standard library (definitionFallback's
// export-data path, since the facts index never covers non-root packages).
func TestE2E_DefinitionOnEmbeddedName(t *testing.T) {
	skipUnlessE2E(t)

	root, locs := writeE2EEmbeddedDefModule(t)
	c := startClient(t, root)
	c.initialize(t, root)

	c.openFile(t, locs.appFile)
	c.waitForIndexReady(t)

	t.Run("same_package_embedded_struct_field", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
				Position:     locs.localFieldPos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("definition(Local) returned %d locations, want 1: %+v", len(result), result)
		}
		wantDecl := mustPos(t, locs.appSrc, "type Local struct{}", "Local")
		if gotPath := result[0].URI.FsPath(); gotPath != locs.appFile || result[0].Range.Start.Line != wantDecl.Line {
			t.Errorf("definition(Local) = %s:%d, want %s:%d (Local's own declaration, not the field itself)", gotPath, result[0].Range.Start.Line, locs.appFile, wantDecl.Line)
		}
	})

	t.Run("cross_package_embedded_struct_field", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
				Position:     locs.baseFieldPos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("definition(base.Base) returned %d locations, want 1: %+v", len(result), result)
		}
		wantDecl := mustPos(t, locs.baseSrc, "type Base struct{}", "Base")
		if gotPath := result[0].URI.FsPath(); gotPath != locs.baseFile || result[0].Range.Start.Line != wantDecl.Line {
			t.Errorf("definition(base.Base) = %s:%d, want %s:%d (base.Base's own declaration)", gotPath, result[0].Range.Start.Line, locs.baseFile, wantDecl.Line)
		}
	})

	t.Run("stdlib_embedded_interface", func(t *testing.T) {
		result := c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentDefinition, &protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(locs.appFile)},
				Position:     locs.readerPos,
			},
		}, e2eIndexBudget)
		if len(result) != 1 {
			t.Fatalf("definition(io.Reader) returned %d locations, want 1: %+v", len(result), result)
		}
		gotPath := result[0].URI.FsPath()
		if gotPath == locs.appFile {
			t.Fatalf("definition(io.Reader) = %s, want the standard library's io package, not app.go itself", gotPath)
		}
		if !strings.HasSuffix(filepath.ToSlash(gotPath), "io/io.go") {
			t.Errorf("definition(io.Reader) = %s, want a path ending in io/io.go", gotPath)
		}
	})
}
