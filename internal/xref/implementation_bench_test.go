package xref

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/index"
	"github.com/sivchari/golance/internal/store"
)

// benchModuleName is the synthetic module path generateBenchModule writes
// its packages under.
const benchModuleName = "example.com/benchimpl"

// benchImplementerStride controls what fraction of generateBenchModule's
// packages fully implement target.Iface (every stride-th package): a
// realistic implementation query has far more name-only candidates (every
// package with a Common method) than actual implementers, matching the
// production report's LookupMethod-by-name counts (e.g. Close:200 against
// a much smaller confirmed-implementer set).
const benchImplementerStride = 4

// generateBenchModule writes numPkgs synthetic packages plus one "target"
// package declaring Iface (two methods: Common, shared by every generated
// package, and Extra, present only on every benchImplementerStride-th one)
// to a fresh module rooted in tb.TempDir(), reproducing the shape a real
// Implementation query over a large monorepo hits: a method-name lookup
// (Common) with many more candidates than the interface's actual
// implementers (see implementingTypes' doc).
func generateBenchModule(tb testing.TB, numPkgs int) string {
	tb.Helper()
	root := tb.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+benchModuleName+"\n\ngo 1.26\n"), 0o600); err != nil {
		tb.Fatalf("write go.mod: %v", err)
	}

	targetDir := filepath.Join(root, "target")
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		tb.Fatalf("mkdir target: %v", err)
	}
	targetSrc := "package target\n\n" +
		"type Iface interface {\n" +
		"\tCommon(x int) string\n" +
		"\tExtra() error\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(targetDir, "target.go"), []byte(targetSrc), 0o600); err != nil {
		tb.Fatalf("write target.go: %v", err)
	}

	for i := 0; i < numPkgs; i++ {
		name := fmt.Sprintf("pkg%d", i)
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			tb.Fatalf("mkdir %s: %v", name, err)
		}
		var src strings.Builder
		fmt.Fprintf(&src, "package %s\n\ntype T struct{}\n\n", name)
		src.WriteString("func (t *T) Common(x int) string { return \"\" }\n")
		if i%benchImplementerStride == 0 {
			src.WriteString("\nfunc (t *T) Extra() error { return nil }\n")
		}
		if err := os.WriteFile(filepath.Join(dir, name+".go"), []byte(src.String()), 0o600); err != nil {
			tb.Fatalf("write %s.go: %v", name, err)
		}
	}
	return root
}

// newBenchResolver loads root's module, builds its facts/export index, and
// returns a Resolver over it plus the snapshot -- the benchmark counterpart
// of xref_test.go's newResolverForDir, parameterized over testing.TB so it
// works from both *testing.T and *testing.B. opts is forwarded to New,
// e.g. WithUnitCacheBytes for a test that needs a non-default byte bound.
func newBenchResolver(tb testing.TB, root string, opts ...Option) (*Resolver, *graph.Snapshot) {
	tb.Helper()
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		tb.Fatalf("graph.Load: %v", err)
	}

	db, err := store.Open(filepath.Join(tb.TempDir(), "index.db"))
	if err != nil {
		tb.Fatalf("store.Open: %v", err)
	}
	tb.Cleanup(func() {
		if err := db.Close(); err != nil {
			tb.Errorf("db.Close: %v", err)
		}
	})
	cas, err := store.OpenCAS(filepath.Join(tb.TempDir(), "cas"))
	if err != nil {
		tb.Fatalf("store.OpenCAS: %v", err)
	}

	stats, err := index.Build(context.Background(), snap, db, cas, index.Options{})
	if err != nil {
		tb.Fatalf("index.Build: %v", err)
	}
	if stats.Errors != 0 {
		tb.Fatalf("index.Build: %d errors", stats.Errors)
	}

	return New(db, cas, snap, false, opts...), snap
}

// benchIdentPos returns the (line, col) of name's first identifier
// occurrence in path, the *testing.B counterpart of xref_test.go's
// identOccurrence.
func benchIdentPos(tb testing.TB, path, name string) (line, col int) {
	tb.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		tb.Fatalf("parse %s: %v", path, err)
	}

	var pos token.Position
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			pos = fset.Position(id.Pos())
			found = true
			return false
		}
		return true
	})
	if !found {
		tb.Fatalf("%s: found no occurrence of %q", path, name)
	}
	return pos.Line, pos.Column
}

// benchTargetFile returns the absolute path of target.go in root's
// generated module.
func benchTargetFile(tb testing.TB, snap *graph.Snapshot) string {
	tb.Helper()
	pkg, ok := snap.Package(benchModuleName + "/target")
	if !ok {
		tb.Fatalf("package %s/target not in snapshot", benchModuleName)
	}
	for _, f := range pkg.GoFiles {
		if filepath.Base(f) == "target.go" {
			return f
		}
	}
	tb.Fatalf("target.go not found in %s/target's GoFiles", benchModuleName)
	return ""
}

// BenchmarkImplementation_Interface measures Resolver.Implementation at
// interface-name granularity (querying target.Iface itself) against a
// synthetic workspace of N packages, benchImplementerStride of which
// actually implement Iface -- see generateBenchModule's doc for why the
// rest still cost a full name-based candidate lookup.
func BenchmarkImplementation_Interface(b *testing.B) {
	for _, n := range []int{150, 400} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			root := generateBenchModule(b, n)
			r, snap := newBenchResolver(b, root)
			targetFile := benchTargetFile(b, snap)
			line, col := benchIdentPos(b, targetFile, "Iface")
			ctx := context.Background()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				locs, err := r.Implementation(ctx, targetFile, line, col)
				if err != nil {
					b.Fatalf("Implementation: %v", err)
				}
				if len(locs) == 0 {
					b.Fatalf("Implementation returned no results")
				}
			}
		})
	}
}

// BenchmarkImplementation_Method is BenchmarkImplementation_Interface's
// method-granularity counterpart, querying target.Iface's Extra method
// directly (see Resolver.implementationOfMethod).
func BenchmarkImplementation_Method(b *testing.B) {
	for _, n := range []int{150, 400} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			root := generateBenchModule(b, n)
			r, snap := newBenchResolver(b, root)
			targetFile := benchTargetFile(b, snap)
			line, col := benchIdentPos(b, targetFile, "Extra")
			ctx := context.Background()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				locs, err := r.Implementation(ctx, targetFile, line, col)
				if err != nil {
					b.Fatalf("Implementation: %v", err)
				}
				if len(locs) == 0 {
					b.Fatalf("Implementation returned no results")
				}
			}
		})
	}
}
