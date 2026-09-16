package xref

import (
	"context"
	"fmt"
	"go/types"
	"path/filepath"
	"testing"
)

// writeMultiIfaceModule writes a module rooted in t.TempDir() with numPkgs
// packages, each declaring its own single-method interface type (pkgN.Iface)
// under a distinct import path -- a minimal fixture for exercising
// Resolver.exportCache's decode cache directly via resolveNamed, one call
// per package, without any of Implementation/References' own candidate
// discovery machinery in the way.
func writeMultiIfaceModule(t *testing.T, numPkgs int) (root string, pkgPaths []string) {
	t.Helper()
	root = t.TempDir()
	writeTestFile(t, root, "go.mod", "module example.com/exportcache\n\ngo 1.23\n")
	pkgPaths = make([]string, numPkgs)
	for i := 0; i < numPkgs; i++ {
		name := fmt.Sprintf("pkg%d", i)
		writeTestFile(t, root, filepath.Join(name, name+".go"), fmt.Sprintf("package %s\n\ntype Iface interface {\n\tM() int\n}\n", name))
		pkgPaths[i] = "example.com/exportcache/" + name
	}
	return root, pkgPaths
}

// TestResolveNamed_ExportCacheReusedAcrossRepeatDecodes pins the decode
// cache's steady-state behavior under the default (generous) byte cap: once
// every package in pkgPaths has been decoded once, decoding all of them
// again performs zero additional gcexportdata.Read calls (see
// typecheck.Cache.Decodes' doc) and resolves to the identical *types.Named
// identity both times, the object-identity guarantee a decode-based
// types.Implements confirmation depends on (see package doc).
func TestResolveNamed_ExportCacheReusedAcrossRepeatDecodes(t *testing.T) {
	root, pkgPaths := writeMultiIfaceModule(t, 5)
	r, _ := newBenchResolver(t, root)
	ctx := context.Background()

	named := make(map[string]*types.Named, len(pkgPaths))
	for _, p := range pkgPaths {
		n, err := r.resolveNamed(ctx, p, "Iface")
		if err != nil {
			t.Fatalf("resolveNamed(%s) (first pass): %v", p, err)
		}
		named[p] = n
	}
	decodesAfterFirst := r.cache.Decodes()
	if decodesAfterFirst != int64(len(pkgPaths)) {
		t.Fatalf("cache.Decodes() after first pass = %d, want %d (one decode per distinct package)", decodesAfterFirst, len(pkgPaths))
	}

	for _, p := range pkgPaths {
		n, err := r.resolveNamed(ctx, p, "Iface")
		if err != nil {
			t.Fatalf("resolveNamed(%s) (repeat pass): %v", p, err)
		}
		if n != named[p] {
			t.Errorf("resolveNamed(%s) repeat identity = %p, want %p (same *types.Named object as the first decode)", p, n, named[p])
		}
	}
	decodesAfterRepeat := r.cache.Decodes()
	if decodesAfterRepeat != decodesAfterFirst {
		t.Errorf("repeat pass added %d decodes, want 0 (every package was already decoded and cached)", decodesAfterRepeat-decodesAfterFirst)
	}
}

// TestReferences_InterfaceMethod_ReusesDecodedExportAcrossRepeatQueries
// extends TestImplementation_ReusesDecodedUnitsAcrossCandidates' coverage to
// References on an interface method (correspondingMethodSymbols ->
// methodReceiver -> methodImplementationSymbols), the call chain the field
// report's phase timer singled out (dominant_phase=resolve, see
// correspondingMethodSymbols' doc): a repeat query performs zero additional
// gcexportdata decodes and returns the identical result set. Reuses
// TestReferences_InterfaceMethodIncludesDirectConcreteCallSite's own
// fixture, which already establishes the non-empty 2-result baseline this
// test's own decode-count assertions build on.
func TestReferences_InterfaceMethod_ReusesDecodedExportAcrossRepeatQueries(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "go.mod", "module example.com/refcorrcache\n\ngo 1.23\n")
	writeTestFile(t, dir, "iface/iface.go", `package iface

type Talker interface {
	Say() string
}
`)
	writeTestFile(t, dir, "impl/impl.go", `package impl

type Concrete struct{}

func (c Concrete) Say() string { return "hi" }
`)
	writeTestFile(t, dir, "use/use.go", `package use

import (
	"example.com/refcorrcache/iface"
	"example.com/refcorrcache/impl"
)

func CallInterface(t iface.Talker) string {
	return t.Say()
}

func CallConcrete() string {
	return impl.Concrete{}.Say()
}
`)

	r, snap := newResolverForDir(t, dir)
	ifaceFile := goFile(t, snap, "example.com/refcorrcache/iface", "iface.go")
	line, col := identOccurrence(t, ifaceFile, "Say")
	ctx := context.Background()

	first, err := r.References(ctx, ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References (first query): %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("References (first query) = %+v, want 2 (interface-typed call + concrete-typed call)", first)
	}
	decodesAfterFirst := r.cache.Decodes()
	if decodesAfterFirst == 0 {
		t.Errorf("cache.Decodes() after first query = 0, want > 0 (resolving Talker.Say's own receiver decodes iface's export data)")
	}

	second, err := r.References(ctx, ifaceFile, line, col, false)
	if err != nil {
		t.Fatalf("References (repeat query): %v", err)
	}
	decodesAfterRepeat := r.cache.Decodes()
	if decodesAfterRepeat != decodesAfterFirst {
		t.Errorf("repeat query added %d decodes, want 0 (every package it needs was already decoded by the first query)", decodesAfterRepeat-decodesAfterFirst)
	}
	if len(second) != len(first) {
		t.Errorf("repeat query returned %d results, want %d (identical to the first query)", len(second), len(first))
	}
}

// TestResolveNamed_ExportCacheEvictsAtByteCap is exportCache's own
// regression test, WithExportCacheBytes' whole reason for existing: a cap
// small enough to cross between individual packages' own decode sizes
// forces the coarse whole-pair reset exportCache's doc describes at least
// once per pass -- proving the cap actually bounds r.cache's growth instead
// of being dead configuration -- while resolveNamed still resolves
// correctly (a fresh *types.Named, not an error) after its own generation
// was discarded out from under it. (r.cache.Decodes() alone cannot pin this:
// each reset replaces r.cache with a fresh typecheck.Cache whose own
// Decodes() restarts at 0, so exportCacheResetCount is what this asserts
// against instead -- see its own doc.)
func TestResolveNamed_ExportCacheEvictsAtByteCap(t *testing.T) {
	root, pkgPaths := writeMultiIfaceModule(t, 5)

	// cap sits strictly between one package's own decoded size and two
	// packages' combined size (see writeMultiIfaceModule's fixed single-method
	// interface shape, probed at 144 bytes/package), so decoding every
	// SECOND package forces exportCache to discard the whole pair.
	const tinyCap = 200
	r, _ := newBenchResolver(t, root, WithExportCacheBytes(tinyCap))
	ctx := context.Background()

	for _, p := range pkgPaths {
		if _, err := r.resolveNamed(ctx, p, "Iface"); err != nil {
			t.Fatalf("resolveNamed(%s) (first pass): %v", p, err)
		}
	}
	resetsAfterFirst := r.exportCacheResetCount()
	if resetsAfterFirst == 0 {
		t.Fatalf("exportCacheResetCount() after first pass = 0, want > 0 (decoding %d packages of 144 bytes each under a %d-byte cap must cross it at least once)", len(pkgPaths), tinyCap)
	}

	for _, p := range pkgPaths {
		n, err := r.resolveNamed(ctx, p, "Iface")
		if err != nil {
			t.Fatalf("resolveNamed(%s) (repeat pass, after eviction): %v", p, err)
		}
		if n == nil || n.Obj() == nil || n.Obj().Name() != "Iface" {
			t.Errorf("resolveNamed(%s) repeat = %v, want a resolved Iface *types.Named even after its cache generation was evicted", p, n)
		}
	}
	resetsAfterRepeat := r.exportCacheResetCount()
	if resetsAfterRepeat <= resetsAfterFirst {
		t.Errorf("repeat pass under a %d-byte cap added %d resets, want > 0 (an evicted package should force at least one more reset on its next repeat decode)", tinyCap, resetsAfterRepeat-resetsAfterFirst)
	}
}
