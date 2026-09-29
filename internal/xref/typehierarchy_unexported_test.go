package xref

import (
	"context"
	"path/filepath"
	"testing"
)

// TestTypeHierarchy_SupertypesOfUnexportedConcreteTarget pins F4: Supertypes
// queried on an unexported concrete type's own declaration used to fail
// outright, because typeHierarchyTarget decoded the target via resolveNamed
// -- export data never carries an unexported package-scope type at all.
func TestTypeHierarchy_SupertypesOfUnexportedConcreteTarget(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/typehierunexported\n\ngo 1.23\n")
	writeTestFile(t, dir, "dber/dber.go", `package dber

type DBer interface {
	NewDB() error
}
`)
	writeTestFile(t, dir, "dber/db.go", `package dber

// db is unexported: its own package's export data never carries it at all.
type db struct{}

func (d *db) NewDB() error { return nil }

func NewDBer() DBer { return &db{} }
`)

	r, snap := newResolverForDir(t, dir)
	dbFile := goFile(t, snap, "example.com/typehierunexported/dber", "db.go")

	line, col := identOccurrence(t, dbFile, "db")
	infos, err := r.Supertypes(context.Background(), dbFile, line, col)
	if err != nil {
		t.Fatalf("Supertypes(db): %v", err)
	}
	assertNames(t, infos, "DBer")
}

// TestTypeHierarchy_SubtypesOfUnexportedInterfaceTarget is
// TestTypeHierarchy_SupertypesOfUnexportedConcreteTarget's Subtypes
// counterpart: Subtypes queried on an unexported interface's own
// declaration used to fail the same way.
func TestTypeHierarchy_SubtypesOfUnexportedInterfaceTarget(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/typehierunexportediface\n\ngo 1.23\n")
	writeTestFile(t, dir, "dber/dber.go", `package dber

// opener is unexported and never referenced by any exported signature: its
// own package's export data never carries it at all.
type opener interface {
	NewDB() error
}
`)
	writeTestFile(t, dir, "dber/db.go", `package dber

type DB struct{}

func (d *DB) NewDB() error { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	dberFile := goFile(t, snap, "example.com/typehierunexportediface/dber", "dber.go")

	line, col := identOccurrence(t, dberFile, "opener")
	infos, err := r.Subtypes(context.Background(), dberFile, line, col)
	if err != nil {
		t.Fatalf("Subtypes(opener): %v", err)
	}
	assertNames(t, infos, "DB")
}

// TestTypeHierarchy_SupertypesOfTestFileTarget covers the other export-data
// gap this fix targets: a concrete type declared in an in-package "_test.go"
// file is present in the facts index but never in Export.
func TestTypeHierarchy_SupertypesOfTestFileTarget(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/typehiertestfile\n\ngo 1.23\n")
	writeTestFile(t, dir, "mock/mock.go", `package mock

type Fetcher interface {
	BatchGet() []int
}

// Real exists only so the package has a non-test file to derive Export
// from.
type Real struct{}
`)
	writeTestFile(t, dir, "mock/mock_test.go", `package mock

// fakeThing is declared in a _test.go file: it never appears in Export at
// all.
type fakeThing struct{}

func (f fakeThing) BatchGet() []int { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	pkg, ok := snap.Package("example.com/typehiertestfile/mock")
	if !ok {
		t.Fatalf("package mock not in snapshot")
	}
	declFile := filepath.Join(pkg.Dir, "mock_test.go")

	line, col := identOccurrence(t, declFile, "fakeThing")
	infos, err := r.Supertypes(context.Background(), declFile, line, col)
	if err != nil {
		t.Fatalf("Supertypes(fakeThing): %v", err)
	}
	assertNames(t, infos, "Fetcher")
}
