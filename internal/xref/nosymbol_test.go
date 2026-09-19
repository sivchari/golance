package xref

import (
	"context"
	"errors"
	"testing"
)

// TestResolveAt_NoSymbolMissWrapsErrNoSymbolAt pins that every "no symbol at
// this position" style resolveAt failure -- a file outside the facts
// index's coverage, or a position matching no reference or definition in an
// indexed file -- is distinguishable via errors.Is(err, ErrNoSymbolAt), so
// internal/server's handlers can treat this ordinary miss differently from a
// genuine facts-read failure without matching message text (see
// handleReferences/handleImplementation in internal/server).
func TestResolveAt_NoSymbolMissWrapsErrNoSymbolAt(t *testing.T) {
	t.Run("file not part of any known package", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, dir, "go.mod", "module example.com/nosymbolmod\n\ngo 1.23\n")
		writeTestFile(t, dir, "app/app.go", `package app

import "fmt"

func F() int { return len(fmt.Sprintf("x")) }
`)
		r, snap := newResolverForDir(t, dir)
		fmtPkg, ok := snap.Package("fmt")
		if !ok || len(fmtPkg.GoFiles) == 0 {
			t.Fatal("fmt package not found in loaded snapshot")
		}

		_, err := r.resolveAt(context.Background(), fmtPkg.GoFiles[0], 1, 1)
		if err == nil {
			t.Fatal("resolveAt succeeded for a dependency file position, want an error")
		}
		if !errors.Is(err, ErrNoSymbolAt) {
			t.Errorf("resolveAt error = %v, want it to wrap ErrNoSymbolAt", err)
		}
	})

	t.Run("position matches no reference or definition", func(t *testing.T) {
		r, snap := newTestResolver(t)
		implFile := goFile(t, snap, pkgImpl, "impl.go")

		// impl.go's first line is a doc comment, never indexed as a
		// reference or a definition.
		_, err := r.resolveAt(context.Background(), implFile, 1, 1)
		if err == nil {
			t.Fatal("resolveAt succeeded at a doc comment position, want a no-symbol miss")
		}
		if !errors.Is(err, ErrNoSymbolAt) {
			t.Errorf("resolveAt error = %v, want it to wrap ErrNoSymbolAt", err)
		}
	})
}
