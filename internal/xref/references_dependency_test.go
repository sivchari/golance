package xref

import (
	"context"
	"testing"
)

// TestReferences_DependencySymbol verifies References answers
// workspace-wide for a symbol DECLARED IN A DEPENDENCY (here,
// strings.TrimSpace, a stdlib symbol never itself indexed as a root
// package — see New's fileToPkg doc): clicking a use of it in one root
// package must still find every other root package's own use, exactly like
// a workspace-declared symbol, since addRef (internal/index/facts.go)
// already records every outgoing reference — including one to a
// dependency's symbol — under a SymbolID stable across every package that
// independently type-checks it (symbolID's own doc). Before
// resolveRefTarget's fix, this returned an empty result: it required the
// target's OWN defining package (here, "strings") to itself have facts
// recorded, which it never does for a dependency.
func TestReferences_DependencySymbol(t *testing.T) {
	r, snap := newTestResolver(t)

	depuse1File := goFile(t, snap, "example.com/xrefmod/depuse1", "depuse1.go")
	depuse2File := goFile(t, snap, "example.com/xrefmod/depuse2", "depuse2.go")

	line, col := identOccurrence(t, depuse1File, "TrimSpace")
	locs, err := r.References(context.Background(), depuse1File, line, col, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}

	wantDepuse2 := identOccurrences(t, depuse2File, "TrimSpace")
	// depuse1 has 1 use, depuse2 has 2 — the declaration itself (in
	// "strings", never indexed) cannot be included, so the total is exactly
	// the sum of workspace uses, not uses+1.
	if want := 1 + len(wantDepuse2); len(locs) != want {
		t.Fatalf("References = %d locations, want %d (all locations: %+v)", len(locs), want, locs)
	}

	counts := map[string]int{}
	for _, l := range locs {
		counts[l.File]++
	}
	if counts[depuse1File] != 1 {
		t.Errorf("References in %s = %d, want 1: %+v", depuse1File, counts[depuse1File], locs)
	}
	if counts[depuse2File] != len(wantDepuse2) {
		t.Errorf("References in %s = %d, want %d: %+v", depuse2File, counts[depuse2File], len(wantDepuse2), locs)
	}

	for _, want := range wantDepuse2 {
		found := false
		for _, l := range locs {
			if l.File == depuse2File && int(l.Line) == want.Line && int(l.Col) == want.Column {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("References missing %s:%d:%d: %+v", depuse2File, want.Line, want.Column, locs)
		}
	}
}

// TestReferences_DependencySymbol_ExcludeDeclarationUnaffected verifies
// includeDecl=false answers identically to includeDecl=true for a
// dependency symbol: there was never a declaration location to add in
// either case (see TestReferences_DependencySymbol's doc), so the two
// calls must return the same set.
func TestReferences_DependencySymbol_ExcludeDeclarationUnaffected(t *testing.T) {
	r, snap := newTestResolver(t)

	depuse1File := goFile(t, snap, "example.com/xrefmod/depuse1", "depuse1.go")
	line, col := identOccurrence(t, depuse1File, "TrimSpace")

	withDecl, err := r.References(context.Background(), depuse1File, line, col, true)
	if err != nil {
		t.Fatalf("References(includeDecl=true): %v", err)
	}
	withoutDecl, err := r.References(context.Background(), depuse1File, line, col, false)
	if err != nil {
		t.Fatalf("References(includeDecl=false): %v", err)
	}
	if len(withDecl) != len(withoutDecl) {
		t.Errorf("References(includeDecl=true) = %d locations, References(includeDecl=false) = %d, want equal (dependency symbol has no answerable declaration): %+v vs %+v", len(withDecl), len(withoutDecl), withDecl, withoutDecl)
	}
}

// TestReferences_WorkspaceSymbolReferencesUnchanged is a regression guard
// for resolveRefTarget's fix: a reference to a workspace (root-package)
// symbol must still resolve its Kind/Name from the facts index as before —
// covered already by TestReferences_SpansDefiningAndReferencingPackages,
// this only pins that the errSymbolNotFound fallback path added for
// dependency symbols does not accidentally also swallow a genuine
// root-package resolution failure into a silent, wrong answer for a case
// that should still return decl info: References with includeDecl=true on
// impl.Person (a root-declared type) must still include the declaration
// itself, byte-exact.
func TestReferences_WorkspaceSymbolReferencesUnchanged(t *testing.T) {
	r, snap := newTestResolver(t)

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	locs, err := r.References(context.Background(), implFile, line, col, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	found := false
	for _, l := range locs {
		if l.File == implFile && int(l.Line) == line && int(l.Col) == col {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("References(includeDecl=true) on a root-declared symbol did not include its own declaration: %+v", locs)
	}
}
