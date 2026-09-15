package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// generateChainModule writes a synthetic module of n packages, pkg0..pkg(n-1),
// where pkgI imports pkg(I-1) (pkg0 has no imports), to dir. This gives each
// package a fan-in of exactly 1 (except the last, which has none), so the
// reference-count eviction logic should never hold more than a small,
// parallelism-bounded number of decoded dependencies at once, however many
// packages are processed in total.
func generateChainModule(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/chain\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for i := 0; i < n; i++ {
		pkgDir := filepath.Join(dir, fmt.Sprintf("pkg%d", i))
		if err := os.MkdirAll(pkgDir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", pkgDir, err)
		}
		var src string
		if i == 0 {
			src = fmt.Sprintf("package pkg%d\n\n// V returns %d.\nfunc V() int { return %d }\n", i, i, i)
		} else {
			src = fmt.Sprintf(
				"package pkg%d\n\nimport \"example.com/chain/pkg%d\"\n\n// V returns pkg%d.V() + 1.\nfunc V() int { return pkg%d.V() + 1 }\n",
				i, i-1, i-1, i-1,
			)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "p.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("write pkg%d/p.go: %v", i, err)
		}
	}
}

// TestBuild_EvictionBoundsCacheSize builds a 20-package linear import chain
// and verifies that the shared type cache never grows anywhere close to the
// total package count: the reference-count eviction must keep evicting
// dependencies as soon as their sole importer finishes.
func TestBuild_EvictionBoundsCacheSize(t *testing.T) {
	const n = 20
	dir := t.TempDir()
	generateChainModule(t, dir, n)

	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	db := openTestDB(t)
	cas := openTestCAS(t)

	var mu sync.Mutex
	var evictions int
	var maxCacheLen int

	opts := Options{
		Parallelism: 1, // deterministic: one package in flight at a time
		onEvicted: func(_ string, cacheLen int) {
			mu.Lock()
			defer mu.Unlock()
			evictions++
			if cacheLen > maxCacheLen {
				maxCacheLen = cacheLen
			}
		},
	}

	stats, err := Build(context.Background(), snap, db, cas, &opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Processed != n {
		t.Fatalf("Processed = %d, want %d", stats.Processed, n)
	}

	// pkg1..pkg(n-1) each have exactly one importer, so each is evicted
	// once its importer finishes: n-1 evictions total.
	if evictions != n-1 {
		t.Errorf("evictions = %d, want %d", evictions, n-1)
	}
	// With Parallelism=1 and a linear chain, the cache holds at most the
	// package currently being checked plus its single direct dependency —
	// never anywhere near all n packages.
	if maxCacheLen > 2 {
		t.Errorf("maxCacheLen = %d, want <= 2 (n=%d packages processed)", maxCacheLen, n)
	}
}

// generateFanInModule writes a synthetic module of n independent root
// packages, pkg0..pkg(n-1), each importing "strconv" from the standard
// library (a non-root package by construction — it lies outside dir's own
// "./..." pattern), to dir. Every root package is otherwise leaf-level: none
// import each other, isolating "strconv"'s own non-root fan-in from the
// root-to-root eviction generateChainModule already covers.
func generateFanInModule(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/fanin\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	for i := 0; i < n; i++ {
		pkgDir := filepath.Join(dir, fmt.Sprintf("pkg%d", i))
		if err := os.MkdirAll(pkgDir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", pkgDir, err)
		}
		src := fmt.Sprintf(
			"package pkg%d\n\nimport \"strconv\"\n\n// V returns strconv.Itoa(%d).\nfunc V() string { return strconv.Itoa(%d) }\n",
			i, i, i,
		)
		if err := os.WriteFile(filepath.Join(pkgDir, "p.go"), []byte(src), 0o600); err != nil {
			t.Fatalf("write pkg%d/p.go: %v", i, err)
		}
	}
}

// TestBuild_NonRootEvictionBoundsCacheSize builds n independent root
// packages that all import the same non-root ("strconv") package and
// verifies the shared typecheck.Cache still evicts it: before
// computeNonRootFanIn (see scheduler.go), only a ROOT package's fan-in was
// ever ref-counted — typecheck.Importer.decode populates the same shared
// Cache for a non-root import through the identical decode path a root
// import uses, but nothing ever called Cache.Delete for one, so "strconv"'s
// decoded *types.Package would survive for Build's entire run once any
// package imported it, regardless of how many packages had since finished
// with it. This pins that a non-root import is evicted exactly once, after
// its last direct root importer finishes — the same reference-counted
// contract root dependencies already had.
func TestBuild_NonRootEvictionBoundsCacheSize(t *testing.T) {
	const n = 20
	dir := t.TempDir()
	generateFanInModule(t, dir, n)

	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	db := openTestDB(t)
	cas := openTestCAS(t)

	var mu sync.Mutex
	var strconvEvictions int

	opts := Options{
		Parallelism: 4,
		onEvicted: func(pkgPath string, _ int) {
			if pkgPath != "strconv" {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			strconvEvictions++
		},
	}

	stats, err := Build(context.Background(), snap, db, cas, &opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Processed != n {
		t.Fatalf("Processed = %d, want %d", stats.Processed, n)
	}

	// Every one of the n root packages imports "strconv" directly, so its
	// fan-in counter reaches zero, and Cache.Delete("strconv") fires,
	// exactly once — after the LAST of them finishes, never once per
	// importer and never not at all.
	if strconvEvictions != 1 {
		t.Errorf(`onEvicted("strconv", ...) called %d times, want exactly 1`, strconvEvictions)
	}
}
