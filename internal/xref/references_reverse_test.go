package xref

import (
	"context"
	"testing"
)

// TestReferences_ConcreteMethodIncludesInterfaceTypedCallSite is
// TestReferences_InterfaceMethodIncludesDirectConcreteCallSite's mirror in
// the other direction: References invoked on a CONCRETE method's own
// declaration must also include call sites reached through an
// interface-typed value (recorded, by info.Selections, against the
// interface method's own SymbolID -- not the concrete method's), exercising
// interfacesSatisfiedByMethod for the first time.
func TestReferences_ConcreteMethodIncludesInterfaceTypedCallSite(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refrev\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Talker interface {
	Say() string
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Concrete struct{}

func (c Concrete) Say() string { return "hi" }
`)
	writeTestFile(t, dir, "use/use.go", `package use

import (
	"example.com/refrev/iface"
	"example.com/refrev/impl"
)

func CallInterface(t iface.Talker) string {
	return t.Say()
}

func CallConcrete() string {
	return impl.Concrete{}.Say()
}
`)

	r, snap := newResolverForDir(t, dir)
	implFile := goFile(t, snap, "example.com/refrev/impl", "impl.go")
	useFile := goFile(t, snap, "example.com/refrev/use", "use.go")

	line, col := identOccurrence(t, implFile, "Say")
	locs, err := r.References(context.Background(), implFile, line, col, false)
	if err != nil {
		t.Fatalf("References(Concrete.Say): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("References(Concrete.Say) = %+v, want 2 (interface-typed call + concrete-typed call)", locs)
	}
	wantRefLoc(t, locs, useFile, 0) // first occurrence: the interface-typed call
	wantRefLoc(t, locs, useFile, 1) // second occurrence: the concrete-typed call
}

// TestReferences_ConcreteMethodIncludesPromotedInterfaceCallSite combines
// two mechanisms this package already tests independently: the promoted-
// method resolution TestReferences_InterfaceMethodIncludesPromotedConcreteCallSite
// pins (a call through the EMBEDDING type resolves, via info.Selections, to
// the EMBEDDED type's own method) and interfacesSatisfiedByMethod's new
// reverse direction. Querying References on base.Base's own Say
// declaration -- the only source position "Say" is actually declared at,
// since Concrete never redeclares it -- must find both: Concrete's promoted
// call site (already reachable via locationsFor's ordinary exact-SymbolID
// match, since that ref is recorded against base.Base's Say) and the
// interface-typed call site (only reachable via interfacesSatisfiedByMethod,
// since base.Base itself -- not Concrete -- is what actually satisfies
// Talker here).
func TestReferences_ConcreteMethodIncludesPromotedInterfaceCallSite(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refrevembed\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Talker interface {
	Say() string
}
`)
	writeTestFile(t, dir, "base/base.go", `package base

type Base struct{}

func (b Base) Say() string { return "hi" }
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

import "example.com/refrevembed/base"

type Concrete struct {
	base.Base
}
`)
	writeTestFile(t, dir, "use/use.go", `package use

import (
	"example.com/refrevembed/iface"
	"example.com/refrevembed/impl"
)

func CallInterface(t iface.Talker) string {
	return t.Say()
}

func CallConcrete() string {
	return impl.Concrete{}.Say()
}
`)

	r, snap := newResolverForDir(t, dir)
	baseFile := goFile(t, snap, "example.com/refrevembed/base", "base.go")
	useFile := goFile(t, snap, "example.com/refrevembed/use", "use.go")

	line, col := identOccurrence(t, baseFile, "Say")
	locs, err := r.References(context.Background(), baseFile, line, col, false)
	if err != nil {
		t.Fatalf("References(Base.Say): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("References(Base.Say) = %+v, want 2 (interface-typed call + promoted concrete-typed call)", locs)
	}
	wantRefLoc(t, locs, useFile, 0) // first occurrence: the interface-typed call
	wantRefLoc(t, locs, useFile, 1) // second occurrence: the promoted concrete-typed call
}

// TestReferences_ConcreteMethodExcludesUnsatisfiedSameNameInterface is the
// precision counterpart of the two tests above: Loud shares Talker's method
// NAME (Say) but not its signature, so Concrete -- which only ever declares
// a parameterless Say -- does not satisfy Loud. interfacesSatisfiedByMethod
// must reject Loud at its types.Implements confirmation step despite Loud
// surfacing as a same-name candidate from the [store.DB.LookupMethod]
// posting list; a candidate call site through Loud must never leak into
// Concrete's own References result.
func TestReferences_ConcreteMethodExcludesUnsatisfiedSameNameInterface(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refrevprecision\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Talker interface {
	Say() string
}

// Loud shares Talker's method name but not its signature: Concrete's own
// Say (no parameters) does not satisfy this.
type Loud interface {
	Say(volume int) string
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Concrete struct{}

func (c Concrete) Say() string { return "hi" }
`)
	writeTestFile(t, dir, "blaster/blaster.go", `package blaster

type Blaster struct{}

func (b Blaster) Say(volume int) string { return "HI" }
`)
	writeTestFile(t, dir, "use/use.go", `package use

import (
	"example.com/refrevprecision/blaster"
	"example.com/refrevprecision/iface"
	"example.com/refrevprecision/impl"
)

func CallTalker(t iface.Talker) string {
	return t.Say()
}

func CallLoud(l iface.Loud) string {
	return l.Say(5)
}

func CallConcrete() string {
	return impl.Concrete{}.Say()
}

func CallBlaster() string {
	return blaster.Blaster{}.Say(5)
}
`)

	r, snap := newResolverForDir(t, dir)
	implFile := goFile(t, snap, "example.com/refrevprecision/impl", "impl.go")
	useFile := goFile(t, snap, "example.com/refrevprecision/use", "use.go")

	line, col := identOccurrence(t, implFile, "Say")
	locs, err := r.References(context.Background(), implFile, line, col, false)
	if err != nil {
		t.Fatalf("References(Concrete.Say): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("References(Concrete.Say) = %+v, want exactly 2 (Talker call + Concrete's own call), never Loud's", locs)
	}
	// The len(locs) == 2 check above already pins that Loud's call site
	// (occurrence 1) and Blaster's (occurrence 3) are both absent; these two
	// confirm which two occurrences DID survive.
	wantRefLoc(t, locs, useFile, 0) // CallTalker's t.Say()
	wantRefLoc(t, locs, useFile, 2) // CallConcrete's impl.Concrete{}.Say()
}

// TestCorrespondingMethodSymbols_GenericInterfaceCandidate is
// interfacesSatisfiedByMethod's generic-candidate counterpart to
// TestImplementation_GenericInterfaceFallsBackToDecode: Container's own
// methods are left unfingerprinted (registerInterfaceMethodSet's generic
// exclusion), so confirming IntBox satisfies it falls back to decoding
// Container's export data and calling types.Implements, exactly as the
// Implementation-query direction already does -- pinning that References'
// new reverse direction shares that same fallback rather than needing (or
// silently skipping) its own generic handling.
//
// This drives correspondingMethodSymbols directly (rather than through a
// call site via References) deliberately: matching a REFERENCE recorded at
// a call site through an INSTANTIATED generic interface value (e.g.
// container.Container[int]) back to the interface's own uninstantiated
// method declaration is a separate, pre-existing gap in how facts
// extraction records such a selection's SymbolID (info.Selections'
// substituted Func there is not the same object -- nor objectpath-
// encodable to the same string -- as [types.Interface.Method] returns for
// the plain, uninstantiated interface), unrelated to and unchanged by this
// fix; declaration-to-declaration resolution (what correspondingMethodSymbols
// itself does, and what this test isolates) is unaffected by it.
func TestCorrespondingMethodSymbols_GenericInterfaceCandidate(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refrevgeneric\n\ngo 1.23\n")
	writeTestFile(t, dir, "container/container.go", `package container

type Container[T any] interface {
	Reset()
}
`)
	writeTestFile(t, dir, "box/box.go", `package box

type IntBox struct {
	V int
}

func (b IntBox) Reset() { b.V = 0 }
`)

	r, snap := newResolverForDir(t, dir)
	boxFile := goFile(t, snap, "example.com/refrevgeneric/box", "box.go")
	containerFile := goFile(t, snap, "example.com/refrevgeneric/container", "container.go")

	line, col := identOccurrence(t, boxFile, "Reset")
	l, c, err := toUint32Pos(line, col)
	if err != nil {
		t.Fatalf("toUint32Pos: %v", err)
	}
	target, err := r.resolveAt(context.Background(), boxFile, l, c)
	if err != nil {
		t.Fatalf("resolveAt(IntBox.Reset): %v", err)
	}

	syms, err := r.correspondingMethodSymbols(context.Background(), target)
	if err != nil {
		t.Fatalf("correspondingMethodSymbols(IntBox.Reset): %v", err)
	}
	if len(syms) != 1 {
		t.Fatalf("correspondingMethodSymbols(IntBox.Reset) = %+v, want exactly 1 (Container.Reset, via the decode fallback)", syms)
	}

	_, _, loc, ok := r.symbolByHash(context.Background(), syms[0].PkgHash, syms[0].IDHash)
	if !ok {
		t.Fatalf("symbolByHash(%+v) not found", syms[0])
	}
	wantLine, wantCol := identOccurrence(t, containerFile, "Reset")
	if loc.File != containerFile || int(loc.Line) != wantLine || int(loc.Col) != wantCol {
		t.Errorf("corresponding symbol location = %+v, want Container.Reset at %s:%d:%d", loc, containerFile, wantLine, wantCol)
	}
}
