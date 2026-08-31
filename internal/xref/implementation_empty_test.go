package xref

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestImplementation_EmptyInterfaceReturnsNoResults pins the intentional
// behavior documented on implementationsOfInterface/interfacesImplementedBy:
// "Go to Implementations" on interface{}/any (and symmetrically, on a
// zero-method concrete type) always returns no results, rather than
// enumerating every type in the workspace. Uses its own synthetic module
// since neither direction needs testdata/module's fixed iface/impl pair.
func TestImplementation_EmptyInterfaceReturnsNoResults(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/emptyiface\n\ngo 1.23\n")
	writeTestFile(t, dir, "types/types.go", `package types

// Empty is the empty interface, spelled out as a named type so it has a
// declaration Implementation can be invoked on.
type Empty interface{}

// Plain has no methods, so it trivially implements Empty (and every other
// zero-method interface).
type Plain struct{}
`)

	r, snap := newResolverForDir(t, dir)
	typesFile := goFile(t, snap, "example.com/emptyiface/types", "types.go")

	line, col := identOccurrence(t, typesFile, "Empty")
	locs, err := r.Implementation(context.Background(), typesFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Empty): %v", err)
	}
	if len(locs) != 0 {
		t.Errorf("Implementation(Empty) = %+v, want no results for the empty interface", locs)
	}

	line, col = identOccurrence(t, typesFile, "Plain")
	locs, err = r.Implementation(context.Background(), typesFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Plain): %v", err)
	}
	if len(locs) != 0 {
		t.Errorf("Implementation(Plain) = %+v, want no results for a zero-method concrete type", locs)
	}
}

// writeTestFile writes content to rel under dir, creating parent
// directories as needed.
func writeTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}
