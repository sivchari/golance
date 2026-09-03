package xref

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
)

// closureBenchModuleName is the synthetic module path
// generateClosureBenchModule writes its packages under.
const closureBenchModuleName = "example.com/benchclosurerefs"

// closureBenchGiantCalls is how many separate call sites
// generateClosureBenchModule's single "giant" package makes to the hot
// symbol, reproducing the field report's "one giant generated-style
// package" alongside its wide reverse-dependency closure: a single unit
// contributing far more reference records than any of the ordinary
// one-call-site importers.
const closureBenchGiantCalls = 500

// generateClosureBenchModule writes a "hot" package declaring one exported
// function, numImporters packages that each import hot and call it exactly
// once (the reverse-dependency closure a References query on Hot must walk),
// and one "giant" package that imports hot and calls it
// closureBenchGiantCalls times in a single file -- reproducing, at a size
// this benchmark can still build in reasonable time, the shape a real
// monorepo's References-hangs report described: a wide closure (up to 1,751
// units in the field) dominated by a handful of unusually large ones.
func generateClosureBenchModule(tb testing.TB, numImporters int) string {
	tb.Helper()
	root := tb.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+closureBenchModuleName+"\n\ngo 1.26\n"), 0o600); err != nil {
		tb.Fatalf("write go.mod: %v", err)
	}

	hotDir := filepath.Join(root, "hot")
	if err := os.MkdirAll(hotDir, 0o750); err != nil {
		tb.Fatalf("mkdir hot: %v", err)
	}
	hotSrc := "package hot\n\nfunc Hot() {}\n"
	if err := os.WriteFile(filepath.Join(hotDir, "hot.go"), []byte(hotSrc), 0o600); err != nil {
		tb.Fatalf("write hot.go: %v", err)
	}

	for i := 0; i < numImporters; i++ {
		name := fmt.Sprintf("pkg%d", i)
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			tb.Fatalf("mkdir %s: %v", name, err)
		}
		src := fmt.Sprintf("package %s\n\nimport %q\n\nfunc Call() { hot.Hot() }\n", name, closureBenchModuleName+"/hot")
		if err := os.WriteFile(filepath.Join(dir, name+".go"), []byte(src), 0o600); err != nil {
			tb.Fatalf("write %s.go: %v", name, err)
		}
	}

	giantDir := filepath.Join(root, "giant")
	if err := os.MkdirAll(giantDir, 0o750); err != nil {
		tb.Fatalf("mkdir giant: %v", err)
	}
	var giant strings.Builder
	fmt.Fprintf(&giant, "package giant\n\nimport %q\n\nfunc CallAll() {\n", closureBenchModuleName+"/hot")
	for i := 0; i < closureBenchGiantCalls; i++ {
		giant.WriteString("\thot.Hot()\n")
	}
	giant.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(giantDir, "giant.go"), []byte(giant.String()), 0o600); err != nil {
		tb.Fatalf("write giant.go: %v", err)
	}

	return root
}

// closureBenchHotFile returns the absolute path of hot.go in root's
// generated module.
func closureBenchHotFile(tb testing.TB, snap *graph.Snapshot) string {
	tb.Helper()
	pkg, ok := snap.Package(closureBenchModuleName + "/hot")
	if !ok {
		tb.Fatalf("package %s/hot not in snapshot", closureBenchModuleName)
	}
	for _, f := range pkg.GoFiles {
		if filepath.Base(f) == "hot.go" {
			return f
		}
	}
	tb.Fatalf("hot.go not found in %s/hot's GoFiles", closureBenchModuleName)
	return ""
}

// closureBlobBytes reports, for every package in r's snapshot reachable
// from hot's own reverse-dependency closure, the total on-disk CAS blob
// size (fullBytes -- what a whole-blob os.ReadFile pays for, Facts, Export,
// and the trailing Files/Index sections together) versus the Facts
// section's size alone (factsBytes). This is a static property of the
// generated corpus, logged alongside BenchmarkReferences_ClosureScale's own
// timing purely for scale context: neither number is what References
// itself reads anymore (locationsForAll answers via
// [store.DB.PostingsFor], never decoding a closure package's facts blob at
// all -- see xref.go's doc), but it quantifies just how large the
// closure-walk read this benchmark's wall-clock improvement replaces used
// to be.
func closureBlobBytes(tb testing.TB, r *Resolver, snap *graph.Snapshot) (fullBytes, factsBytes int64) {
	tb.Helper()
	for _, pkgPath := range snap.ClosureUnits(closureBenchModuleName + "/hot") {
		pkgHash := store.Hash(pkgPath)
		ptr, err := r.db.GetUnit(context.Background(), pkgHash)
		if err != nil {
			tb.Fatalf("GetUnit(%s): %v", pkgPath, err)
		}
		blob, ok, err := r.cas.Get(context.Background(), ptr.BlobKey)
		if err != nil || !ok {
			tb.Fatalf("cas.Get(%s): ok=%v err=%v", pkgPath, ok, err)
		}
		fullBytes += int64(len(blob))
		_, factsLen, err := store.UnitFactsRange(blob)
		if err != nil {
			tb.Fatalf("UnitFactsRange(%s): %v", pkgPath, err)
		}
		factsBytes += int64(factsLen)
	}
	return fullBytes, factsBytes
}

// BenchmarkReferences_ClosureScale measures References' wall-clock cost
// against a synthetic workspace whose reverse-dependency closure over one
// hot symbol scales into the thousands, the regression shape a real
// monorepo report traced a 4.5+ minute References hang to (see this
// package's own git history / PR description for the field report this
// guards against). b.ReportMetric reports the closure's total on-disk blob
// size against its Facts-only size (see closureBlobBytes), so a before/after
// run of this benchmark reports both wall time and bytes read without any
// extra plumbing.
func BenchmarkReferences_ClosureScale(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			root := generateClosureBenchModule(b, n)
			r, snap := newBenchResolver(b, root)
			hotFile := closureBenchHotFile(b, snap)
			line, col := benchIdentPos(b, hotFile, "Hot")
			ctx := context.Background()
			wantLocs := n + closureBenchGiantCalls

			fullBytes, factsBytes := closureBlobBytes(b, r, snap)
			b.Logf("closure blob bytes: full=%d facts=%d", fullBytes, factsBytes)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				locs, err := r.References(ctx, hotFile, line, col, false)
				if err != nil {
					b.Fatalf("References: %v", err)
				}
				if len(locs) != wantLocs {
					b.Fatalf("References = %d locs, want %d", len(locs), wantLocs)
				}
			}
		})
	}
}
