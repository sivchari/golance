package xref

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// refsBenchModuleName is the synthetic module path
// generateRefsBenchModule writes its packages under.
const refsBenchModuleName = "example.com/benchrefsrev"

// refsBenchSatisfierStride mirrors benchImplementerStride's role, on the
// candidate-interface side instead of the candidate-implementer side: only
// every stride-th generated interface consists of Common alone (so
// target.Concrete actually satisfies it); the rest also require an Extra
// method Concrete does not have, so interfacesSatisfiedByMethod's
// types.Implements confirmation must reject them despite each one
// surfacing as a same-name "Common" candidate.
const refsBenchSatisfierStride = 4

// generateRefsBenchModule writes numIfaces synthetic single-method-name
// candidate interfaces plus one "target" package declaring a concrete type
// with a Common method, reproducing the shape References on a concrete
// method hits in a large workspace: a single-name LookupMethod("Common")
// lookup (see interfacesSatisfiedByMethod's doc for why this stays bounded
// to one name, unlike Implementation's own concrete -> interfaces
// direction) against many same-name interface candidates, most of which
// the concrete type does not actually satisfy.
func generateRefsBenchModule(tb testing.TB, numIfaces int) string {
	tb.Helper()
	root := tb.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+refsBenchModuleName+"\n\ngo 1.26\n"), 0o600); err != nil {
		tb.Fatalf("write go.mod: %v", err)
	}

	targetDir := filepath.Join(root, "target")
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		tb.Fatalf("mkdir target: %v", err)
	}
	targetSrc := "package target\n\n" +
		"type Concrete struct{}\n\n" +
		"func (c *Concrete) Common(x int) string { return \"\" }\n"
	if err := os.WriteFile(filepath.Join(targetDir, "target.go"), []byte(targetSrc), 0o600); err != nil {
		tb.Fatalf("write target.go: %v", err)
	}

	for i := 0; i < numIfaces; i++ {
		name := fmt.Sprintf("pkg%d", i)
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			tb.Fatalf("mkdir %s: %v", name, err)
		}
		var src strings.Builder
		fmt.Fprintf(&src, "package %s\n\ntype Iface interface {\n\tCommon(x int) string\n", name)
		if i%refsBenchSatisfierStride != 0 {
			src.WriteString("\tExtra() error\n")
		}
		src.WriteString("}\n")
		if err := os.WriteFile(filepath.Join(dir, name+".go"), []byte(src.String()), 0o600); err != nil {
			tb.Fatalf("write %s.go: %v", name, err)
		}
	}
	return root
}

// refsBenchTargetFile returns the absolute path of target.go in root's
// generated module, the refsBenchModuleName counterpart of
// implementation_bench_test.go's benchTargetFile.
func refsBenchTargetFile(tb testing.TB, snap *graph.Snapshot) string {
	tb.Helper()
	pkg, ok := snap.Package(refsBenchModuleName + "/target")
	if !ok {
		tb.Fatalf("package %s/target not in snapshot", refsBenchModuleName)
	}
	for _, f := range pkg.GoFiles {
		if filepath.Base(f) == "target.go" {
			return f
		}
	}
	tb.Fatalf("target.go not found in %s/target's GoFiles", refsBenchModuleName)
	return ""
}

// BenchmarkReferences_ConcreteMethodAgainstManyDecoyInterfaces measures
// References at a concrete method's own declaration against a synthetic
// workspace of N same-method-name candidate interfaces, only
// refsBenchSatisfierStride of which the queried concrete type actually
// satisfies -- the pathological case interfacesSatisfiedByMethod's own doc
// calls out: many decoys sharing a common method name (Close, String, ...
// modeled here as Common) must not blow up References' cost, since the
// candidate list is bounded by a single LookupMethod("Common") posting
// list and per-candidate confirmation is cached across the whole query
// (see unitCache's doc).
func BenchmarkReferences_ConcreteMethodAgainstManyDecoyInterfaces(b *testing.B) {
	for _, n := range []int{150, 400} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			root := generateRefsBenchModule(b, n)
			r, snap := newBenchResolver(b, root)
			targetFile := refsBenchTargetFile(b, snap)
			line, col := benchIdentPos(b, targetFile, "Common")
			ctx := context.Background()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := r.References(ctx, targetFile, line, col, false); err != nil {
					b.Fatalf("References: %v", err)
				}
			}
		})
	}
}
