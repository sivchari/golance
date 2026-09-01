package xref

import (
	"context"
	"testing"
)

// wantRefLoc asserts locs contains a reference at the occurrence-th
// (0-based) position of the "Say" identifier in file.
func wantRefLoc(t *testing.T, locs []Location, file string, occurrence int) {
	t.Helper()
	const name = "Say"
	positions := identOccurrences(t, file, name)
	if len(positions) <= occurrence {
		t.Fatalf("%s: found %d occurrences of %q, want at least %d", file, len(positions), name, occurrence+1)
	}
	want := positions[occurrence]
	for _, l := range locs {
		if l.File == file && int(l.Line) == want.Line && int(l.Col) == want.Column {
			return
		}
	}
	t.Errorf("locations %+v missing %s occurrence %d at %s:%d:%d", locs, name, occurrence, file, want.Line, want.Column)
}

// TestReferences_InterfaceMethodIncludesDirectConcreteCallSite covers
// References invoked on an interface method's own declaration: before the
// fix, locationsFor matched only target's exact SymbolID, so a call through
// a concretely-typed value -- recorded against the CONCRETE method's own
// SymbolID, not the interface method's -- never showed up, even though it
// is exactly the kind of call site gopls's own References treats as a
// reference to the interface method.
func TestReferences_InterfaceMethodIncludesDirectConcreteCallSite(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refcorr\n\ngo 1.23\n")
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
	"example.com/refcorr/iface"
	"example.com/refcorr/impl"
)

func CallInterface(t iface.Talker) string {
	return t.Say()
}

func CallConcrete() string {
	return impl.Concrete{}.Say()
}
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/refcorr/iface", "iface.go")
	useFile := goFile(t, snap, "example.com/refcorr/use", "use.go")

	line, col := identOccurrence(t, ifaceFile, "Say")
	locs, err := r.References(context.Background(), ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References(Talker.Say): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("References(Talker.Say) = %+v, want 2 (interface-typed call + concrete-typed call)", locs)
	}
	wantRefLoc(t, locs, useFile, 0) // first occurrence: the interface-typed call
	wantRefLoc(t, locs, useFile, 1) // second occurrence: the concrete-typed call
}

// TestReferences_InterfaceMethodIncludesPromotedConcreteCallSite is
// TestReferences_InterfaceMethodIncludesDirectConcreteCallSite's
// counterpart when the implementer only has the method via struct
// embedding. A call through the embedding type's value is itself recorded
// (by addRefs, via info.Selections) against the EMBEDDED field's own
// method, so this exercises correspondingMethodSymbols'/
// concreteMethodSymbol's promoted-package resolution (see
// implementation.go's methodFuncSymbol) rather than locationsFor itself.
func TestReferences_InterfaceMethodIncludesPromotedConcreteCallSite(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refcorrembed\n\ngo 1.23\n")
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

import "example.com/refcorrembed/base"

type Concrete struct {
	base.Base
}
`)
	writeTestFile(t, dir, "use/use.go", `package use

import (
	"example.com/refcorrembed/iface"
	"example.com/refcorrembed/impl"
)

func CallInterface(t iface.Talker) string {
	return t.Say()
}

func CallConcrete() string {
	return impl.Concrete{}.Say()
}
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/refcorrembed/iface", "iface.go")
	useFile := goFile(t, snap, "example.com/refcorrembed/use", "use.go")

	line, col := identOccurrence(t, ifaceFile, "Say")
	locs, err := r.References(context.Background(), ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References(Talker.Say): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("References(Talker.Say) = %+v, want 2 (interface-typed call + promoted concrete-typed call)", locs)
	}
	wantRefLoc(t, locs, useFile, 0) // first occurrence: the interface-typed call
	wantRefLoc(t, locs, useFile, 1) // second occurrence: the promoted concrete-typed call
}

// TestReferences_NonMethodSymbolUnaffected pins that the interface <->
// concrete correspondence lookup only activates for index.KindMethod
// targets: References on an ordinary function must behave exactly as
// before (no correspondingMethodSymbols call, no risk of it misfiring).
func TestReferences_NonMethodSymbolUnaffected(t *testing.T) {
	r, snap := newTestResolver(t)
	implFile := goFile(t, snap, pkgImpl, "impl.go")

	line, col := identOccurrence(t, implFile, "NewPerson")
	locs, err := r.References(context.Background(), implFile, line, col, false)
	if err != nil {
		t.Fatalf("References(NewPerson): %v", err)
	}
	if len(locs) == 0 {
		t.Fatalf("References(NewPerson) = %+v, want at least the known call sites in user/user2", locs)
	}
}
