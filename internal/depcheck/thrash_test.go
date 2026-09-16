package depcheck

import (
	"context"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sivchari/golance/internal/typecheck"
)

// fakeExportSource is a minimal, in-memory stand-in for
// internal/depexport.Cache, sufficient to exercise
// Provider.SetExportSource's own contract without needing depexport (which
// would need to import depcheck's own real MetadataSource machinery just
// for a test). Mirrors depexport.Cache.ExportDataComplete's real shape
// exactly, including the reentrant call back into the SAME Provider on a
// miss (internal/server's real wiring: depexport.Cache's own provider field
// is the identical *depcheck.Provider it is installed on) — this is
// deliberate, not a simplification, since that reentrancy is exactly what
// production exercises and what surfaced the "unsafe" panic ImportFrom now
// guards against (see its own doc).
type fakeExportSource struct {
	provider *Provider

	mu    sync.Mutex
	blobs map[string][]byte
}

func newFakeExportSource(provider *Provider) *fakeExportSource {
	return &fakeExportSource{provider: provider, blobs: make(map[string][]byte)}
}

func (f *fakeExportSource) ExportDataComplete(pkgPath string) (data []byte, complete, ok bool, err error) {
	f.mu.Lock()
	if blob, hit := f.blobs[pkgPath]; hit {
		f.mu.Unlock()
		return blob, true, true, nil
	}
	f.mu.Unlock()

	cp, err := f.provider.Package(context.Background(), pkgPath)
	if err != nil {
		return nil, false, false, err
	}
	blob, err := typecheck.WriteExport(cp.Types(), f.provider.FileSet())
	if err != nil {
		return nil, false, false, err
	}
	complete = !cp.Incomplete()
	if complete {
		f.mu.Lock()
		f.blobs[pkgPath] = blob
		f.mu.Unlock()
	}
	return blob, complete, true, nil
}

// The star fixture: one root package blank-importing numLeaves small leaf
// packages, each of which imports every one of numFat "fat" packages (each
// declaring nDeclsPerFat exported funcs, standing in for a large generated
// API client), references the first fat package's exported type once (so a
// check that touches a leaf actually resolves a real symbol from it, not
// just a blank import), and THEN imports numPrivPerLeaf trivial packages
// private to that one leaf (never imported by any other leaf).
//
// The private imports matter for reproducing real thrash, not just
// padding: every leaf touches the SAME fat packages, so under pure LRU
// eviction a fat package importED FIRST in each leaf would simply be
// bumped back to most-recently-used every time it is needed again next —
// it would never actually get evicted, no matter how small the cap, since
// nothing else is ever inserted between one leaf's use of it and the
// next's. Importing enough leaf-private packages AFTER the shared ones
// inserts that churn: by the time leaf i's own check finishes, its private
// imports have pushed the fat packages (and leaf i's own entry) out of a
// small-enough cap, so leaf i+1 misses on them again — matching the real
// regression's own mechanism (RecommendedCap's doc): "a widely-shared
// package... gets evicted and re-checked from scratch every time a new
// importer reaches it again".
const (
	starRootPkgPath = "test/star/root"
	starLeafPrefix  = "test/star/leaf"
	starFatPrefix   = "test/star/fat"
	starPrivPrefix  = "test/star/priv"
)

func starLeafPkgPath(i int) string  { return fmt.Sprintf("%s%d", starLeafPrefix, i) }
func starFatPkgPath(k int) string   { return fmt.Sprintf("%s%d", starFatPrefix, k) }
func starLeafFileName(i int) string { return fmt.Sprintf("leaf%d.go", i) }
func starFatFileName(k int) string  { return fmt.Sprintf("fat%d.go", k) }

func starPrivPkgPath(leaf, j int) string  { return fmt.Sprintf("%s%d_%d", starPrivPrefix, leaf, j) }
func starPrivFileName(leaf, j int) string { return fmt.Sprintf("priv%d_%d.go", leaf, j) }

func starLeafIndex(pkgPath string) (int, bool) {
	if !strings.HasPrefix(pkgPath, starLeafPrefix) {
		return 0, false
	}
	i, err := strconv.Atoi(strings.TrimPrefix(pkgPath, starLeafPrefix))
	return i, err == nil
}

func starFatIndex(pkgPath string) (int, bool) {
	if !strings.HasPrefix(pkgPath, starFatPrefix) {
		return 0, false
	}
	k, err := strconv.Atoi(strings.TrimPrefix(pkgPath, starFatPrefix))
	return k, err == nil
}

// starPrivIndex parses a private filler package's pkgPath back into (leaf,
// j), the inverse of starPrivPkgPath.
func starPrivIndex(pkgPath string) (leaf, j int, ok bool) {
	if !strings.HasPrefix(pkgPath, starPrivPrefix) {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(pkgPath, starPrivPrefix), "_", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	leaf, err1 := strconv.Atoi(parts[0])
	j, err2 := strconv.Atoi(parts[1])
	return leaf, j, err1 == nil && err2 == nil
}

// starMetadataSource serves the star fixture's package metadata, every
// file written by writeStarFixture into one shared directory.
type starMetadataSource struct {
	dir            string
	numLeaves      int
	numFat         int
	numPrivPerLeaf int
}

func (s starMetadataSource) Package(pkgPath string) (dir string, goFiles, imports []string, ok bool) {
	if pkgPath == starRootPkgPath {
		imps := make([]string, s.numLeaves)
		for i := range s.numLeaves {
			imps[i] = starLeafPkgPath(i)
		}
		return s.dir, []string{filepath.Join(s.dir, "root.go")}, imps, true
	}
	if i, isLeaf := starLeafIndex(pkgPath); isLeaf {
		if i >= s.numLeaves {
			return "", nil, nil, false
		}
		imps := make([]string, 0, s.numFat+s.numPrivPerLeaf)
		for k := range s.numFat {
			imps = append(imps, starFatPkgPath(k))
		}
		for j := range s.numPrivPerLeaf {
			imps = append(imps, starPrivPkgPath(i, j))
		}
		return s.dir, []string{filepath.Join(s.dir, starLeafFileName(i))}, imps, true
	}
	if k, isFat := starFatIndex(pkgPath); isFat {
		if k >= s.numFat {
			return "", nil, nil, false
		}
		return s.dir, []string{filepath.Join(s.dir, starFatFileName(k))}, nil, true
	}
	if leaf, j, isPriv := starPrivIndex(pkgPath); isPriv {
		if leaf >= s.numLeaves || j >= s.numPrivPerLeaf {
			return "", nil, nil, false
		}
		return s.dir, []string{filepath.Join(s.dir, starPrivFileName(leaf, j))}, nil, true
	}
	return "", nil, nil, false
}

// writeStarFixture writes the star fixture's real .go source files to dir,
// matching starMetadataSource's own view of the shape.
func writeStarFixture(t *testing.T, dir string, numLeaves, numFat, nDeclsPerFat, numPrivPerLeaf int) {
	t.Helper()

	for k := range numFat {
		var b strings.Builder
		fmt.Fprintf(&b, "package fat%d\n\ntype T struct{}\n\n", k)
		for d := range nDeclsPerFat {
			fmt.Fprintf(&b, "func F%d() {}\n", d)
		}
		path := filepath.Join(dir, starFatFileName(k))
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	for i := range numLeaves {
		for j := range numPrivPerLeaf {
			src := fmt.Sprintf("package priv%d_%d\n\nconst V = %d\n", i, j, j)
			path := filepath.Join(dir, starPrivFileName(i, j))
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}

		var b strings.Builder
		fmt.Fprintf(&b, "package leaf%d\n\nimport (\n", i)
		fmt.Fprintf(&b, "\t%q\n", starFatPkgPath(0))
		for k := 1; k < numFat; k++ {
			fmt.Fprintf(&b, "\t_ %q\n", starFatPkgPath(k))
		}
		for j := range numPrivPerLeaf {
			fmt.Fprintf(&b, "\t_ %q\n", starPrivPkgPath(i, j))
		}
		b.WriteString(")\n\nvar Ref fat0.T\n")
		path := filepath.Join(dir, starLeafFileName(i))
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	var b strings.Builder
	b.WriteString("package root\n\nimport (\n")
	for i := range numLeaves {
		fmt.Fprintf(&b, "\t_ %q\n", starLeafPkgPath(i))
	}
	b.WriteString(")\n")
	path := filepath.Join(dir, "root.go")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestProvider_TransitiveThrash reproduces the production regression
// measured against a real dependency closure (textDocument/definition on a
// heavy package: 3m0.9s, dominant_phase=engine.Get at 2m46s) with a
// synthetic one: a closure of numLeaves+numFat+1 distinct packages, only a
// handful of which (the fat ones) are big, every leaf sharing them. A
// declarations-only LRU capacity smaller than the closure lets p.lru evict a
// fat package after an early leaf finishes with it, so a later leaf's own
// ImportFrom("fat...") call misses the LRU — but closureScope (see
// ctxImporter's own doc) still resolves it to the exact SAME *CheckedPackage
// the earlier leaf got, because THIS one Package(root) call's own recursive
// resolution already pinned it: eviction only affects a DIFFERENT,
// concurrently in-flight closure's ability to reuse it, never this one's own
// identity or its own repeat-check cost. Before closureScope existed, a
// later leaf's miss forced a genuine re-parse/re-check from scratch — this
// test now pins that closureScope eliminates that thrash on its own, with or
// without a Provider.SetExportSource ExportSource configured (which remains
// a second, independent mechanism — see exportResolver's own doc — for the
// case a single closure's own width exceeds even what one Package call
// should pin, e.g. reused across many separate top-level calls).
func TestProvider_TransitiveThrash(t *testing.T) {
	const (
		numLeaves      = 200
		numFat         = 3
		nDeclsPerFat   = 1500
		numPrivPerLeaf = 12
	)
	// leaves + fat packages + root itself + every leaf's own private fillers.
	distinct := int64(numLeaves + numFat + 1 + numLeaves*numPrivPerLeaf)

	dir := t.TempDir()
	writeStarFixture(t, dir, numLeaves, numFat, nDeclsPerFat, numPrivPerLeaf)
	meta := starMetadataSource{dir: dir, numLeaves: numLeaves, numFat: numFat, numPrivPerLeaf: numPrivPerLeaf}
	ctx := context.Background()

	t.Run("cap exceeds closure: no thrash regardless of ExportSource", func(t *testing.T) {
		p := NewProvider(meta, Options{Cap: numLeaves + numFat + 10})
		start := time.Now()
		if _, err := p.Package(ctx, starRootPkgPath); err != nil {
			t.Fatalf("Package(root): %v", err)
		}
		elapsed := time.Since(start)
		t.Logf("checked=%d distinct=%d elapsed=%s", p.Checked(), distinct, elapsed)
		if p.Checked() > distinct {
			t.Errorf("Checked() = %d, want <= %d (the whole closure fits inside the cap, so nothing should ever be evicted and re-checked)", p.Checked(), distinct)
		}
	})

	t.Run("cap smaller than closure, no ExportSource: closureScope still eliminates thrash", func(t *testing.T) {
		p := NewProvider(meta, Options{Cap: 8})
		start := time.Now()
		if _, err := p.Package(ctx, starRootPkgPath); err != nil {
			t.Fatalf("Package(root): %v", err)
		}
		elapsed := time.Since(start)
		extra := p.Checked() - distinct
		t.Logf("checked=%d distinct=%d extra=%d elapsed=%s (no ExportSource configured)", p.Checked(), distinct, extra, elapsed)
		if extra != 0 {
			t.Errorf("Checked() - distinct = %d, want 0 (closureScope pins every package this one Package(root) call's own "+
				"transitive resolution touches, so p.lru evicting a fat package for an unrelated closure must never force "+
				"THIS closure's own later leaf to re-check it)", extra)
		}
	})

	t.Run("cap smaller than closure, with ExportSource: thrash eliminated", func(t *testing.T) {
		p := NewProvider(meta, Options{Cap: 8})
		p.SetExportSource(newFakeExportSource(p))
		start := time.Now()
		if _, err := p.Package(ctx, starRootPkgPath); err != nil {
			t.Fatalf("Package(root): %v", err)
		}
		elapsed := time.Since(start)
		t.Logf("checked=%d decoded=%d distinct=%d elapsed=%s", p.Checked(), p.Decoded(), distinct, elapsed)
		if p.Checked() > distinct+int64(numFat) {
			t.Errorf("Checked() = %d, want close to %d (near 1x: the decode fast path should absorb repeat transitive touches instead of re-checking from source)", p.Checked(), distinct)
		}
		if p.Decoded() == 0 {
			t.Error("Decoded() = 0, want > 0 (the decode fast path should have been used at least once)")
		}
		if elapsed > 2*time.Second {
			t.Errorf("elapsed = %s, want well under 2s (a generous bound the pre-fix, no-ExportSource case above should exceed by a wide margin)", elapsed)
		}
	})
}

// TestProvider_ExportSource_PropagatesIncompleteness verifies trap (c): a
// transitive import resolved via the decode fast path that is itself
// Incomplete (see fakeExportSource's own refusal to cache such a blob,
// mirroring depexport.Cache.checkAndPersist's identical persist guard)
// still makes the CURRENT check Incomplete too — exactly like resolving the
// same import via a full recursive check already does (see
// ctxImporter.importIncomplete's doc) — so a caller composing a further
// blob from this check (depexport.Cache) still refuses to persist it.
func TestProvider_ExportSource_PropagatesIncompleteness(t *testing.T) {
	dir := t.TempDir()
	writeIncompleteFixture(t, dir)
	meta := incompleteFixtureMetadataSource{dir: dir}
	p := NewProvider(meta, Options{})
	p.SetExportSource(newFakeExportSource(p))

	cp, err := p.Package(context.Background(), incompleteOuterPkgPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", incompleteOuterPkgPath, err)
	}
	if !cp.Incomplete() {
		t.Error("Incomplete() = false for a package transitively importing an Incomplete dependency resolved via the decode fast path, want true")
	}
}

// TestProvider_TransitiveDecodeIdentity_StableAcrossEvictions verifies
// identity safety (trap (a)): two independent top-level checks (leaf0,
// leaf1) that each transitively reference the SAME shared dependency
// (fat0.T) resolve it to the IDENTICAL *types.TypeName, even though leaf0
// and leaf1 themselves evict each other from a Cap-1 declarations-only LRU
// between the two calls — the decode fast path's own cache (exportResolver)
// is what keeps fat0's identity stable here, unlike the small LRU it
// replaces for a transitive import.
func TestProvider_TransitiveDecodeIdentity_StableAcrossEvictions(t *testing.T) {
	dir := t.TempDir()
	writeStarFixture(t, dir, 2, 1, 5, 0)
	meta := starMetadataSource{dir: dir, numLeaves: 2, numFat: 1}
	ctx := context.Background()

	p := NewProvider(meta, Options{Cap: 1})
	p.SetExportSource(newFakeExportSource(p))

	leaf0, err := p.Package(ctx, starLeafPkgPath(0))
	if err != nil {
		t.Fatalf("Package(leaf0): %v", err)
	}
	leaf1, err := p.Package(ctx, starLeafPkgPath(1))
	if err != nil {
		t.Fatalf("Package(leaf1): %v", err)
	}

	ref0 := leaf0.Types().Scope().Lookup("Ref")
	ref1 := leaf1.Types().Scope().Lookup("Ref")
	if ref0 == nil || ref1 == nil {
		t.Fatal("Ref not found in leaf0's or leaf1's checked package scope")
	}
	named0, ok := ref0.Type().(*types.Named)
	if !ok {
		t.Fatalf("leaf0.Ref type = %T, want *types.Named", ref0.Type())
	}
	named1, ok := ref1.Type().(*types.Named)
	if !ok {
		t.Fatalf("leaf1.Ref type = %T, want *types.Named", ref1.Type())
	}
	if named0.Obj() != named1.Obj() {
		t.Error("fat0.T identity diverged between leaf0's and leaf1's independent checks; the decode cache should keep it stable even though leaf0 and leaf1 evict each other from the small declarations-only LRU")
	}
}

// TestProvider_DirectRequestAfterTransitiveDecode_StillSourceChecked
// verifies trap (b): once a package has been resolved only via the decode
// fast path (as fat0 is here, reached transitively through leaf0), a later
// caller that requests it DIRECTLY (e.g. DependencyDefinition navigating
// into it) still gets a full, byte-exact-position source check — never the
// decode cache's own line-only-position instance — the same precision
// TestProvider_StdlibExactPosition already pins for the ordinary case.
func TestProvider_DirectRequestAfterTransitiveDecode_StillSourceChecked(t *testing.T) {
	dir := t.TempDir()
	writeStarFixture(t, dir, 2, 1, 5, 0)
	meta := starMetadataSource{dir: dir, numLeaves: 2, numFat: 1}
	ctx := context.Background()

	p := NewProvider(meta, Options{})
	p.SetExportSource(newFakeExportSource(p))

	if _, err := p.Package(ctx, starLeafPkgPath(0)); err != nil {
		t.Fatalf("Package(leaf0): %v", err)
	}

	fatCP, err := p.Package(ctx, starFatPkgPath(0))
	if err != nil {
		t.Fatalf("Package(fat0): %v", err)
	}
	if len(fatCP.Files()) == 0 {
		t.Fatal("Package(fat0) returned no parsed files; want a real source-checked AST even though fat0 was already resolved via the decode fast path")
	}

	obj := fatCP.Types().Scope().Lookup("T")
	if obj == nil {
		t.Fatal("fat0.T not found in the checked package scope")
	}
	got := p.FileSet().Position(obj.Pos())
	wantLine, wantCol := declIdentInFile(t, got.Filename, "T")
	if got.Line != wantLine || got.Column != wantCol {
		t.Errorf("fat0.T position = %d:%d, want %d:%d (byte-exact, from parsing %s directly)", got.Line, got.Column, wantLine, wantCol, got.Filename)
	}
}
