package xref

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
)

const (
	pkgIface     = "example.com/xrefmod/iface"
	pkgImpl      = "example.com/xrefmod/impl"
	pkgUser      = "example.com/xrefmod/user"
	pkgUser2     = "example.com/xrefmod/user2"
	pkgInpkgtest = "example.com/xrefmod/inpkgtest"
)

// newTestResolver builds a facts index for testdata/module and returns a
// Resolver over it, plus the snapshot (for locating test source files).
func newTestResolver(t *testing.T) (*Resolver, *graph.Snapshot) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	return newResolverForDir(t, root)
}

// newResolverForDir is newTestResolver's shared core, parameterized by
// module root, for tests that need their own synthetic module instead of
// the shared testdata/module fixture.
func newResolverForDir(t *testing.T, root string) (*Resolver, *graph.Snapshot) {
	t.Helper()
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}

	stats, err := index.Build(context.Background(), snap, db, cas, index.Options{})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("index.Build: %d errors", stats.Errors)
	}

	return New(db, cas, snap, false), snap
}

// newResolverAndStoreForDir is newResolverForDir plus direct access to the
// underlying db/cas: most tests only need a Resolver, but a test that wants
// to corrupt or remove a specific package's stored blob after indexing
// (simulating export data becoming unavailable at query time -- see
// implementation_decodefailure_test.go) needs the store handles
// newResolverForDir keeps private to itself.
func newResolverAndStoreForDir(t *testing.T, root string) (*Resolver, *graph.Snapshot, *store.DB, *store.CAS) {
	t.Helper()
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	cas, err := store.OpenCAS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}

	stats, err := index.Build(context.Background(), snap, db, cas, index.Options{})
	if err != nil {
		t.Fatalf("index.Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("index.Build: %d errors", stats.Errors)
	}

	return New(db, cas, snap, false), snap, db, cas
}

// goFile returns the absolute path of the file named base in pkgPath's
// GoFiles, as reported by go/packages (matching what Resolver's fileToPkg
// index was built from).
func goFile(t *testing.T, snap *graph.Snapshot, pkgPath, base string) string {
	t.Helper()
	pkg, ok := snap.Package(pkgPath)
	if !ok {
		t.Fatalf("package %s not in snapshot", pkgPath)
	}
	for _, f := range pkg.GoFiles {
		if filepath.Base(f) == base {
			return f
		}
	}
	t.Fatalf("file %s not found in package %s", base, pkgPath)
	return ""
}

// identOccurrence parses path and returns the (line, col) of the first
// identifier named name, in source order. Using the parser (rather than a
// text search) means comments and substrings of longer identifiers (e.g.
// "Person" inside "NewPerson") can never produce a false match.
func identOccurrence(t *testing.T, path, name string) (line, col int) {
	t.Helper()
	positions := identOccurrences(t, path, name)
	p := positions[0]
	return p.Line, p.Column
}

// identOccurrences is identOccurrence's counterpart for every occurrence of
// name in path, in source order, for fixtures that declare the same
// identifier more than once (e.g. a method name shared by several
// receivers).
func identOccurrences(t *testing.T, path, name string) []token.Position {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var positions []token.Position
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			positions = append(positions, fset.Position(id.Pos()))
		}
		return true
	})
	if len(positions) < 1 {
		t.Fatalf("%s: found no occurrences of %q", path, name)
	}
	return positions
}

func TestDefinition_CrossPackage(t *testing.T) {
	r, snap := newTestResolver(t)

	userFile := goFile(t, snap, pkgUser, "user.go")
	line, col := identOccurrence(t, userFile, "Person") // impl.Person in Declare's return type

	locs, err := r.Definition(context.Background(), userFile, line, col)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Definition returned %d locations, want 1: %+v", len(locs), locs)
	}

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	wantLine, wantCol := identOccurrence(t, implFile, "Person") // the struct decl
	got := locs[0]
	if got.File != implFile || int(got.Line) != wantLine || int(got.Col) != wantCol {
		t.Errorf("Definition = %+v, want {%s %d %d}", got, implFile, wantLine, wantCol)
	}
}

func TestDefinition_OnDeclarationResolvesToItself(t *testing.T) {
	r, snap := newTestResolver(t)

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	locs, err := r.Definition(context.Background(), implFile, line, col)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 || locs[0].File != implFile || int(locs[0].Line) != line || int(locs[0].Col) != col {
		t.Errorf("Definition = %+v, want self at %s:%d:%d", locs, implFile, line, col)
	}
}

// TestReferences_SpansDefiningAndReferencingPackages verifies References
// returns every occurrence of impl.Person across impl (its own package,
// including the declaration and impl's own internal uses) and every
// importer (user, user2), matching gopls-equivalent completeness by count.
func TestReferences_SpansDefiningAndReferencingPackages(t *testing.T) {
	r, snap := newTestResolver(t)

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person") // the declaration

	locs, err := r.References(context.Background(), implFile, line, col, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}

	userFile := goFile(t, snap, pkgUser, "user.go")
	user2File := goFile(t, snap, pkgUser2, "user2.go")

	counts := map[string]int{}
	for _, l := range locs {
		counts[l.File]++
	}

	// impl.go: decl (1) + receiver type (2) + NewPerson return type (3) +
	// NewPerson composite literal (4) = 4 occurrences of "Person".
	// user.go: Declare's return type + composite literal = 2.
	// user2.go: Use2's return type = 1.
	want := map[string]int{implFile: 4, userFile: 2, user2File: 1}
	for file, n := range want {
		if counts[file] != n {
			t.Errorf("References in %s = %d, want %d (all locations: %+v)", file, counts[file], n, locs)
		}
	}
	if total := len(locs); total != 7 {
		t.Errorf("References total = %d, want 7: %+v", total, locs)
	}
}

func TestReferences_ExcludesDeclarationWhenNotIncluded(t *testing.T) {
	r, snap := newTestResolver(t)

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	declLine, declCol := identOccurrence(t, implFile, "Person")

	locs, err := r.References(context.Background(), implFile, declLine, declCol, false)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(locs) != 6 {
		t.Fatalf("References (excl. decl) = %d, want 6: %+v", len(locs), locs)
	}
	for _, l := range locs {
		if l.File == implFile && int(l.Line) == declLine && int(l.Col) == declCol {
			t.Errorf("References with includeDecl=false still returned the declaration: %+v", l)
		}
	}
}

func TestImplementation_InterfaceToImplementer(t *testing.T) {
	r, snap := newTestResolver(t)

	ifaceFile := goFile(t, snap, pkgIface, "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Greeter")

	locs, err := r.Implementation(context.Background(), ifaceFile, line, col)
	if err != nil {
		t.Fatalf("Implementation: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation returned %d locations, want 1: %+v", len(locs), locs)
	}

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	wantLine, wantCol := identOccurrence(t, implFile, "Person")
	got := locs[0]
	if got.File != implFile || int(got.Line) != wantLine || int(got.Col) != wantCol {
		t.Errorf("Implementation = %+v, want Person at %s:%d:%d", got, implFile, wantLine, wantCol)
	}
}

func TestImplementation_ConcreteTypeToInterface(t *testing.T) {
	r, snap := newTestResolver(t)

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	locs, err := r.Implementation(context.Background(), implFile, line, col)
	if err != nil {
		t.Fatalf("Implementation: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Implementation returned %d locations, want 1: %+v", len(locs), locs)
	}

	ifaceFile := goFile(t, snap, pkgIface, "iface.go")
	wantLine, wantCol := identOccurrence(t, ifaceFile, "Greeter")
	got := locs[0]
	if got.File != ifaceFile || int(got.Line) != wantLine || int(got.Col) != wantCol {
		t.Errorf("Implementation = %+v, want Greeter at %s:%d:%d", got, ifaceFile, wantLine, wantCol)
	}
}

func TestWorkspaceSymbol_PrefixMatch(t *testing.T) {
	r, snap := newTestResolver(t)

	results, err := r.WorkspaceSymbol(context.Background(), "Per")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	wantLine, wantCol := identOccurrence(t, implFile, "Person")

	found := false
	for _, s := range results {
		if s.Name != "Person" {
			continue
		}
		found = true
		if s.Container != pkgImpl {
			t.Errorf("Person.Container = %q, want %q", s.Container, pkgImpl)
		}
		if s.Kind != index.KindType {
			t.Errorf("Person.Kind = %d, want %d (KindType)", s.Kind, index.KindType)
		}
		if s.Location.File != implFile || int(s.Location.Line) != wantLine || int(s.Location.Col) != wantCol {
			t.Errorf("Person.Location = %+v, want %s:%d:%d", s.Location, implFile, wantLine, wantCol)
		}
	}
	if !found {
		t.Errorf("WorkspaceSymbol(%q) did not return Person: %+v", "Per", results)
	}
}

func TestWorkspaceSymbol_NoMatch(t *testing.T) {
	r, _ := newTestResolver(t)

	results, err := r.WorkspaceSymbol(context.Background(), "Zzz")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("WorkspaceSymbol(%q) = %+v, want none", "Zzz", results)
	}
}

func TestRename_EditsEveryReferenceAcrossFiles(t *testing.T) {
	r, snap := newTestResolver(t)

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	edits, err := r.Rename(context.Background(), implFile, line, col, "Human")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	userFile := goFile(t, snap, pkgUser, "user.go")
	user2File := goFile(t, snap, pkgUser2, "user2.go")

	want := map[string]int{implFile: 4, userFile: 2, user2File: 1}
	if len(edits) != len(want) {
		t.Fatalf("Rename touched %d files, want %d: %+v", len(edits), len(want), edits)
	}
	for file, n := range want {
		es, ok := edits[file]
		if !ok {
			t.Errorf("Rename has no edits for %s", file)
			continue
		}
		if len(es) != n {
			t.Errorf("Rename edits in %s = %d, want %d: %+v", file, len(es), n, es)
		}
		for _, e := range es {
			if e.NewText != "Human" {
				t.Errorf("edit NewText = %q, want %q", e.NewText, "Human")
			}
		}
	}
}

// TestQueries_HonorCanceledContext verifies that every xref query entry
// point (finding 11) checks ctx before doing any real work: called with an
// already-canceled context, each must return promptly with an error
// satisfying errors.Is(err, context.Canceled) — a real context error, not a
// degraded-to-empty "nothing found" result (the distinction that matters to
// a caller like internal/rpc's dispatch layer, which maps exactly this
// error into a RequestCancelled response).
func TestQueries_HonorCanceledContext(t *testing.T) {
	r, snap := newTestResolver(t)
	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.Definition(ctx, implFile, line, col); !errors.Is(err, context.Canceled) {
		t.Errorf("Definition with a canceled context: err = %v, want context.Canceled", err)
	}
	if _, err := r.References(ctx, implFile, line, col, true); !errors.Is(err, context.Canceled) {
		t.Errorf("References with a canceled context: err = %v, want context.Canceled", err)
	}
	if _, err := r.Implementation(ctx, implFile, line, col); !errors.Is(err, context.Canceled) {
		t.Errorf("Implementation with a canceled context: err = %v, want context.Canceled", err)
	}
	if _, err := r.WorkspaceSymbol(ctx, "Per"); !errors.Is(err, context.Canceled) {
		t.Errorf("WorkspaceSymbol with a canceled context: err = %v, want context.Canceled", err)
	}
	if _, err := r.Rename(ctx, implFile, line, col, "Human"); !errors.Is(err, context.Canceled) {
		t.Errorf("Rename with a canceled context: err = %v, want context.Canceled", err)
	}
}

// TestLocationsFor_HonorsCanceledContext exercises the per-package loop
// boundary check inside locationsFor directly (rather than only the
// entry-point check in resolveAt, which TestQueries_HonorCanceledContext
// already covers indirectly): resolveAt runs first, uncanceled, to obtain a
// real target; ctx is then canceled before locationsFor's own closure-unit
// loop ever runs. includeDecl is false so the very first thing locationsFor
// does is enter that loop, whose ctx.Err() check (checked once per package,
// see locationsFor's doc) must fire on its first iteration.
func TestLocationsFor_HonorsCanceledContext(t *testing.T) {
	r, snap := newTestResolver(t)
	implFile := goFile(t, snap, pkgImpl, "impl.go")
	line, col := identOccurrence(t, implFile, "Person")

	l, c, err := toUint32Pos(line, col)
	if err != nil {
		t.Fatalf("toUint32Pos: %v", err)
	}
	target, err := r.resolveAt(context.Background(), implFile, l, c)
	if err != nil {
		t.Fatalf("resolveAt: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.locationsFor(ctx, target, false); !errors.Is(err, context.Canceled) {
		t.Errorf("locationsFor with a canceled context: err = %v, want context.Canceled", err)
	}
}

// TestResolveAt_MapsTestFilePositionToUnit verifies that resolveAt can
// resolve a position inside an in-package "_test.go" file at all: r.fileToPkg
// is built from graph.Package.GoFiles, which internal/graph's loadMode (no
// packages.Config.Tests) never populates with a _test.go path (see
// internal/index/testfiles.go's identical gap for the facts index itself,
// closed by PR #36). Before Resolver grew its own directory fallback, this
// failed with "is not part of any known package" for every _test.go
// position, regardless of what the position pointed at.
func TestResolveAt_MapsTestFilePositionToUnit(t *testing.T) {
	r, snap := newTestResolver(t)
	pkg, ok := snap.Package(pkgInpkgtest)
	if !ok {
		t.Fatalf("package %s not in snapshot", pkgInpkgtest)
	}
	testFile := filepath.Join(pkg.Dir, "inpkgtest_test.go")
	line, col := identOccurrence(t, testFile, "useImplPerson") // the func decl, resolves to itself

	l, c, err := toUint32Pos(line, col)
	if err != nil {
		t.Fatalf("toUint32Pos: %v", err)
	}
	sym, err := r.resolveAt(context.Background(), testFile, l, c)
	if err != nil {
		t.Fatalf("resolveAt: %v", err)
	}
	if sym.Name != "useImplPerson" {
		t.Errorf("resolveAt = %+v, want a symbol named useImplPerson", sym)
	}
}

// TestResolveAt_ExternalTestPackageFileStillDegrades pins that resolveAt's
// directory fallback (see TestResolveAt_MapsTestFilePositionToUnit) does not
// blindly trust every file in a known package's directory: inpkgtest's
// external "_test"-suffixed test package file sits in the same directory as
// inpkgtest_test.go, but its own package clause ("inpkgtest_test") fails
// testFilesInPackage's canonical-name filter, so it never joined
// inpkgtest's facts. fileIndexOf's lookup against the unit's own facts file
// table — the source of truth for what was actually indexed — must still
// reject a position here.
func TestResolveAt_ExternalTestPackageFileStillDegrades(t *testing.T) {
	r, snap := newTestResolver(t)
	pkg, ok := snap.Package(pkgInpkgtest)
	if !ok {
		t.Fatalf("package %s not in snapshot", pkgInpkgtest)
	}
	extFile := filepath.Join(pkg.Dir, "inpkgtest_ext_test.go")
	line, col := identOccurrence(t, extFile, "ExternalOnly")

	l, c, err := toUint32Pos(line, col)
	if err != nil {
		t.Fatalf("toUint32Pos: %v", err)
	}
	if _, err := r.resolveAt(context.Background(), extFile, l, c); err == nil {
		t.Fatal("resolveAt succeeded for a position in the external test package file, want an error (never indexed)")
	}
}

// TestDefinition_FromTestFilePosition_CrossPackage verifies the user-visible
// bug: a definition query issued from a position inside an in-package
// "_test.go" file, targeting a symbol in a *different* workspace package,
// must resolve through the facts index to the exact declaration — not just
// the same-package case PR #36's e2e coverage exercised (masked by
// definitionFallback's SamePackageDefinition at the server layer, which
// this xref-level test bypasses entirely by calling Definition directly).
func TestDefinition_FromTestFilePosition_CrossPackage(t *testing.T) {
	r, snap := newTestResolver(t)
	pkg, ok := snap.Package(pkgInpkgtest)
	if !ok {
		t.Fatalf("package %s not in snapshot", pkgInpkgtest)
	}
	testFile := filepath.Join(pkg.Dir, "inpkgtest_test.go")
	line, col := identOccurrence(t, testFile, "Person") // impl.Person in useImplPerson's return type

	locs, err := r.Definition(context.Background(), testFile, line, col)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("Definition returned %d locations, want 1: %+v", len(locs), locs)
	}

	implFile := goFile(t, snap, pkgImpl, "impl.go")
	wantLine, wantCol := identOccurrence(t, implFile, "Person") // the struct decl
	got := locs[0]
	if got.File != implFile || int(got.Line) != wantLine || int(got.Col) != wantCol {
		t.Errorf("Definition = %+v, want {%s %d %d}", got, implFile, wantLine, wantCol)
	}
}
