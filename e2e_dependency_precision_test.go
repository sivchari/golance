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

	"github.com/sivchari/golance/internal/overlay"
)

// selectorIdentPosition returns the LSP position of the occurrence-th
// (1-based) "pkgAlias.sel" selector's Sel identifier in path, read and
// parsed fresh from disk — a precise query position for "Go to Definition"
// on a specific cross-package reference, unambiguous even when sel is also
// a locally-declared identifier elsewhere in the same file (as with
// internal/store.Open alongside bbolt.Open, immediately below it).
func selectorIdentPosition(t *testing.T, path, pkgAlias, sel string, occurrence int) protocol.Position {
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
		se, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := se.X.(*ast.Ident)
		if !ok || id.Name != pkgAlias || se.Sel.Name != sel {
			return true
		}
		positions = append(positions, se.Sel.Pos())
		return true
	})
	if occurrence < 1 || occurrence > len(positions) {
		t.Fatalf("%s: found %d occurrences of %s.%s, want at least %d", path, len(positions), pkgAlias, sel, occurrence)
	}
	return posAtOffset(t, path, fset, data, positions[occurrence-1])
}

// declPosition returns the LSP position of name's own top-level func/type
// declaring identifier in path — the ground truth
// TestE2E_DependencyDefinitionExactColumn checks the live server's
// definition result against, derived by parsing the real target file
// directly rather than hardcoding a version-fragile line/column constant.
func declPosition(t *testing.T, path, name string) protocol.Position {
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
	var found *ast.Ident
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == name {
				found = d.Name
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == name {
					found = ts.Name
				}
			}
		}
	}
	if found == nil {
		t.Fatalf("no top-level func/type declaration named %q in %s", name, path)
	}
	return posAtOffset(t, path, fset, data, found.Pos())
}

// posAtOffset converts pos (valid in fset, which parsed data from path)
// into the LSP (UTF-16) position textDocument/definition results use.
func posAtOffset(t *testing.T, path string, fset *token.FileSet, data []byte, pos token.Pos) protocol.Position {
	t.Helper()
	tf := fset.File(pos)
	offset := tf.Offset(pos)
	p, ok := overlay.UTF16PositionForByteOffset(data, offset)
	if !ok {
		t.Fatalf("%s: offset %d out of range", path, offset)
	}
	return p
}

// TestE2E_DependencyDefinitionExactColumn verifies, through a real running
// golance session, that "Go to Definition" into a real module dependency
// (go.etcd.io/bbolt — one of golance's own go.mod requirements) lands at
// the EXACT line and column of the declaration, not merely the correct
// file — the precision bar internal/depcheck's source-checked dependency
// provider exists to meet (see the provider's own package doc and
// internal/langfeat.DependencyDefinition). This runs golance against its
// OWN repository root rather than a synthetic temp module: only golance's
// own go.mod/go.sum already resolves a real, non-stdlib dependency without
// this test fabricating (and needing to keep in sync) one of its own.
func TestE2E_DependencyDefinitionExactColumn(t *testing.T) {
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

	pos := selectorIdentPosition(t, storeFile, "bbolt", "Open", 1)
	got := definitionAt(t, c, storeFile, pos)
	if len(got) != 1 {
		t.Fatalf("definition(bbolt.Open) = %+v, want exactly 1 location", got)
	}
	gotPath := got[0].URI.FsPath()
	if !strings.Contains(filepath.ToSlash(gotPath), "go.etcd.io/bbolt@") {
		t.Fatalf("definition(bbolt.Open) = %s, want a path inside the go.etcd.io/bbolt module cache", gotPath)
	}
	want := declPosition(t, gotPath, "Open")
	if got[0].Range.Start != want {
		t.Errorf("definition(bbolt.Open) landed at %+v, want %+v (Open's own declaring identifier in %s)", got[0].Range.Start, want, gotPath)
	}
}
