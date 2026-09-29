package xref

import (
	"context"
	"go/token"
	"path/filepath"
	"testing"
)

// TestReferences_MethodOnUnexportedReceiver_DirectCallSite pins the
// confirmed production bug (v0.7.20): References on a method whose receiver
// is an unexported struct used to fail the WHOLE call, because
// correspondingMethodSymbols' resolveMethodFunc decodes the method's own
// defining package's export data and looks up its objectpath there --
// export data never carries an unexported package-scope type at all, so
// that lookup always errored ("package does not contain ..."), and that
// error propagated all the way out of References instead of leaving target's
// own (fully facts-resolvable, no decode needed) direct call sites intact.
// This is the everyday unexported-repository-struct pattern with no
// interface involved at all.
func TestReferences_MethodOnUnexportedReceiver_DirectCallSite(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/unexportedrecv\n\ngo 1.23\n")
	writeTestFile(t, dir, "repo/repo.go", `package repo

// bonusDeductionItem is unexported: its own package's export data never
// carries it at all.
type bonusDeductionItem struct{}

func (b bonusDeductionItem) BatchGet() []int { return nil }

// CallDirect calls BatchGet from within the same package.
func CallDirect() []int {
	item := bonusDeductionItem{}
	return item.BatchGet()
}
`)

	r, snap := newResolverForDir(t, dir)
	repoFile := goFile(t, snap, "example.com/unexportedrecv/repo", "repo.go")

	line, col := identOccurrence(t, repoFile, "BatchGet")
	locs, err := r.References(context.Background(), repoFile, line, col, false)
	if err != nil {
		t.Fatalf("References(BatchGet): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("References(BatchGet) = %+v, want exactly 1 result (CallDirect's own call site)", locs)
	}
	occurrences := identOccurrences(t, repoFile, "BatchGet")
	want := occurrences[1] // occurrence 0 is the declaration itself
	if int(locs[0].Line) != want.Line || int(locs[0].Col) != want.Column {
		t.Errorf("References(BatchGet) = %+v, want the call site at %d:%d", locs, want.Line, want.Column)
	}
}

// TestReferences_MethodOnUnexportedReceiver_SatisfiesExportedInterface
// covers the same unexported-receiver bug queried from the CONCRETE method's
// own declaration, where the correct result also depends on resolving the
// interface it satisfies (Fetcher.BatchGet) -- the direction
// interfacesSatisfiedByMethod used to require decoding the unexported
// receiver's own export data for (via methodReceiver/resolveMethodFunc),
// which is structurally impossible for an unexported type. The fix derives
// the receiver's identity from the facts index instead, so this must find
// both the direct, same-package call site and the interface-typed call site
// in a different package.
func TestReferences_MethodOnUnexportedReceiver_SatisfiesExportedInterface(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/unexportedrecviface\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Fetcher interface {
	BatchGet() []int
}
`)
	writeTestFile(t, dir, "repo/repo.go", `package repo

import "example.com/unexportedrecviface/iface"

// bonusDeductionItem is unexported; NewRepo hides it behind the exported
// Fetcher interface, the idiomatic "constructor returns the interface"
// pattern.
type bonusDeductionItem struct{}

func (b bonusDeductionItem) BatchGet() []int { return nil }

func NewRepo() iface.Fetcher { return bonusDeductionItem{} }

// CallDirect calls BatchGet directly, within repo's own package, on the
// concrete (never exported) type.
func CallDirect() []int {
	item := bonusDeductionItem{}
	return item.BatchGet()
}
`)
	writeTestFile(t, dir, "use/use.go", `package use

import "example.com/unexportedrecviface/iface"

func CallInterface(f iface.Fetcher) []int {
	return f.BatchGet()
}
`)

	r, snap := newResolverForDir(t, dir)
	repoFile := goFile(t, snap, "example.com/unexportedrecviface/repo", "repo.go")
	useFile := goFile(t, snap, "example.com/unexportedrecviface/use", "use.go")

	line, col := identOccurrence(t, repoFile, "BatchGet")
	locs, err := r.References(context.Background(), repoFile, line, col, false)
	if err != nil {
		t.Fatalf("References(bonusDeductionItem.BatchGet): %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("References(bonusDeductionItem.BatchGet) = %+v, want 2 (repo's own direct call plus use's interface-typed call)", locs)
	}
	wantBatchGetLoc(t, locs, repoFile, identOccurrences(t, repoFile, "BatchGet")[1])
	wantBatchGetLoc(t, locs, useFile, identOccurrences(t, useFile, "BatchGet")[0])
}

// wantBatchGetLoc fails the test unless locs contains a reference at want's
// position in file.
func wantBatchGetLoc(t *testing.T, locs []Location, file string, want token.Position) {
	t.Helper()
	for _, l := range locs {
		if l.File == file && int(l.Line) == want.Line && int(l.Col) == want.Column {
			return
		}
	}
	t.Errorf("locations %+v missing BatchGet call site at %s:%d:%d", locs, file, want.Line, want.Column)
}

// TestReferences_MethodDeclaredInTestFile covers the other export-data gap
// the fix targets: a type declared in an in-package "_test.go" file (the
// mock pattern) is present in the facts index (the test-inclusive facts
// pass -- see internal/index's checkOnePackage) but never in Export, which
// is always built from non-test files alone. References on such a method
// must still succeed and find a call site in a different _test.go file of
// the same package.
func TestReferences_MethodDeclaredInTestFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/testfilerecv\n\ngo 1.23\n")
	writeTestFile(t, dir, "mock/mock.go", `package mock

// Real exists only so the package has a non-test file to derive Export
// from.
type Real struct{}
`)
	writeTestFile(t, dir, "mock/mock_test.go", `package mock

// fakeThing is declared in a _test.go file: it never appears in Export at
// all.
type fakeThing struct{}

func (f fakeThing) Do() int { return 0 }
`)
	writeTestFile(t, dir, "mock/other_test.go", `package mock

func useFake() int {
	f := fakeThing{}
	return f.Do()
}
`)

	r, snap := newResolverForDir(t, dir)
	pkg, ok := snap.Package("example.com/testfilerecv/mock")
	if !ok {
		t.Fatalf("package mock not in snapshot")
	}
	declFile := filepath.Join(pkg.Dir, "mock_test.go")
	useFile := filepath.Join(pkg.Dir, "other_test.go")

	line, col := identOccurrence(t, declFile, "Do")
	locs, err := r.References(context.Background(), declFile, line, col, false)
	if err != nil {
		t.Fatalf("References(fakeThing.Do): %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("References(fakeThing.Do) = %+v, want exactly 1 result (useFake's own call site)", locs)
	}
	occurrences := identOccurrences(t, useFile, "Do")
	want := occurrences[0]
	if locs[0].File != useFile || int(locs[0].Line) != want.Line || int(locs[0].Col) != want.Column {
		t.Errorf("References(fakeThing.Do) = %+v, want the call site in %s at %d:%d", locs, useFile, want.Line, want.Column)
	}
}
