package xref

import (
	"context"
	"testing"
)

// TestImplementation_UnexportedStructImplementsExportedInterface pins the
// confirmed production bug this fix targets directly: "unexported struct
// implements an exported interface, constructor returns the interface" —
// the dominant Go pattern for a package's own dependency-injected internals
// (e.g. an unexported *sql.DB wrapper satisfying an exported DBer
// interface). Export data only ever carries exported package-scope
// objects, so the pre-fix decode-then-types.Implements confirmation could
// never see db at all, no matter how genuinely it implemented DBer — see
// implementingTypes' doc for the fingerprint-based fix. Both the interface
// name and its method name are queried, matching the two cursor positions
// the real report exercised ("DBer" and "DBer.NewDB").
func TestImplementation_UnexportedStructImplementsExportedInterface(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/unexportedimpl\n\ngo 1.23\n")
	writeTestFile(t, dir, "dber/dber.go", `package dber

// DBer is the exported entry point a caller depends on; its only
// implementer is deliberately unexported (see db.go).
type DBer interface {
	NewDB() error
}
`)
	writeTestFile(t, dir, "dber/db.go", `package dber

// db is the unexported implementer: real code only ever sees it through
// the DBer interface NewDBer returns.
type db struct{}

func (d *db) NewDB() error { return nil }

// NewDBer constructs a db and returns it as DBer, the "constructor returns
// the interface" half of the pattern.
func NewDBer() DBer { return &db{} }
`)

	r, snap := newResolverForDir(t, dir)
	dberFile := goFile(t, snap, "example.com/unexportedimpl/dber", "dber.go")
	dbFile := goFile(t, snap, "example.com/unexportedimpl/dber", "db.go")

	t.Run("from_interface_name", func(t *testing.T) {
		line, col := identOccurrence(t, dberFile, "DBer")
		locs, err := r.Implementation(context.Background(), dberFile, line, col)
		if err != nil {
			t.Fatalf("Implementation(DBer): %v", err)
		}
		if len(locs) != 1 {
			t.Fatalf("Implementation(DBer) = %+v, want exactly 1 result (the unexported db type)", locs)
		}
		wantLoc(t, locs, dbFile, "db")
	})

	t.Run("from_method_name", func(t *testing.T) {
		line, col := identOccurrence(t, dberFile, "NewDB")
		locs, err := r.Implementation(context.Background(), dberFile, line, col)
		if err != nil {
			t.Fatalf("Implementation(DBer.NewDB): %v", err)
		}
		if len(locs) != 1 {
			t.Fatalf("Implementation(DBer.NewDB) = %+v, want exactly 1 result (db's own NewDB method)", locs)
		}
		wantLoc(t, locs, dbFile, "NewDB")
	})
}

// TestImplementation_UnexportedImplementerWithPromotedMethod covers the
// same unexported-implementer fix for a method the implementer gets via
// struct embedding rather than declaring directly: registerMethodSet
// indexes named's pointer method set, which already flattens a promoted
// method the same way types.NewMethodSet always has, but the promoted
// method's OWN SymbolID (methodEntrySelf's MethodPkgHash/MethodIDHash)
// must resolve to the EMBEDDED type's declaration, not the embedder's —
// see methodEntrySelf's doc and methodFuncSymbol's identical concern on the
// decode-based side this mirrors. closeHelper, which declares Close
// directly, also genuinely implements Closer entirely on its own (any type
// with a matching method does); both types are unexported, so the result
// pins that the unexported-implementer fix finds BOTH -- the direct
// declaration and the promoted one -- rather than either masking the
// other.
func TestImplementation_UnexportedImplementerWithPromotedMethod(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/unexportedpromoted\n\ngo 1.23\n")
	writeTestFile(t, dir, "closer/closer.go", `package closer

type Closer interface {
	Close() error
}
`)
	writeTestFile(t, dir, "closer/impl.go", `package closer

// closeHelper declares Close directly; conn gets it only by embedding
// closeHelper, never redeclaring it itself.
type closeHelper struct{}

func (closeHelper) Close() error { return nil }

// conn is the unexported implementer: its own scope has no Close
// declaration at all, only closeHelper's, promoted through embedding.
type conn struct {
	closeHelper
}

func NewConn() Closer { return conn{} }
`)

	r, snap := newResolverForDir(t, dir)
	closerFile := goFile(t, snap, "example.com/unexportedpromoted/closer", "closer.go")
	implFile := goFile(t, snap, "example.com/unexportedpromoted/closer", "impl.go")

	line, col := identOccurrence(t, closerFile, "Closer")
	locs, err := r.Implementation(context.Background(), closerFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Closer): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("Implementation(Closer) = %+v, want exactly 2 results (closeHelper, which implements Closer directly, and conn, via embedding it)", locs)
	}
	wantLoc(t, locs, implFile, "closeHelper")
	wantLoc(t, locs, implFile, "conn")

	// The method-name query must resolve to closeHelper's and conn's OWN
	// respective methods: conn declares no Close of its own, so its result
	// must still land on closeHelper's declaration (the promoted method's
	// true origin) -- deduplicated to one location shared by both
	// implementers (see locationsOfSymbols' doc).
	line, col = identOccurrence(t, closerFile, "Close")
	locs, err = r.Implementation(context.Background(), closerFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Closer.Close): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Closer.Close) = %+v, want exactly 1 location (closeHelper.Close, shared by both implementers)", locs)
	}
	wantLoc(t, locs, implFile, "Close")
}

// TestImplementation_ExportedImplementerStillConfirmed pins that an
// EXPORTED implementer is still found after the unexported-implementer fix:
// implementingTypes' fingerprint confirmation applies uniformly to every
// candidate regardless of whether it happens to also be decodable, so this
// is a plain regression guard that the common case (already covered
// end-to-end by xref_test.go's TestImplementation_InterfaceToImplementer
// against a different fixture) keeps working, spelled out explicitly here
// per this fix's own test plan.
func TestImplementation_ExportedImplementerStillConfirmed(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/exportedimpl\n\ngo 1.23\n")
	writeTestFile(t, dir, "greeter/greeter.go", `package greeter

type Greeter interface {
	Greet() string
}
`)
	writeTestFile(t, dir, "greeter/person.go", `package greeter

// Person is exported: its export data decodes cleanly, so this candidate
// was never affected by the unexported-implementer bug.
type Person struct{}

func (Person) Greet() string { return "hi" }
`)

	r, snap := newResolverForDir(t, dir)
	greeterFile := goFile(t, snap, "example.com/exportedimpl/greeter", "greeter.go")
	personFile := goFile(t, snap, "example.com/exportedimpl/greeter", "person.go")

	line, col := identOccurrence(t, greeterFile, "Greeter")
	locs, err := r.Implementation(context.Background(), greeterFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Greeter): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Greeter) = %+v, want exactly 1 result (Person)", locs)
	}
	wantLoc(t, locs, personFile, "Person")
}

// TestImplementation_GenericCandidateFallsBackToDecode covers the
// documented generics exclusion (registerMethodSet's doc): a generic
// receiver's method is never fingerprinted (Fingerprint stays 0, the
// sentinel implementingTypes treats as "cannot confirm via fingerprint"),
// so a generic candidate must still be found through the pre-fix
// decode-and-types.Implements fallback, exactly as before this change —
// proving the fallback path this fix keeps (rather than removes) still
// works. The candidate is exported so decode can actually succeed; an
// unexported generic candidate would be an entirely separate, unaddressed
// gap (fingerprinting excluded AND decode impossible), out of scope here.
func TestImplementation_GenericCandidateFallsBackToDecode(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/genericimpl\n\ngo 1.23\n")
	writeTestFile(t, dir, "store/store.go", `package store

type Saver interface {
	Save() error
}
`)
	writeTestFile(t, dir, "store/box.go", `package store

// Box's receiver is generic, so registerMethodSet leaves its Save entry
// unfingerprinted (see its doc); confirming Box implements Saver must fall
// back to decoding Box's export data and calling types.Implements.
type Box[T any] struct {
	Value T
}

func (b Box[T]) Save() error { return nil }
`)

	r, snap := newResolverForDir(t, dir)
	storeFile := goFile(t, snap, "example.com/genericimpl/store", "store.go")
	boxFile := goFile(t, snap, "example.com/genericimpl/store", "box.go")

	line, col := identOccurrence(t, storeFile, "Saver")
	locs, err := r.Implementation(context.Background(), storeFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Saver): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Saver) = %+v, want exactly 1 result (Box, via the decode fallback)", locs)
	}
	wantLoc(t, locs, boxFile, "Box")
}

// TestImplementation_GenericInterfaceFallsBackToDecode is
// TestImplementation_GenericCandidateFallsBackToDecode's interface-side
// counterpart: a generic INTERFACE also leaves its own methods
// unfingerprinted (registerInterfaceMethodSet's identical exclusion), so
// implementingTypes' ifaceGeneric check must route every candidate through
// the decode fallback too, rather than compare against an empty/zero
// fingerprint map. Reset's signature deliberately never mentions
// Container's type parameter T: go/types' own types.Implements already
// cannot structurally match an uninstantiated generic interface's method
// against a concrete candidate once T actually appears in that method's
// signature (confirmed empirically -- a pre-existing go/types limitation,
// unrelated to and unchanged by this fix, so not what this test is for);
// a method that never depends on T is the case where the decode fallback
// this fix retains for generics can still succeed.
func TestImplementation_GenericInterfaceFallsBackToDecode(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/genericiface\n\ngo 1.23\n")
	writeTestFile(t, dir, "container/container.go", `package container

type Container[T any] interface {
	Reset()
}
`)
	writeTestFile(t, dir, "container/box.go", `package container

type IntBox struct {
	V int
}

func (b IntBox) Reset() { b.V = 0 }
`)

	r, snap := newResolverForDir(t, dir)
	containerFile := goFile(t, snap, "example.com/genericiface/container", "container.go")
	boxFile := goFile(t, snap, "example.com/genericiface/container", "box.go")

	line, col := identOccurrence(t, containerFile, "Container")
	locs, err := r.Implementation(context.Background(), containerFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Container): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Container) = %+v, want exactly 1 result (IntBox, via the decode fallback)", locs)
	}
	wantLoc(t, locs, boxFile, "IntBox")
}

// TestImplementation_FingerprintMismatchExcludesUnexportedCandidate is the
// fingerprint-soundness pin the fix's own test plan calls for: two
// unexported candidates share a method NAME with the queried interface,
// but only one shares its full SIGNATURE too. Both are unexported, so
// neither can fall back to a successful decode -- this is exactly the
// shape that would have made the pre-fix name-only-then-decode design
// silently drop BOTH (the real bug), and would make an unsound
// "same name always confirms" fingerprint design wrongly include BOTH. The
// correct behavior is exactly one result: matching name AND signature
// confirms via fingerprint with no decode needed at all; matching name
// alone still correctly finds no confirmation (its mismatched fingerprint
// forces the decode fallback, which then fails for an unexported type and
// is skipped, never wrongly included).
func TestImplementation_FingerprintMismatchExcludesUnexportedCandidate(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/fpmismatch\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Saver interface {
	Save(x int) error
}
`)
	writeTestFile(t, dir, "match/match.go", `package match

// saverA's Save signature matches Saver's exactly.
type saverA struct{}

func (saverA) Save(x int) error { return nil }

func NewSaverA() *saverA { return &saverA{} }
`)
	writeTestFile(t, dir, "mismatch/mismatch.go", `package mismatch

// saverB shares Saver's method NAME but not its signature: the parameter
// type differs (string, not int). It must never be reported as an
// implementer.
type saverB struct{}

func (saverB) Save(x string) error { return nil }

func NewSaverB() *saverB { return &saverB{} }
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/fpmismatch/iface", "iface.go")
	matchFile := goFile(t, snap, "example.com/fpmismatch/match", "match.go")

	line, col := identOccurrence(t, ifaceFile, "Saver")
	locs, err := r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation(Saver): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation(Saver) = %+v, want exactly 1 result (saverA only; saverB's Save(string) must not match Save(int))", locs)
	}
	wantLoc(t, locs, matchFile, "saverA")
}
