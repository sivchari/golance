package xref

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeTypePosFixture writes a small module reproducing the field-type
// cross-reference shape these tests exercise: repo declares an interface
// (Store) with a method (Get) and a concrete implementer (Impl); usecase
// declares a struct field typed as the interface and calls the method
// through it.
func writeTypePosFixture(t *testing.T, dir, module string) {
	t.Helper()
	writeTypePosFile(t, dir, "go.mod", "module "+module+"\n\ngo 1.23\n")
	writeTypePosFile(t, dir, "repo/repo.go", `package repo

type Store interface {
	Get(id int) string
}

type Impl struct{}

func (Impl) Get(id int) string { return "" }
`)
	writeTypePosFile(t, dir, "usecase/usecase.go", `package usecase

import "`+module+`/repo"

type U struct {
	s repo.Store
}

func (u *U) Call() string {
	return u.s.Get(1)
}
`)
}

func writeTypePosFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestDefinition_StructFieldTypeReference verifies Definition at a struct
// field's interface type resolves through the facts index, and that
// References on the interface's own declaration includes that field-type
// use: internal/index/facts.go's addRefs records a ref for every
// info.Uses/info.Selections entry with no filter on the enclosing AST node
// kind, so a field's type identifier (recorded in info.Uses exactly like
// any other qualified-identifier use) is indexed the same way a function
// body's call site is.
func TestDefinition_StructFieldTypeReference(t *testing.T) {
	dir := t.TempDir()
	writeTypePosFixture(t, dir, "example.com/typepos1")

	r, snap := newResolverForDir(t, dir)
	usecaseFile := goFile(t, snap, "example.com/typepos1/usecase", "usecase.go")
	repoFile := goFile(t, snap, "example.com/typepos1/repo", "repo.go")

	fieldTypeLine, fieldTypeCol := identOccurrence(t, usecaseFile, "Store")
	locs, err := r.Definition(context.Background(), usecaseFile, fieldTypeLine, fieldTypeCol)
	if err != nil {
		t.Fatalf("Definition(field type Store): %v", err)
	}
	if len(locs) != 1 || locs[0].File != repoFile {
		t.Fatalf("Definition(field type Store) = %+v, want repo.go", locs)
	}

	declLine, declCol := identOccurrence(t, repoFile, "Store")
	refs, err := r.References(context.Background(), repoFile, declLine, declCol, true)
	if err != nil {
		t.Fatalf("References(Store): %v", err)
	}
	found := false
	for _, l := range refs {
		if l.File == usecaseFile && l.Line == uint32(fieldTypeLine) {
			found = true
		}
	}
	if !found {
		t.Errorf("References(Store) = %+v, missing field-type use at %s:%d", refs, usecaseFile, fieldTypeLine)
	}
}

// TestReferences_InterfaceMethodCallSiteThroughField verifies References on
// an interface method's own declaration includes a call site reached
// through a struct field of that interface type (u.s.Get(1)).
func TestReferences_InterfaceMethodCallSiteThroughField(t *testing.T) {
	dir := t.TempDir()
	writeTypePosFixture(t, dir, "example.com/typepos2")

	r, snap := newResolverForDir(t, dir)
	usecaseFile := goFile(t, snap, "example.com/typepos2/usecase", "usecase.go")
	repoFile := goFile(t, snap, "example.com/typepos2/repo", "repo.go")

	getLine, getCol := identOccurrence(t, repoFile, "Get")
	refs, err := r.References(context.Background(), repoFile, getLine, getCol, false)
	if err != nil {
		t.Fatalf("References(Store.Get): %v", err)
	}

	callOcc := identOccurrences(t, usecaseFile, "Get")[0]
	found := false
	for _, l := range refs {
		if l.File == usecaseFile && int(l.Line) == callOcc.Line && int(l.Col) == callOcc.Column {
			found = true
		}
	}
	if !found {
		t.Errorf("References(Store.Get) = %+v, missing call site at %s:%d:%d", refs, usecaseFile, callOcc.Line, callOcc.Column)
	}
}

// TestDefinition_FieldTypeSameNameAsEnclosingType covers the exact shape a
// production report traced golance's "no symbol at this position" miss to:
// the struct being declared shares its base name with the field type it
// references in another package (usecase.Contract has a field typed
// repository.Contract). A name collision like this is exactly the kind of
// thing a position- or name-based lookup could get wrong; Definition here
// is keyed on the ref's exact byte-column span, not on the name, so it is
// unaffected.
func TestDefinition_FieldTypeSameNameAsEnclosingType(t *testing.T) {
	dir := t.TempDir()
	writeTypePosFile(t, dir, "go.mod", "module example.com/typepos3\n\ngo 1.23\n")
	writeTypePosFile(t, dir, "repository/repository.go", `package repository

type Contract struct {
	ID string
}
`)
	writeTypePosFile(t, dir, "usecase/usecase.go", `package usecase

import "example.com/typepos3/repository"

type Contract struct {
	contractRepo repository.Contract
}
`)

	r, snap := newResolverForDir(t, dir)
	usecaseFile := goFile(t, snap, "example.com/typepos3/usecase", "usecase.go")
	repoFile := goFile(t, snap, "example.com/typepos3/repository", "repository.go")

	occs := identOccurrences(t, usecaseFile, "Contract")
	if len(occs) < 2 {
		t.Fatalf("want at least 2 occurrences of Contract in usecase.go, got %d", len(occs))
	}
	fieldTypeOcc := occs[1] // occs[0] is the enclosing struct's own name.

	locs, err := r.Definition(context.Background(), usecaseFile, fieldTypeOcc.Line, fieldTypeOcc.Column)
	if err != nil {
		t.Fatalf("Definition(field type Contract) at %d:%d: %v", fieldTypeOcc.Line, fieldTypeOcc.Column, err)
	}
	if len(locs) != 1 || locs[0].File != repoFile {
		t.Fatalf("Definition(field type Contract) = %+v, want repository.go", locs)
	}
}

// TestDefinition_VariousTypePositions covers Definition at a pointer field,
// an embedded (anonymous) field, a function's parameter and result types, an
// interface method's parameter and result types, and a generic
// instantiation's own type argument -- every construct the facts index's
// addRefs walks via info.Uses (a plain identifier use, regardless of AST
// context) rather than any node-kind-specific traversal.
func TestDefinition_VariousTypePositions(t *testing.T) {
	dir := t.TempDir()
	writeTypePosFile(t, dir, "go.mod", "module example.com/typepos4\n\ngo 1.23\n")
	writeTypePosFile(t, dir, "repo/repo.go", `package repo

type Store interface {
	Get(id int) string
}

type Impl struct {
	Field int
}

func (Impl) Get(id int) string { return "" }

type Box[T any] struct {
	V T
}
`)
	writeTypePosFile(t, dir, "usecase/usecase.go", `package usecase

import "example.com/typepos4/repo"

type PtrField struct {
	s *repo.Store
}

type Embed struct {
	repo.Impl
}

func TakesParam(s repo.Store) repo.Store {
	return s
}

type IfaceParam interface {
	Do(s repo.Store) repo.Store
}

type GenericField struct {
	B repo.Box[int]
}
`)

	r, snap := newResolverForDir(t, dir)
	usecaseFile := goFile(t, snap, "example.com/typepos4/usecase", "usecase.go")

	for _, name := range []string{"Store", "Impl", "Box"} {
		for _, occ := range identOccurrences(t, usecaseFile, name) {
			locs, err := r.Definition(context.Background(), usecaseFile, occ.Line, occ.Column)
			if err != nil {
				t.Errorf("Definition(%s at %d:%d): %v", name, occ.Line, occ.Column, err)
				continue
			}
			if len(locs) == 0 {
				t.Errorf("Definition(%s at %d:%d): empty result", name, occ.Line, occ.Column)
			}
		}
	}
}
