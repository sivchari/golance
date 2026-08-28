package xref

import (
	"context"
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
	pkgIface = "example.com/xrefmod/iface"
	pkgImpl  = "example.com/xrefmod/impl"
	pkgUser  = "example.com/xrefmod/user"
	pkgUser2 = "example.com/xrefmod/user2"
)

// newTestResolver builds a facts index for testdata/module and returns a
// Resolver over it, plus the snapshot (for locating test source files).
func newTestResolver(t *testing.T) (*Resolver, *graph.Snapshot) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
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
	p := positions[0]
	return p.Line, p.Column
}

func TestDefinition_CrossPackage(t *testing.T) {
	r, snap := newTestResolver(t)

	userFile := goFile(t, snap, pkgUser, "user.go")
	line, col := identOccurrence(t, userFile, "Person") // impl.Person in Declare's return type

	locs, err := r.Definition(userFile, line, col)
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

	locs, err := r.Definition(implFile, line, col)
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

	locs, err := r.References(implFile, line, col, true)
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

	locs, err := r.References(implFile, declLine, declCol, false)
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

	locs, err := r.Implementation(ifaceFile, line, col)
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

	locs, err := r.Implementation(implFile, line, col)
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

	results, err := r.WorkspaceSymbol("Per")
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

	results, err := r.WorkspaceSymbol("Zzz")
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

	edits, err := r.Rename(implFile, line, col, "Human")
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
