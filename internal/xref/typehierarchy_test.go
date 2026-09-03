package xref

import (
	"context"
	"sort"
	"testing"
)

// typeHierarchyNames returns infos' own Name fields, sorted, for assertions
// that only care which types were found, not their locations.
func typeHierarchyNames(infos []TypeHierarchyItemInfo) []string {
	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	sort.Strings(names)
	return names
}

func assertNames(t *testing.T, got []TypeHierarchyItemInfo, want ...string) {
	t.Helper()
	gotNames := typeHierarchyNames(got)
	sort.Strings(want)
	if len(gotNames) != len(want) {
		t.Fatalf("got %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("got %v, want %v", gotNames, want)
		}
	}
}

// writeTypeHierarchyFixture builds the two-package fixture gopls's own
// TypeHierarchy marker test (gopls/internal/test/marker/testdata/
// typehierarchy/basic.txt) poses its Supertypes/Subtypes queries against:
// package a declares I (one method F), J (embeds I's method plus G), and
// concrete S (implements both); package b declares the structurally
// identical BI/BJ/BS, so a query must also cross package boundaries.
func writeTypeHierarchyFixture(t *testing.T, dir string) {
	t.Helper()
	writeTestFile(t, dir, "go.mod", "module example.com/typehier\n\ngo 1.23\n")
	writeTestFile(t, dir, "a/a.go", `package a

type I interface { F() }

type J interface { F(); G() }

type S int

func (S) F() {}
func (S) G() {}
`)
	writeTestFile(t, dir, "b/b.go", `package b

type BI interface { F() }

type BJ interface { F(); G() }

type BS int

func (BS) F() {}
func (BS) G() {}
`)
}

func TestTypeHierarchy_SubtypesMatchesGoplsMarkerFixture(t *testing.T) {
	dir := t.TempDir()
	writeTypeHierarchyFixture(t, dir)
	r, snap := newResolverForDir(t, dir)
	aFile := goFile(t, snap, "example.com/typehier/a", "a.go")

	tests := []struct {
		name string
		want []string
	}{
		{"S", nil},
		{"I", []string{"J", "S", "BI", "BJ", "BS"}},
		{"J", []string{"S", "BJ", "BS"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, col := identOccurrence(t, aFile, tt.name)
			got, err := r.Subtypes(context.Background(), aFile, line, col)
			if err != nil {
				t.Fatalf("Subtypes(%s): %v", tt.name, err)
			}
			assertNames(t, got, tt.want...)
		})
	}
}

func TestTypeHierarchy_SupertypesMatchesGoplsMarkerFixture(t *testing.T) {
	dir := t.TempDir()
	writeTypeHierarchyFixture(t, dir)
	r, snap := newResolverForDir(t, dir)
	aFile := goFile(t, snap, "example.com/typehier/a", "a.go")

	tests := []struct {
		name string
		want []string
	}{
		{"S", []string{"I", "J", "BI", "BJ"}},
		{"I", []string{"BI"}},
		{"J", []string{"I", "BI", "BJ"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, col := identOccurrence(t, aFile, tt.name)
			got, err := r.Supertypes(context.Background(), aFile, line, col)
			if err != nil {
				t.Fatalf("Supertypes(%s): %v", tt.name, err)
			}
			assertNames(t, got, tt.want...)
		})
	}
}

// TestTypeHierarchy_SubtypesIncludesInterfaceKindAndLocation pins that a
// Subtypes result reports IsInterface correctly for both an interface
// subtype (J, satisfying I by embedding-equivalent method superset) and a
// concrete one (S), and that Location resolves to the candidate's own
// declaring identifier.
func TestTypeHierarchy_SubtypesIncludesInterfaceKindAndLocation(t *testing.T) {
	dir := t.TempDir()
	writeTypeHierarchyFixture(t, dir)
	r, snap := newResolverForDir(t, dir)
	aFile := goFile(t, snap, "example.com/typehier/a", "a.go")

	line, col := identOccurrence(t, aFile, "I")
	got, err := r.Subtypes(context.Background(), aFile, line, col)
	if err != nil {
		t.Fatalf("Subtypes(I): %v", err)
	}
	byName := make(map[string]TypeHierarchyItemInfo, len(got))
	for _, info := range got {
		byName[info.Name] = info
	}
	j, ok := byName["J"]
	if !ok || !j.IsInterface {
		t.Fatalf("J: got %+v, want IsInterface=true", j)
	}
	s, ok := byName["S"]
	if !ok || s.IsInterface {
		t.Fatalf("S: got %+v, want IsInterface=false", s)
	}
	if s.Location.File != aFile {
		t.Fatalf("S.Location.File = %s, want %s", s.Location.File, aFile)
	}
	wantLine, _ := identOccurrence(t, aFile, "S")
	if int(s.Location.Line) != wantLine {
		t.Fatalf("S.Location.Line = %d, want %d", s.Location.Line, wantLine)
	}
}

// TestTypeHierarchy_EmptyInterfaceReturnsNoResults mirrors
// TestImplementation_EmptyInterfaceReturnsNoResults: neither direction
// should enumerate the whole workspace against interface{}/any.
func TestTypeHierarchy_EmptyInterfaceReturnsNoResults(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/emptyiface\n\ngo 1.23\n")
	writeTestFile(t, dir, "empty/empty.go", `package empty

type Empty interface{}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type S struct{}
`)
	r, snap := newResolverForDir(t, dir)
	emptyFile := goFile(t, snap, "example.com/emptyiface/empty", "empty.go")

	line, col := identOccurrence(t, emptyFile, "Empty")
	subs, err := r.Subtypes(context.Background(), emptyFile, line, col)
	if err != nil {
		t.Fatalf("Subtypes(Empty): %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("Subtypes(Empty) = %+v, want none", subs)
	}
	sups, err := r.Supertypes(context.Background(), emptyFile, line, col)
	if err != nil {
		t.Fatalf("Supertypes(Empty): %v", err)
	}
	if len(sups) != 0 {
		t.Fatalf("Supertypes(Empty) = %+v, want none", sups)
	}
}

// TestTypeHierarchy_ConcreteTypeHasNoSubtypes covers Subtypes on a
// zero-method-set concrete type too (not just S, which has methods): Go has
// no subclassing, so this must be nil, not an error, regardless of the
// concrete type's own method set shape.
func TestTypeHierarchy_ConcreteTypeHasNoSubtypes(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/nosubtypes\n\ngo 1.23\n")
	writeTestFile(t, dir, "p/p.go", `package p

type S struct{}

func (S) F() {}
`)
	r, snap := newResolverForDir(t, dir)
	pFile := goFile(t, snap, "example.com/nosubtypes/p", "p.go")

	line, col := identOccurrence(t, pFile, "S")
	got, err := r.Subtypes(context.Background(), pFile, line, col)
	if err != nil {
		t.Fatalf("Subtypes(S): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Subtypes(S) = %+v, want none", got)
	}
}

// TestTypeHierarchy_UnexportedImplementerFound covers the same
// unexported-implementer case implementation_unexported_test.go pins for
// "Go to Implementations": Subtypes must find an unexported implementer
// via the fingerprint fast path, not just a decoded, exported one.
func TestTypeHierarchy_UnexportedImplementerFound(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/unexported\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Greeter interface { Greet() string }
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type unexportedGreeter struct{}

func (unexportedGreeter) Greet() string { return "hi" }
`)
	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/unexported/iface", "iface.go")

	line, col := identOccurrence(t, ifaceFile, "Greeter")
	got, err := r.Subtypes(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Subtypes(Greeter): %v", err)
	}
	assertNames(t, got, "unexportedGreeter")
}

// TestTypeHierarchy_PromotedStructMethodSatisfiesInterface covers Subtypes
// finding an implementer that only satisfies the interface via struct
// embedding (promoted methods), mirroring
// TestImplementation_PromotedStructMethod's identical fixture shape for
// "Go to Implementations": Multi requires both A and Close; Base alone only
// has Close (so Base itself does not implement Multi), and Wrapper
// satisfies Multi by declaring A directly plus getting Close promoted from
// embedding base.Base.
func TestTypeHierarchy_PromotedStructMethodSatisfiesInterface(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/embedpromoted\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Multi interface {
	A() string
	Close() error
}
`)
	writeTestFile(t, dir, "base/base.go", `package base

// Base declares Close only; it does not implement iface.Multi on its own.
type Base struct{}

func (b Base) Close() error { return nil }
`)
	writeTestFile(t, dir, "wrapper/wrapper.go", `package wrapper

import "example.com/embedpromoted/base"

// Wrapper implements Multi: A directly, Close via embedding base.Base.
type Wrapper struct {
	base.Base
}

func (w Wrapper) A() string { return "a" }
`)
	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/embedpromoted/iface", "iface.go")

	line, col := identOccurrence(t, ifaceFile, "Multi")
	got, err := r.Subtypes(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Subtypes(Multi): %v", err)
	}
	assertNames(t, got, "Wrapper")
}

// TestTypeHierarchy_GenericInterfaceDoesNotPanic documents golance's
// current behavior for a generic (uninstantiated) interface: like gopls's
// own TypeHierarchy ("TODO(adonovan): Support type hierarchy by
// signatures"), generics are not the primary supported case. Fingerprint
// confirmation is skipped for a generic query type (see Subtypes'/
// Supertypes' generic guard, mirroring implementingTypes' identical
// exclusion), falling back to a live types.Implements decode -- this pins
// that the fallback runs cleanly (no panic, no error) even though it may
// not find every instantiation-specific relationship.
func TestTypeHierarchy_GenericInterfaceDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/generichier\n\ngo 1.23\n")
	writeTestFile(t, dir, "p/p.go", `package p

type Container[T any] interface {
	Get() T
}

type Box[T any] struct{ v T }

func (b Box[T]) Get() T { return b.v }
`)
	r, snap := newResolverForDir(t, dir)
	pFile := goFile(t, snap, "example.com/generichier/p", "p.go")

	line, col := identOccurrence(t, pFile, "Container")
	if _, err := r.Subtypes(context.Background(), pFile, line, col); err != nil {
		t.Fatalf("Subtypes(Container): %v", err)
	}
	if _, err := r.Supertypes(context.Background(), pFile, line, col); err != nil {
		t.Fatalf("Supertypes(Container): %v", err)
	}
}

// TestTypeHierarchy_MethodPositionUnsupported covers preparing on a method
// name: gopls's own PrepareTypeHierarchy documents this as out of scope
// ("Allow methods too?" TODO in type_hierarchy.go), and golance's
// implementation likewise only supports index.KindType/KindInterface
// targets -- Supertypes/Subtypes on a method position must fail, not panic
// or silently return an empty result mistaken for "genuinely no relatives".
func TestTypeHierarchy_MethodPositionUnsupported(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/methodpos\n\ngo 1.23\n")
	writeTestFile(t, dir, "p/p.go", `package p

type S struct{}

func (S) F() {}
`)
	r, snap := newResolverForDir(t, dir)
	pFile := goFile(t, snap, "example.com/methodpos/p", "p.go")

	line, col := identOccurrence(t, pFile, "F")
	if _, err := r.Supertypes(context.Background(), pFile, line, col); err == nil {
		t.Fatal("Supertypes(F) = nil error, want an error for a method position")
	}
	if _, err := r.Subtypes(context.Background(), pFile, line, col); err == nil {
		t.Fatal("Subtypes(F) = nil error, want an error for a method position")
	}
}
