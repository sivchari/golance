package xref

import (
	"context"
	"fmt"
	"testing"
)

// TestImplementation_PromotedStructMethod covers "Go to Implementation"
// invoked on an interface METHOD name (methodImplementations, the
// interface -> implementers direction), when the implementer only has that
// method via struct embedding. Before the fix, concreteMethodLocation built
// the promoted method's SymbolID against the embedding type's OWN package
// instead of the embedded field's declaring package, so the lookup always
// missed and Implementation silently returned no results for this method
// even though the type-level "Go to Implementation" on the interface's own
// name found the implementer just fine (registerMethodSet already indexes
// promoted method names against the embedding type).
func TestImplementation_PromotedStructMethod(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/embedstruct\n\ngo 1.23\n")
	writeTestFile(t, dir, "base/base.go", `package base

// Base declares Close; Wrapper implements it only by embedding Base.
type Base struct{}

func (b Base) Close() error { return nil }
`)
	writeTestFile(t, dir, "multi/multi.go", `package multi

// Multi requires both a directly satisfiable method and one Wrapper only
// gets via embedding.
type Multi interface {
	A() string
	Close() error
}
`)
	writeTestFile(t, dir, "wrapper/wrapper.go", `package wrapper

import "example.com/embedstruct/base"

// Wrapper implements Multi: A directly, Close via embedding base.Base.
type Wrapper struct {
	base.Base
}

func (w Wrapper) A() string { return "a" }
`)

	r, snap := newResolverForDir(t, dir)
	multiFile := goFile(t, snap, "example.com/embedstruct/multi", "multi.go")
	baseFile := goFile(t, snap, "example.com/embedstruct/base", "base.go")

	// The interface's own name still finds Wrapper as an implementer.
	line, col := identOccurrence(t, multiFile, "Multi")
	locs, err := r.Implementation(context.Background(), multiFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Multi): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Multi) = %+v, want 1 result (Wrapper)", locs)
	}

	// The promoted method's own name must resolve to Base's declaration --
	// Wrapper has no Close of its own to point at.
	line, col = identOccurrence(t, multiFile, "Close")
	locs, err = r.Implementation(context.Background(), multiFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Multi.Close): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Multi.Close) = %+v, want 1 result (base.Base.Close)", locs)
	}
	wantLoc(t, locs, baseFile, "Close")
}

// TestImplementation_PromotedInterfaceMethod covers the mirror direction
// (methodInterfaces, concrete -> interfaces it satisfies), invoked on a
// concrete method whose satisfied interface only requires that method via
// embedding another interface. Before the fix, interfaceMethodLocation had
// the identical bug: it built the promoted interface method's SymbolID
// against the embedding INTERFACE's own package instead of the embedded
// interface's declaring package.
func TestImplementation_PromotedInterfaceMethod(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/embediface\n\ngo 1.23\n")
	writeTestFile(t, dir, "base/base.go", `package base

// Closer declares Close; Multi promotes it by embedding Closer.
type Closer interface {
	Close() error
}
`)
	writeTestFile(t, dir, "multi/multi.go", `package multi

import "example.com/embediface/base"

// Multi requires Close only via embedding base.Closer.
type Multi interface {
	base.Closer
	A() string
}
`)
	writeTestFile(t, dir, "wrapper/wrapper.go", `package wrapper

// Wrapper implements Multi directly (no embedding on this side).
type Wrapper struct{}

func (w Wrapper) A() string    { return "a" }
func (w Wrapper) Close() error { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	wrapperFile := goFile(t, snap, "example.com/embediface/wrapper", "wrapper.go")
	baseFile := goFile(t, snap, "example.com/embediface/base", "base.go")

	positions := identOccurrences(t, wrapperFile, "Close")
	locs, err := r.Implementation(context.Background(), wrapperFile, positions[0].Line, positions[0].Column)
	if err != nil {
		t.Fatalf("Implementation(Wrapper.Close): %v", err)
	}
	// Wrapper structurally satisfies both Multi (via its promoted Close)
	// and base.Closer itself, so the promoted-through-Multi resolution --
	// the one this test exists to pin -- must be present alongside it.
	wantLoc(t, locs, baseFile, "Close")
}

// TestImplementation_ManyUnrelatedCandidatesStaysSound covers
// implementingTypes' candidate widening (candidatesByAllMethods, an
// intersection across every one of the queried interface's method names)
// under many decoy candidates sharing one of those names: a single-method
// interface named Decoy0..DecoyN-1, each declaring only Close, alongside
// the real Multi (A + Close) and its real implementer. Since the
// intersection is bounded by the SMALLEST per-name candidate set rather
// than widened by the most common name, adding decoys under "Close" must
// not change Multi's own Implementation result -- the decoys are missing
// "A" and fall out of the intersection before any export data is decoded
// for them, unlike a union-based first pass would.
func TestImplementation_ManyUnrelatedCandidatesStaysSound(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/manydecoys\n\ngo 1.23\n")
	writeTestFile(t, dir, "multi/multi.go", `package multi

type Multi interface {
	A() string
	Close() error
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Real struct{}

func (r Real) A() string     { return "a" }
func (r Real) Close() error  { return nil }
`)
	const decoys = 30
	for i := 0; i < decoys; i++ {
		writeTestFile(t, dir, fmt.Sprintf("decoy%d/decoy.go", i), fmt.Sprintf(`package decoy%d

// Decoy declares only Close, sharing Multi's most common method name
// without satisfying Multi itself.
type Decoy struct{}

func (d Decoy) Close() error { return nil }
`, i))
	}

	r, snap := newResolverForDir(t, dir)
	multiFile := goFile(t, snap, "example.com/manydecoys/multi", "multi.go")
	implFile := goFile(t, snap, "example.com/manydecoys/impl", "impl.go")

	line, col := identOccurrence(t, multiFile, "Multi")
	locs, err := r.Implementation(context.Background(), multiFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Multi): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Multi) = %+v, want exactly 1 result (Real) despite %d Close-only decoys", locs, decoys)
	}
	wantLoc(t, locs, implFile, "Real")
}

// TestImplementation_MethodQueryDedupesCoincidentalDoubleImplementer covers
// a case surfaced while testing the promoted-method fix above: for a
// single-method interface, the embedded helper type can independently
// satisfy the interface on its own, in addition to the type that embeds
// it -- both are genuinely distinct implementers, so the interface's own
// name must list both declarations. But a method-granular query resolves
// both to the exact same underlying Func (Wrapper's Close IS Base's Close,
// via promotion), so it must report that single declaration once, not
// twice, unlike the type-level query.
func TestImplementation_MethodQueryDedupesCoincidentalDoubleImplementer(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/doubleimpl\n\ngo 1.23\n")
	writeTestFile(t, dir, "base/base.go", `package base

// Base satisfies Closer entirely on its own.
type Base struct{}

func (b Base) Close() error { return nil }
`)
	writeTestFile(t, dir, "closer/closer.go", `package closer

type Closer interface {
	Close() error
}
`)
	writeTestFile(t, dir, "wrapper/wrapper.go", `package wrapper

import "example.com/doubleimpl/base"

// Wrapper also satisfies Closer, but only via embedding base.Base.
type Wrapper struct {
	base.Base
}
`)

	r, snap := newResolverForDir(t, dir)
	closerFile := goFile(t, snap, "example.com/doubleimpl/closer", "closer.go")
	baseFile := goFile(t, snap, "example.com/doubleimpl/base", "base.go")
	wrapperFile := goFile(t, snap, "example.com/doubleimpl/wrapper", "wrapper.go")

	line, col := identOccurrence(t, closerFile, "Closer")
	locs, err := r.Implementation(context.Background(), closerFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Closer): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("Implementation(Closer) = %+v, want 2 results (Base and Wrapper are both genuine implementers)", locs)
	}
	wantLoc(t, locs, baseFile, "Base")
	wantLoc(t, locs, wrapperFile, "Wrapper")

	line, col = identOccurrence(t, closerFile, "Close")
	locs, err = r.Implementation(context.Background(), closerFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Closer.Close): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Closer.Close) = %+v, want exactly 1 result: Base and Wrapper's Close resolve to the same declaration", locs)
	}
	wantLoc(t, locs, baseFile, "Close")
}
