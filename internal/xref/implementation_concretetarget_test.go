package xref

import (
	"context"
	"path/filepath"
	"testing"
)

// TestImplementation_ConcreteUnexportedTarget_MethodName pins F1: querying
// Implementation FROM an unexported concrete method's own declaration (not
// the interface's) used to hard-fail outright, because
// implementationOfMethod's methodReceiver decodes the method's own defining
// package's export data and looks up its objectpath there -- export data
// never carries an unexported package-scope type at all, so that lookup
// always errored and the whole query failed instead of finding the
// interface it implements.
func TestImplementation_ConcreteUnexportedTarget_MethodName(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/concretetarget\n\ngo 1.23\n")
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
	dberFile := goFile(t, snap, "example.com/concretetarget/dber", "dber.go")
	dbFile := goFile(t, snap, "example.com/concretetarget/dber", "db.go")

	line, col := identOccurrence(t, dbFile, "NewDB")
	locs, err := r.Implementation(context.Background(), dbFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(db.NewDB): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(db.NewDB) = %+v, want exactly 1 result (DBer.NewDB)", locs)
	}
	wantLoc(t, locs, dberFile, "NewDB")
}

// TestImplementation_ConcreteUnexportedTarget_TypeName is
// TestImplementation_ConcreteUnexportedTarget_MethodName's type-name
// counterpart: querying Implementation from the unexported concrete type's
// own name, rather than one of its methods, used to hard-fail the same way
// via resolveNamed instead of resolveMethodFunc.
func TestImplementation_ConcreteUnexportedTarget_TypeName(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/concretetargetname\n\ngo 1.23\n")
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
	dberFile := goFile(t, snap, "example.com/concretetargetname/dber", "dber.go")
	dbFile := goFile(t, snap, "example.com/concretetargetname/dber", "db.go")

	line, col := identOccurrence(t, dbFile, "db")
	locs, err := r.Implementation(context.Background(), dbFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(db): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(db) = %+v, want exactly 1 result (DBer)", locs)
	}
	wantLoc(t, locs, dberFile, "DBer")
}

// TestImplementation_ConcreteTestFileTarget covers the other export-data gap
// this fix targets: a concrete type declared in an in-package "_test.go"
// file (the mock pattern) is present in the facts index (the test-inclusive
// facts pass -- see internal/index's checkOnePackage) but never in Export,
// which is always built from non-test files alone. Both a method-name query
// and a type-name query rooted at the _test.go declaration must still find
// the interface it implements.
func TestImplementation_ConcreteTestFileTarget(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/testfiletarget\n\ngo 1.23\n")
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

func newFake() Fetcher { return fakeThing{} }
`)

	r, snap := newResolverForDir(t, dir)
	pkg, ok := snap.Package("example.com/testfiletarget/mock")
	if !ok {
		t.Fatalf("package mock not in snapshot")
	}
	mockFile := filepath.Join(pkg.Dir, "mock.go")
	declFile := filepath.Join(pkg.Dir, "mock_test.go")

	line, col := identOccurrence(t, declFile, "BatchGet")
	locs, err := r.Implementation(context.Background(), declFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(fakeThing.BatchGet): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(fakeThing.BatchGet) = %+v, want exactly 1 result (Fetcher.BatchGet)", locs)
	}
	wantLoc(t, locs, mockFile, "BatchGet")

	line, col = identOccurrence(t, declFile, "fakeThing")
	locs, err = r.Implementation(context.Background(), declFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(fakeThing): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(fakeThing) = %+v, want exactly 1 result (Fetcher)", locs)
	}
	wantLoc(t, locs, mockFile, "Fetcher")
}
