package xref

import (
	"context"
	"path/filepath"
	"testing"
)

// TestImplementation_ConcreteTypeSatisfiesUnexportedInterface pins F6:
// Implementation queried on an EXPORTED concrete type's own name, where it
// implements an UNEXPORTED interface, used to silently drop that interface
// from the result -- implementedInterfacesConfirm decoded every candidate
// interface via resolveNamed to confirm it with types.Implements, which
// always fails for an unexported package-scope type, dropping it via a bare
// `continue` no matter how genuinely the concrete type implemented it.
func TestImplementation_ConcreteTypeSatisfiesUnexportedInterface(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/concretesatisfiesunexported\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

// fetcher is unexported and never referenced by any exported signature: its
// own package's export data never carries it at all.
type fetcher interface {
	BatchGet() []int
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

// Repo is exported: its own export data decodes cleanly.
type Repo struct{}

func (r Repo) BatchGet() []int { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/concretesatisfiesunexported/iface", "iface.go")
	implFile := goFile(t, snap, "example.com/concretesatisfiesunexported/impl", "impl.go")

	line, col := identOccurrence(t, implFile, "Repo")
	locs, err := r.Implementation(context.Background(), implFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Repo): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Repo) = %+v, want exactly 1 result (fetcher)", locs)
	}
	wantLoc(t, locs, ifaceFile, "fetcher")
}

// TestImplementation_ConcreteMethodSatisfiesUnexportedInterfaceMethod is
// TestImplementation_ConcreteTypeSatisfiesUnexportedInterface's
// method-granular counterpart: querying the concrete type's own method must
// resolve to the unexported interface's matching method.
func TestImplementation_ConcreteMethodSatisfiesUnexportedInterfaceMethod(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/concretemethodsatisfiesunexported\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type fetcher interface {
	BatchGet() []int
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Repo struct{}

func (r Repo) BatchGet() []int { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/concretemethodsatisfiesunexported/iface", "iface.go")
	implFile := goFile(t, snap, "example.com/concretemethodsatisfiesunexported/impl", "impl.go")

	line, col := identOccurrence(t, implFile, "BatchGet")
	locs, err := r.Implementation(context.Background(), implFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Repo.BatchGet): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Repo.BatchGet) = %+v, want exactly 1 result (fetcher.BatchGet)", locs)
	}
	wantLoc(t, locs, ifaceFile, "BatchGet")
}

// TestTypeHierarchy_SupertypesOfConcreteSatisfiesUnexportedInterface pins F7:
// confirmSupertypeCandidate has the identical drop for Supertypes' own
// decode-based path.
func TestTypeHierarchy_SupertypesOfConcreteSatisfiesUnexportedInterface(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/supertypesunexported\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type fetcher interface {
	BatchGet() []int
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Repo struct{}

func (r Repo) BatchGet() []int { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	implFile := goFile(t, snap, "example.com/supertypesunexported/impl", "impl.go")

	line, col := identOccurrence(t, implFile, "Repo")
	infos, err := r.Supertypes(context.Background(), implFile, line, col)
	if err != nil {
		t.Fatalf("Supertypes(Repo): %v", err)
	}
	assertNames(t, infos, "fetcher")
}

// TestImplementation_ConcreteTypeSatisfiesTestFileInterface covers the
// other export-data gap this fix targets: a candidate interface declared in
// an in-package "_test.go" file is present in the facts index but never in
// Export.
func TestImplementation_ConcreteTypeSatisfiesTestFileInterface(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/concretesatisfiestestfileiface\n\ngo 1.23\n")
	writeTestFile(t, dir, "mock/mock.go", `package mock

// Real exists only so the package has a non-test file to derive Export
// from.
type Real struct{}
`)
	writeTestFile(t, dir, "mock/mock_test.go", `package mock

// Fetcher is declared in a _test.go file: it never appears in Export at
// all.
type Fetcher interface {
	BatchGet() []int
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Repo struct{}

func (r Repo) BatchGet() []int { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	pkg, ok := snap.Package("example.com/concretesatisfiestestfileiface/mock")
	if !ok {
		t.Fatalf("package mock not in snapshot")
	}
	declFile := filepath.Join(pkg.Dir, "mock_test.go")
	implFile := goFile(t, snap, "example.com/concretesatisfiestestfileiface/impl", "impl.go")

	line, col := identOccurrence(t, implFile, "Repo")
	locs, err := r.Implementation(context.Background(), implFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Repo): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Repo) = %+v, want exactly 1 result (Fetcher)", locs)
	}
	wantLoc(t, locs, declFile, "Fetcher")
}
