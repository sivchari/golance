package golance_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// identOccurrencePosition returns the LSP position of the occurrence-th
// (1-based) plain *ast.Ident named name in path, read and parsed fresh from
// disk — selectorIdentPosition's bare-identifier counterpart, for a query
// position that is not part of a "pkgAlias.sel" selector (an unqualified
// function call, or a local variable, both declared and used within the
// SAME package — e.g. everything TestE2E_DependencyFullBodyNavigation
// queries INSIDE the dependency file itself).
func identOccurrencePosition(t *testing.T, path, name string, occurrence int) protocol.Position {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var positions []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		positions = append(positions, id.Pos())
		return true
	})
	if occurrence < 1 || occurrence > len(positions) {
		t.Fatalf("%s: found %d occurrence(s) of %s, want at least %d", path, len(positions), name, occurrence)
	}
	return posAtOffset(t, path, fset, data, positions[occurrence-1])
}

// docCommentOf returns the doc comment recorded for the top-level type
// declaration named name in path, read and parsed fresh from disk (with
// parser.ParseComments) — the ground truth
// TestE2E_HoverDependencyDocComment checks a live hover response against,
// rather than hardcoding a version-fragile doc string.
func docCommentOf(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				continue
			}
			if ts.Doc != nil {
				return ts.Doc.Text()
			}
			if gd.Doc != nil {
				return gd.Doc.Text()
			}
		}
	}
	t.Fatalf("%s: no doc comment found for type %s", path, name)
	return ""
}

// hoverMarkdown requests textDocument/hover at pos in file and returns its
// rendered markdown, failing t if the request errors, returns no hover, or
// its Contents is not the MarkupContent shape every golance hover response
// uses (see hoverMarkdown in handlers_langfeat.go).
func hoverMarkdown(t *testing.T, c *lspClient, file string, pos protocol.Position) string {
	t.Helper()
	resp := c.call(t, protocol.MethodTextDocumentHover, &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
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
	return md.Value
}

// typeDefinitionAt requests textDocument/typeDefinition at pos in file and
// returns the (non-empty) result, failing t otherwise — typeDefinitionAt's
// definitionAt counterpart (e2e_worktree_test.go).
func typeDefinitionAt(t *testing.T, c *lspClient, file string, pos protocol.Position) protocol.LocationSlice {
	t.Helper()
	return c.waitForNonEmptyLocations(t, protocol.MethodTextDocumentTypeDefinition, &protocol.TypeDefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(file)},
			Position:     pos,
		},
	}, e2eRequestBudget)
}

// TestE2E_DependencyFullBodyNavigation drives a real golance binary against
// golance's OWN repository root (like TestE2E_DependencyDefinitionExactColumn:
// only golance's own go.mod/go.sum already resolves a real, non-stdlib
// dependency without fabricating one) and covers Phase 3's unification of
// files opened INSIDE a dependency onto internal/depcheck's full-bodies mode
// (see internal/depcheck.Provider.PackageWithBodies and
// internal/server.resolveCheckedPackage): before this phase, such a file
// was still checked through internal/check.Engine's own export-data-import
// pipeline, degrading hover on a body-local symbol to nothing at all and
// giving an onward jump export-data's line-only, exported-only positions.
//
//   - hover on a body-local symbol proves full-body type information is
//     present at all (a declarations-only check, as internal/check.Engine's
//     superseded pipeline for this case ran, never populates a statement's
//     local variable in types.Info.Defs/Uses).
//   - definition to another file of the SAME dependency package proves
//     cross-file navigation stays inside the depcheck-checked package's own
//     identity (go.etcd.io/bbolt's Open, in db.go, calling getDiscardLogger,
//     declared in logger.go).
//   - definition onward to the dependency's OWN dependency
//     (go.etcd.io/bbolt/internal/common) proves the recursive
//     "re-entrant check on demand" importer chain (depcheck.Provider's own
//     doc) still resolves beyond the first hop, exact to the column.
//   - typeDefinition on a workspace file's reference to a dependency type
//     proves item 3's early-return fix (typeDefinitionCrossPackage's
//     dependencyTypeDeclaration fallback): the facts index never covers a
//     non-root package, so before that fix this returned nothing at all.
func TestE2E_DependencyFullBodyNavigation(t *testing.T) {
	skipUnlessE2E(t)

	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	storeFile := filepath.Join(root, "internal", "store", "store.go")

	c := startClient(t, root)
	c.initialize(t, root)
	c.openFile(t, storeFile)
	c.waitForIndexReady(t)

	// First hop: workspace -> go.etcd.io/bbolt, landing inside its own
	// db.go (the same file store.go's own bbolt.DB field type declaration
	// and bbolt.Open both resolve into).
	openPos := selectorIdentPosition(t, storeFile, "bbolt", "Open", 1)
	first := definitionAt(t, c, storeFile, openPos)
	if len(first) != 1 {
		t.Fatalf("definition(bbolt.Open) = %+v, want exactly 1 location", first)
	}
	dbGoFile := first[0].URI.FsPath()
	if !strings.HasSuffix(filepath.ToSlash(dbGoFile), "db.go") {
		t.Fatalf("definition(bbolt.Open) = %s, want a path ending in db.go (inside the go.etcd.io/bbolt module cache)", dbGoFile)
	}
	c.openFile(t, dbGoFile)

	t.Run("hover_local_symbol_full_body", func(t *testing.T) {
		// lg is declared "lg := db.Logger()" -- a body-local variable whose
		// static type (bbolt's own Logger interface) is resolved only when
		// statement-level type-checking actually ran; a declarations-only
		// check (IgnoreFuncBodies, the superseded pipeline for this case)
		// never populates types.Info.Defs/Uses for it at all, so hover would
		// return nil, not a Logger-typed signature.
		pos := identOccurrencePosition(t, dbGoFile, "lg", 1)
		md := hoverMarkdown(t, c, dbGoFile, pos)
		if !strings.Contains(md, "lg") || !strings.Contains(md, "Logger") {
			t.Errorf("hover(lg) = %q, want its signature to name both lg and its Logger type", md)
		}
	})

	t.Run("definition_to_another_file_of_the_dependency", func(t *testing.T) {
		pos := identOccurrencePosition(t, dbGoFile, "getDiscardLogger", 1)
		got := definitionAt(t, c, dbGoFile, pos)
		if len(got) != 1 {
			t.Fatalf("definition(getDiscardLogger) = %+v, want exactly 1 location", got)
		}
		gotPath := got[0].URI.FsPath()
		if !strings.HasSuffix(filepath.ToSlash(gotPath), "logger.go") {
			t.Fatalf("definition(getDiscardLogger) = %s, want a path ending in logger.go (another file of the SAME bbolt package)", gotPath)
		}
		want := declPosition(t, gotPath, "getDiscardLogger")
		if got[0].Range.Start != want {
			t.Errorf("definition(getDiscardLogger) landed at %+v, want %+v (its own declaring identifier in %s)", got[0].Range.Start, want, gotPath)
		}
	})

	t.Run("definition_onward_to_a_dependency_of_the_dependency", func(t *testing.T) {
		pos := selectorIdentPosition(t, dbGoFile, "common", "DefaultMaxBatchSize", 1)
		got := definitionAt(t, c, dbGoFile, pos)
		if len(got) != 1 {
			t.Fatalf("definition(common.DefaultMaxBatchSize) = %+v, want exactly 1 location", got)
		}
		gotPath := got[0].URI.FsPath()
		// The module cache directory is "go.etcd.io/bbolt@<version>/internal/common",
		// not "bbolt/internal/common" — the "@<version>" suffix rules out a
		// plain substring check on the unversioned import path.
		slash := filepath.ToSlash(gotPath)
		if !strings.Contains(slash, "go.etcd.io/bbolt@") || !strings.HasSuffix(slash, "/internal/common/types.go") {
			t.Fatalf("definition(common.DefaultMaxBatchSize) = %s, want a path inside the go.etcd.io/bbolt module cache's internal/common package", gotPath)
		}
		want := declPosition(t, gotPath, "DefaultMaxBatchSize")
		if got[0].Range.Start != want {
			t.Errorf("definition(common.DefaultMaxBatchSize) landed at %+v, want %+v (its own declaring identifier in %s)", got[0].Range.Start, want, gotPath)
		}
	})

	t.Run("typedefinition_cross_package_from_workspace_into_a_dependency", func(t *testing.T) {
		// store.go's own "bolt *bbolt.DB" field — the same reference
		// openPos's own definition(bbolt.Open) hop is unrelated to; this is
		// a typeDefinition query, not a definition one, and targets the
		// TYPE bbolt.DB rather than the func bbolt.Open.
		pos := selectorIdentPosition(t, storeFile, "bbolt", "DB", 1)
		got := typeDefinitionAt(t, c, storeFile, pos)
		if len(got) != 1 {
			t.Fatalf("typeDefinition(bbolt.DB) = %+v, want exactly 1 location", got)
		}
		gotPath := got[0].URI.FsPath()
		if !strings.HasSuffix(filepath.ToSlash(gotPath), "db.go") {
			t.Fatalf("typeDefinition(bbolt.DB) = %s, want a path ending in db.go (inside the go.etcd.io/bbolt module cache)", gotPath)
		}
		want := declPosition(t, gotPath, "DB")
		if got[0].Range.Start != want {
			t.Errorf("typeDefinition(bbolt.DB) landed at %+v, want %+v (DB's own declaring identifier in %s) — before item 3's fix this returned no results at all, since the facts index never covers a non-root package", got[0].Range.Start, want, gotPath)
		}
	})
}

// TestE2E_HoverDependencyDocComment drives a real golance binary over a
// synthetic temp module and verifies that hovering a standard library
// symbol from a WORKSPACE file shows its real doc comment — item 2's fix
// (internal/depcheck.Provider.DocAt, wired through
// internal/server.crossPackageDoc): before this phase, a cross-package
// hover's Doc was always empty (internal/check.Engine's export-data-decoded
// object carries no source-level doc comment at all).
func TestE2E_HoverDependencyDocComment(t *testing.T) {
	skipUnlessE2E(t)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	writeE2EFile(t, root, "go.mod", "module example.com/e2ehoverdoc\n\ngo 1.23\n")

	const appSrc = `package app

import "strings"

func NewBuilder() strings.Builder {
	var b strings.Builder
	return b
}
`
	appFile := writeE2EFile(t, root, "app/app.go", appSrc)
	// "var b strings.Builder", not the func signature's identical
	// "strings.Builder" return type: that line's first "Builder" substring
	// match falls inside "NewBuilder" itself (mustPos finds the first
	// substring match on the line, not a token-boundary match).
	builderPos := mustPos(t, appSrc, "var b strings.Builder", "Builder")

	c := startClient(t, root)
	c.initialize(t, root)
	c.openFile(t, appFile)
	c.waitForIndexReady(t)

	// Discover strings.Builder's real declaration file via the live
	// server's own definition, rather than assuming its filename, so the
	// ground truth docCommentOf reads is the exact file this hover result
	// itself points at.
	defLoc := definitionAt(t, c, appFile, builderPos)
	if len(defLoc) != 1 {
		t.Fatalf("definition(strings.Builder) = %+v, want exactly 1 location", defLoc)
	}
	builderFile := defLoc[0].URI.FsPath()
	wantDoc := docCommentOf(t, builderFile, "Builder")
	wantFirstLine := strings.TrimSpace(strings.SplitN(wantDoc, "\n", 2)[0])
	if wantFirstLine == "" {
		t.Fatalf("docCommentOf(%s, Builder) returned an empty doc comment", builderFile)
	}

	md := hoverMarkdown(t, c, appFile, builderPos)
	if !strings.Contains(md, wantFirstLine) {
		t.Errorf("hover(strings.Builder) = %q, want it to contain %q (strings.Builder's own real GOROOT doc comment, read from %s)", md, wantFirstLine, builderFile)
	}
}
