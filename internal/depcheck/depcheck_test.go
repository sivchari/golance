package depcheck

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/graph"
	"golang.org/x/tools/go/types/objectpath"
)

// loadTestGraph loads depcheck's own tiny fixture module (testdata/module)
// as a real *graph.Snapshot — the same Phase 1 mechanism production code
// uses (Tests: true, full transitive closure) — and wraps it as a
// MetadataSource.
func loadTestGraph(t *testing.T) MetadataSource {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	return NewGraphMetadataSource(snap)
}

// declIdentInFile returns the (line, column) of name's top-level declaring
// identifier in path, parsed independently of any Provider — the
// ground-truth position TestProvider_StdlibExactPosition checks the
// Provider's own result against.
func declIdentInFile(t *testing.T, path, name string) (line, col int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var found *ast.Ident
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name == name {
				found = d.Name
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == name {
					found = ts.Name
				}
			}
		}
	}
	if found == nil {
		t.Fatalf("no top-level func/type declaration named %q in %s", name, path)
	}
	p := fset.Position(found.Pos())
	return p.Line, p.Column
}

// TestProvider_StdlibExactPosition verifies the central precision claim: a
// Provider-checked stdlib package's declaration positions are byte-exact
// (real line AND column), not export data's line-only encoding. The
// expected position is derived independently, by parsing the real GOROOT
// file's content directly, rather than hardcoded — so this stays correct
// across Go versions that reformat or move these declarations.
func TestProvider_StdlibExactPosition(t *testing.T) {
	meta := loadTestGraph(t)
	p := NewProvider(meta, Options{})

	cp, err := p.Package(context.Background(), "strings")
	if err != nil {
		t.Fatalf("Package(strings): %v", err)
	}

	tests := []string{"Builder", "Join"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			obj := cp.Types().Scope().Lookup(name)
			if obj == nil {
				t.Fatalf("strings.%s not found in checked package scope", name)
			}
			if !obj.Pos().IsValid() {
				t.Fatalf("strings.%s has no valid position", name)
			}
			got := p.FileSet().Position(obj.Pos())
			wantLine, wantCol := declIdentInFile(t, got.Filename, name)
			if got.Line != wantLine || got.Column != wantCol {
				t.Errorf("strings.%s position = %d:%d, want %d:%d (from parsing %s directly)", name, got.Line, got.Column, wantLine, wantCol, got.Filename)
			}
		})
	}
}

// TestProvider_Decl_UnexportedScopeLookup covers Decl's fallback path: an
// unexported object objectpath cannot encode a path for (see resolveObject's
// doc), resolved instead by a package-level scope lookup by name. obj is
// deliberately resolved from a SEPARATE Provider's independent check of the
// same package — simulating a caller (like the workspace side of
// DependencyDefinition) that resolved obj against some OTHER *types.Package
// instance for this import path, exactly the instance-boundary Decl exists
// to cross.
func TestProvider_Decl_UnexportedScopeLookup(t *testing.T) {
	meta := loadTestGraph(t)
	ctx := context.Background()
	const pkgPath = "example.com/depcheckmod/dep"

	other := NewProvider(meta, Options{})
	otherCP, err := other.Package(ctx, pkgPath)
	if err != nil {
		t.Fatalf("Package(%s) on the other provider: %v", pkgPath, err)
	}
	obj := otherCP.Types().Scope().Lookup("secret")
	if obj == nil {
		t.Fatal("dep.secret not found in the other provider's checked package scope")
	}
	if obj.Exported() {
		t.Fatal("dep.secret is exported, want unexported (test fixture invariant)")
	}

	p := NewProvider(meta, Options{})
	id, fset, err := p.Decl(ctx, pkgPath, obj)
	if err != nil {
		t.Fatalf("Decl: %v", err)
	}
	if id.Name != "secret" {
		t.Errorf("Decl returned identifier %q, want %q", id.Name, "secret")
	}
	if fset != p.FileSet() {
		t.Error("Decl returned a *token.FileSet other than p's own FileSet")
	}
	pos := fset.Position(id.Pos())
	if !filepath.IsAbs(pos.Filename) || filepath.Base(filepath.Dir(pos.Filename)) != "dep" {
		t.Errorf("Filename = %q, want an absolute path inside the dep fixture package's directory", pos.Filename)
	}
}

// TestProvider_LRUEviction verifies that, with the LRU capacity set to 1,
// only the most recently checked package stays resident: an older entry is
// evicted and, if requested again, freshly re-checked rather than served
// from a stale cache slot. This is the "entries are released" bound on
// memory the LRU exists to enforce.
func TestProvider_LRUEviction(t *testing.T) {
	meta := loadTestGraph(t)
	p := NewProvider(meta, Options{Cap: 1})
	ctx := context.Background()

	if _, err := p.Package(ctx, "errors"); err != nil {
		t.Fatalf("Package(errors): %v", err)
	}
	if got := p.Len(); got > 1 {
		t.Errorf("Len() = %d after checking one package with Cap=1, want <= 1", got)
	}
	checkedAfterFirst := p.Checked()

	if _, err := p.Package(ctx, "errors"); err != nil {
		t.Fatalf("Package(errors) again: %v", err)
	}
	if got := p.Checked(); got != checkedAfterFirst {
		t.Errorf("Checked() = %d after a cache hit, want unchanged %d", got, checkedAfterFirst)
	}

	if _, err := p.Package(ctx, "strings"); err != nil {
		t.Fatalf("Package(strings): %v", err)
	}
	if got := p.Len(); got > 1 {
		t.Errorf("Len() = %d after checking a second package with Cap=1, want <= 1", got)
	}

	if _, err := p.Package(ctx, "errors"); err != nil {
		t.Fatalf("Package(errors) a third time: %v", err)
	}
	if got := p.Checked(); got <= checkedAfterFirst {
		t.Errorf("Checked() = %d, want > %d (errors should have been evicted by checking strings, and re-checked here)", got, checkedAfterFirst)
	}
}

// countingMetadataSource wraps a MetadataSource, counting how many times
// Package is called for each import path — used to prove singleflight
// actually collapsed N concurrent callers for the same path onto a single
// underlying check, not just that they happened to observe the same result.
type countingMetadataSource struct {
	MetadataSource
	mu     sync.Mutex
	counts map[string]int
}

func newCountingMetadataSource(inner MetadataSource) *countingMetadataSource {
	return &countingMetadataSource{MetadataSource: inner, counts: make(map[string]int)}
}

func (c *countingMetadataSource) Package(pkgPath string) (string, []string, []string, bool) {
	c.mu.Lock()
	c.counts[pkgPath]++
	c.mu.Unlock()
	return c.MetadataSource.Package(pkgPath)
}

func (c *countingMetadataSource) count(pkgPath string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[pkgPath]
}

// TestProvider_Singleflight verifies that many concurrent Package calls for
// the SAME uncached package collapse onto a single underlying check
// (singleflight), rather than each independently parsing and type-checking
// it: countingMetadataSource.Package (the only way Provider.check learns a
// package's metadata) is called exactly once for the target path despite
// 20 concurrent callers, and every caller observes the identical
// *CheckedPackage instance.
func TestProvider_Singleflight(t *testing.T) {
	meta := newCountingMetadataSource(loadTestGraph(t))
	p := NewProvider(meta, Options{})
	ctx := context.Background()
	const pkgPath = "example.com/depcheckmod/dep"

	const n = 20
	var wg sync.WaitGroup
	results := make([]*CheckedPackage, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.Package(ctx, pkgPath)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Package(%s): %v", i, pkgPath, err)
		}
	}
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Errorf("goroutine %d got a different *CheckedPackage than goroutine 0", i)
		}
	}
	if got := meta.count(pkgPath); got != 1 {
		t.Errorf("MetadataSource.Package(%s) called %d times, want exactly 1 (singleflight should collapse %d concurrent callers)", pkgPath, got, n)
	}
}

// TestProvider_TestOnlyDependency verifies a Provider can check a
// test-only dependency (here, "testing" itself) purely from Phase 1's graph
// metadata — the graph includes test variants (internal/graph's package
// doc), so "testing" is known even though nothing in this fixture module's
// non-test code imports it.
func TestProvider_TestOnlyDependency(t *testing.T) {
	meta := loadTestGraph(t)
	p := NewProvider(meta, Options{})

	cp, err := p.Package(context.Background(), "testing")
	if err != nil {
		t.Fatalf("Package(testing): %v", err)
	}
	if obj := cp.Types().Scope().Lookup("T"); obj == nil {
		t.Error("testing.T not found in checked package scope")
	}
}

// TestProvider_ColdCheckTiming measures (and logs, run with -v) cold
// type-check time for a few real dependencies of golance's own module: a
// small stdlib package ("fmt"), and two real, moderately large module
// dependencies golance already requires (go.etcd.io/bbolt,
// golang.org/x/tools/go/packages) — see the task's request to measure and
// report this. It loads golance's own repository graph (not the small
// fixture module the other tests use) specifically to reach these real
// dependencies without adding a test-only go.mod requirement.
func TestProvider_ColdCheckTiming(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load(golance's own module): %v", err)
	}
	meta := NewGraphMetadataSource(snap)

	for _, pkgPath := range []string{"fmt", "go.etcd.io/bbolt", "golang.org/x/tools/go/packages"} {
		t.Run(pkgPath, func(t *testing.T) {
			p := NewProvider(meta, Options{})
			start := time.Now()
			cp, err := p.Package(context.Background(), pkgPath)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("Package(%s): %v", pkgPath, err)
			}
			t.Logf("cold check of %s: %s (%d files, %d packages checked total incl. its own dependency closure)", pkgPath, elapsed, len(cp.Files()), p.Checked())
		})
	}
}

// TestProvider_DocAt verifies DocAt returns the real, ParseComments-parsed
// doc comment for an exported declaration — the mechanism
// internal/server.crossPackageDoc uses to answer a workspace file's hover
// into a dependency (task item 2), unlike export data, which carries no
// doc comment at all.
func TestProvider_DocAt(t *testing.T) {
	meta := loadTestGraph(t)
	ctx := context.Background()
	const pkgPath = "example.com/depcheckmod/dep"

	p := NewProvider(meta, Options{})
	cp, err := p.Package(ctx, pkgPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", pkgPath, err)
	}
	obj := cp.Types().Scope().Lookup("Greet")
	if obj == nil {
		t.Fatal("dep.Greet not found in the checked package scope")
	}
	objPath, err := objectpath.For(obj)
	if err != nil {
		t.Fatalf("objectpath.For(Greet): %v", err)
	}

	doc, err := p.DocAt(ctx, pkgPath, string(objPath))
	if err != nil {
		t.Fatalf("DocAt(%s, %s): %v", pkgPath, objPath, err)
	}
	const want = "Greet returns a greeting for name, built with strings.Builder.\n"
	if doc != want {
		t.Errorf("DocAt(Greet) = %q, want %q", doc, want)
	}
}

// TestProvider_DocAt_NotFound verifies DocAt reports an error, rather than
// panicking or silently returning "", for an objPath that does not resolve
// against pkgPath's checked package.
func TestProvider_DocAt_NotFound(t *testing.T) {
	meta := loadTestGraph(t)
	p := NewProvider(meta, Options{})

	if _, err := p.DocAt(context.Background(), "example.com/depcheckmod/dep", "NoSuchSymbol"); err == nil {
		t.Error("DocAt for a nonexistent objPath returned no error, want one")
	}
}
