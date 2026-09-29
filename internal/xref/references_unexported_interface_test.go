package xref

import (
	"context"
	"testing"
)

// TestReferences_MethodOnUnexportedInterface_FindsImplementerCallSite pins
// F5: References on a method of an UNEXPORTED interface used to silently
// drop the implementer's own call sites, with no log at all, because
// correspondingMethodSymbols' interface branch decoded the interface via
// resolveNamedOK -- export data never carries an unexported package-scope
// type, so that decode always failed and the whole corresponding-methods
// lookup silently degraded to nothing. The interface method's own direct
// (interface-typed) call site was still found; only the concrete
// implementer's OWN call site, reached only through
// correspondingMethodSymbols, was missing.
func TestReferences_MethodOnUnexportedInterface_FindsImplementerCallSite(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/unexportediface\n\ngo 1.23\n")
	writeTestFile(t, dir, "repo/repo.go", `package repo

// fetcher is unexported: its own package's export data never carries it at
// all.
type fetcher interface {
	BatchGet() []int
}

type Impl struct{}

func (i Impl) BatchGet() []int { return nil }

// CallInterface calls BatchGet through the unexported interface type.
func CallInterface() []int {
	var f fetcher = Impl{}
	return f.BatchGet()
}

// CallConcrete calls BatchGet directly on the concrete implementer.
func CallConcrete() []int {
	i := Impl{}
	return i.BatchGet()
}
`)

	r, snap := newResolverForDir(t, dir)
	repoFile := goFile(t, snap, "example.com/unexportediface/repo", "repo.go")

	line, col := identOccurrence(t, repoFile, "BatchGet") // the interface's own method declaration
	locs, err := r.References(context.Background(), repoFile, line, col, false)
	if err != nil {
		t.Fatalf("References(fetcher.BatchGet): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("References(fetcher.BatchGet) = %+v, want 2 (CallInterface's interface-typed call plus CallConcrete's own call)", locs)
	}
	// occurrence 0 is the interface's own declaration, 1 is Impl's own
	// declaration, 2 is CallInterface's call, 3 is CallConcrete's call.
	occurrences := identOccurrences(t, repoFile, "BatchGet")
	wantBatchGetLoc(t, locs, repoFile, occurrences[2]) // CallInterface's f.BatchGet()
	wantBatchGetLoc(t, locs, repoFile, occurrences[3]) // CallConcrete's i.BatchGet()
}
