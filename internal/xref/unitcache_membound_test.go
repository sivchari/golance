package xref

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generateFatBenchModule is generateBenchModule's counterpart for exercising
// unitCache's byte bound specifically: each package declares numFields
// exported struct fields and methods (rather than generateBenchModule's
// fixed two methods), so its facts/export blobs are large enough that a
// deliberately small WithUnitCacheBytes bound actually forces eviction
// mid-sweep -- the field report's "a few hundred candidate packages, each
// with a multi-MB export-data blob" shape, at a size this test can still
// run quickly.
func generateFatBenchModule(tb testing.TB, numPkgs, numFields int) string {
	tb.Helper()
	root := tb.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+benchModuleName+"\n\ngo 1.26\n"), 0o600); err != nil {
		tb.Fatalf("write go.mod: %v", err)
	}

	targetDir := filepath.Join(root, "target")
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		tb.Fatalf("mkdir target: %v", err)
	}
	targetSrc := "package target\n\ntype Iface interface {\n\tCommon(x int) string\n}\n"
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
		fmt.Fprintf(&src, "package %s\n\ntype T struct {\n", name)
		for f := 0; f < numFields; f++ {
			fmt.Fprintf(&src, "\tField%d string\n", f)
		}
		src.WriteString("}\n\n")
		src.WriteString("func (t *T) Common(x int) string { return \"\" }\n")
		for f := 0; f < numFields; f++ {
			fmt.Fprintf(&src, "func (t *T) Getter%d() string { return t.Field%d }\n", f, f)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".go"), []byte(src.String()), 0o600); err != nil {
			tb.Fatalf("write %s.go: %v", name, err)
		}
	}
	return root
}

// TestUnitCache_QuerySweepStaysUnderByteBound is the integration-level
// counterpart of TestUnitCache_PutEvictsByByteBound: rather than putting
// synthetic entries directly, it drives real Implementation queries (one
// per generated package's own Common method, forcing r.unitBlob to decode
// and cache that package) across enough packages that their combined
// facts+export size would, without a byte bound, comfortably exceed a
// deliberately small WithUnitCacheBytes -- pinning that a real query sweep
// (not just a synthetic put loop) keeps r.units' retained size bounded.
func TestUnitCache_QuerySweepStaysUnderByteBound(t *testing.T) {
	const numPkgs = 60
	const maxBytes = 32 << 10 // 32KiB: far below what 60 packages' facts+export would total unbounded

	root := generateFatBenchModule(t, numPkgs, 40)
	r, snap := newBenchResolver(t, root, WithUnitCacheBytes(maxBytes))
	ctx := context.Background()

	for i := 0; i < numPkgs; i++ {
		pkgPath := benchModuleName + "/" + fmt.Sprintf("pkg%d", i)
		pkg, ok := snap.Package(pkgPath)
		if !ok {
			t.Fatalf("package %s not in snapshot", pkgPath)
		}
		var file string
		for _, f := range pkg.GoFiles {
			if strings.HasSuffix(f, fmt.Sprintf("pkg%d.go", i)) {
				file = f
				break
			}
		}
		if file == "" {
			t.Fatalf("pkg%d.go not found in %s's GoFiles", i, pkgPath)
		}
		line, col := benchIdentPos(t, file, "Common")
		if _, err := r.References(ctx, file, line, col, false); err != nil {
			t.Fatalf("References on pkg%d.Common: %v", i, err)
		}

		if r.units.numBytes > r.units.maxBytes {
			t.Fatalf("after package %d: r.units.numBytes=%d exceeds maxBytes=%d", i, r.units.numBytes, r.units.maxBytes)
		}
	}

	hits, misses := r.units.stats()
	if misses < numPkgs {
		t.Errorf("misses=%d, want at least %d (one per distinct package touched)", misses, numPkgs)
	}
	if hits == 0 {
		t.Error("hits=0, want > 0 (repeat within-query lookups of an already-decoded package)")
	}
	// A cache bounded to maxBytes but forced to cycle through numPkgs
	// distinct packages must actually be evicting, not just happening to
	// fit: the earliest package's entry should no longer be resident.
	if r.units.order.Len() >= numPkgs {
		t.Errorf("order.Len()=%d, want fewer than %d entries resident (byte bound must have evicted some)", r.units.order.Len(), numPkgs)
	}
}
