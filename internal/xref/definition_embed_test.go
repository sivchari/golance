package xref

import (
	"context"
	"testing"
)

// TestDefinition_EmbeddedStructField covers "Go to Definition" invoked on
// an embedded struct field's name, through the primary workspace facts
// index path (resolveAt), for both a same-package and a cross-package
// embedder. Per gopls (golang/go#42254), it must jump to the embedded
// TYPE's own declaration. Unlike langfeat.SamePackageDefinition's index-
// unavailable fallback (see internal/langfeat/definition_test.go's
// TestSamePackageDefinition_EmbeddedField), this path already works without
// any special-casing: an embedded field's identifier is BOTH a definition
// (the implicit field, in facts' symbol table) and a reference (the type
// name, in facts' ref table, from types.Info.Uses) at the identical
// position, and resolveAt checks the ref table first -- see resolveAt's
// doc. This test pins that this stays true.
func TestDefinition_EmbeddedStructField(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/embeddef\n\ngo 1.23\n")
	writeTestFile(t, dir, "base/base.go", `package base

// Base is embedded by Wrapper across packages.
type Base struct{}
`)
	writeTestFile(t, dir, "wrapper/wrapper.go", `package wrapper

import "example.com/embeddef/base"

// Local is embedded by Wrapper within the same package.
type Local struct{}

// Wrapper embeds Local (same package) and base.Base (cross package).
type Wrapper struct {
	Local
	base.Base
}
`)

	r, snap := newResolverForDir(t, dir)
	wrapperFile := goFile(t, snap, "example.com/embeddef/wrapper", "wrapper.go")
	baseFile := goFile(t, snap, "example.com/embeddef/base", "base.go")

	t.Run("same package", func(t *testing.T) {
		occ := identOccurrences(t, wrapperFile, "Local")
		if len(occ) != 2 {
			t.Fatalf("expected 2 occurrences of Local (decl + embedded field), got %d", len(occ))
		}
		locs, err := r.Definition(context.Background(), wrapperFile, occ[1].Line, occ[1].Column)
		if err != nil {
			t.Fatalf("Definition(Local embedded field): %v", err)
		}
		wantLoc(t, locs, wrapperFile, "Local")
	})

	t.Run("cross package", func(t *testing.T) {
		line, col := identOccurrence(t, wrapperFile, "Base")
		locs, err := r.Definition(context.Background(), wrapperFile, line, col)
		if err != nil {
			t.Fatalf("Definition(base.Base embedded field): %v", err)
		}
		wantLoc(t, locs, baseFile, "Base")
	})
}
