package depcheck

import (
	"context"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
)

// loadGenericSplitGraph loads testdata/genericsplit, the fixture mirroring
// github.com/stephenafamo/bob's Mod[T]/mods.Set[T] shape (see outer/outer.go),
// as a real *graph.Snapshot.
func loadGenericSplitGraph(t *testing.T) MetadataSource {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "genericsplit"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	return NewGraphMetadataSource(snap)
}

// TestProvider_ClosureScope_PreventsGenericTypeIdentitySplit is the
// regression pin for a generics-heavy indexing failure reported against
// packages shaped like github.com/stephenafamo/bob: testdata/genericsplit/
// outer/outer.go compiles cleanly under the real toolchain (see the
// fixture's own go.mod) -- `var _ bobpkg.Mod[shared.C] = modspkg.MakeDefault()`
// type-checks because modspkg.Set[T]'s pointer-receiver Apply(T) satisfies
// bobpkg.Mod[T] for any T, including shared.C.
//
// Before closureScope existed, forcing the declarations-only LRU capacity to
// 1 reproduced a false type-check failure here: resolving "outer"'s own
// imports evicted "shared" from the LRU BETWEEN two points in the SAME
// recursive check -- modspkg's own nested check resolved "shared" first (to
// type modspkg.MakeDefault's return type *Set[shared.C]), got evicted by
// modspkg's own entry being cached, and outer's own direct import of
// "shared" then re-checked it from scratch, producing a second,
// non-identical *types.Named for the same declaration. go/types compares
// named types by object identity for generic instantiation/interface
// satisfaction, so `Mod[shared.C]` and `Set[shared.C]` disagreed on which
// shared.C they meant even though both came from the same, unmodified
// source -- exactly the "widely-shared dependency gets evicted and
// re-checked from scratch partway through resolving cp's own transitive
// imports" mechanism internal/depexport's own checkAndPersist doc already
// names, but without that doc's confirmed decode-panic (the blob still
// round-trips fine; the corruption is silent).
//
// closureScope's fix: every top-level Provider.Package/PackageWithBodies
// call pins its own transitive closure's resolved packages for that call's
// whole duration, so the shared LRU can still evict "shared" to make room
// for an unrelated, concurrently in-flight closure without this closure
// ever re-resolving it. Cap=1 here (the smallest possible, guaranteeing the
// LRU evicts "shared" the moment ANY other package is cached, well below
// what a real workspace's RecommendedCap sizing would allow before a much
// wider closure hit the same thrash) is the sharpest test of that pin: if it
// holds even here, it holds under any real Cap sizing too.
func TestProvider_ClosureScope_PreventsGenericTypeIdentitySplit(t *testing.T) {
	meta := loadGenericSplitGraph(t)
	p := NewProvider(meta, Options{Cap: 1})
	ctx := context.Background()

	const outerPkgPath = "example.com/genericsplit/outer"
	cp, err := p.Package(ctx, outerPkgPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", outerPkgPath, err)
	}

	if cp.Incomplete() {
		t.Fatalf("Package(%s).Incomplete() = true with Cap=1, want false: "+
			"outer.go's `var _ bobpkg.Mod[shared.C] = modspkg.MakeDefault()` compiles under the real toolchain "+
			"(see testdata/genericsplit/go.mod); an Incomplete result here means closureScope failed to prevent "+
			"the LRU-eviction-mid-closure identity split this test guards against", outerPkgPath)
	}

	// Confirm WHY it's complete, not just that it is: within outer's OWN
	// checked Info, the var decl's LHS type argument (bobpkg.Mod[shared.C],
	// resolved via outer's own direct "shared" import) and RHS type argument
	// (modspkg.MakeDefault()'s return type, resolved via modspkg's own
	// nested "shared" import, cached mid-closure) must be the exact SAME
	// *types.Named object -- not merely types.Identical, but pointer-equal,
	// since closureScope's whole point is that this closure's SECOND request
	// for "shared" reuses its FIRST resolution rather than re-checking it.
	lhsType, rhsType := genericSplitVarDeclTypes(t, cp)
	lhsNamed, ok := lhsType.(*types.Named)
	if !ok || lhsNamed.TypeArgs().Len() != 1 {
		t.Fatalf("LHS type %v is not an instantiated named type with one type argument", lhsType)
	}
	rhsPtr, ok := rhsType.(*types.Pointer)
	if !ok {
		t.Fatalf("RHS type %v is not a pointer type", rhsType)
	}
	rhsNamed, ok := rhsPtr.Elem().(*types.Named)
	if !ok || rhsNamed.TypeArgs().Len() != 1 {
		t.Fatalf("RHS pointer elem %v is not an instantiated named type with one type argument", rhsPtr.Elem())
	}
	lhsC, rhsC := lhsNamed.TypeArgs().At(0), rhsNamed.TypeArgs().At(0)
	if lhsC != rhsC {
		t.Errorf("LHS bobpkg.Mod[shared.C] type-arg shared.C = %p, RHS modspkg.Set[shared.C] type-arg shared.C = %p: "+
			"want the identical object (closureScope should have reused one shared.C resolution for this whole closure)",
			lhsC, rhsC)
	}
}

// TestProvider_LRU_PreventsCrossCallGenericTypeIdentitySplit is the
// cross-call counterpart to TestProvider_ClosureScope_PreventsGenericTypeIdentitySplit:
// closureScope only protects a single top-level Package/PackageWithBodies
// call's own in-flight resolutions (#116). Once that call returns, "shared"
// has no protection left except the LRU's own dependency-based pin (see
// lru.go's put doc) that modspkg's own cached entry holds on it for as long
// as modspkg itself stays cached. Delete exercises that pin directly and
// deterministically: it shares evictOldest's exact pin-checking code (see
// lruCache.delete), so a pinned entry surviving a Delete call proves the
// same invariant a capacity-driven eviction would need, without depending
// on incidental LRU ordering to force one.
func TestProvider_LRU_PreventsCrossCallGenericTypeIdentitySplit(t *testing.T) {
	meta := loadGenericSplitGraph(t)
	p := NewProvider(meta, Options{Cap: 10})
	ctx := context.Background()

	const (
		sharedPkgPath  = "example.com/genericsplit/shared"
		modspkgPkgPath = "example.com/genericsplit/modspkg"
		outerPkgPath   = "example.com/genericsplit/outer"
	)

	if _, err := p.Package(ctx, modspkgPkgPath); err != nil {
		t.Fatalf("Package(%s): %v", modspkgPkgPath, err)
	}
	checkedAfterModspkg := p.Checked()

	p.Delete(sharedPkgPath)
	if _, ok := p.lru.get(sharedPkgPath); !ok {
		t.Fatal("Delete(shared) removed a pinned entry: modspkg's own cached-lifetime dependency claim on shared should have deferred this")
	}

	cp, err := p.Package(ctx, outerPkgPath)
	if err != nil {
		t.Fatalf("Package(%s): %v", outerPkgPath, err)
	}
	if cp.Incomplete() {
		t.Fatalf("Package(%s).Incomplete() = true, want false: outer.go compiles under the real toolchain; "+
			"an Incomplete result here means the LRU's dependency-based pin failed to prevent a cross-call identity split", outerPkgPath)
	}

	// +2, not +4: outer and bobpkg are freshly checked (never cached before),
	// but shared and modspkg must be served from cache, not re-checked.
	if got, want := p.Checked(), checkedAfterModspkg+2; got != want {
		t.Errorf("Checked() = %d after resolving outer, want %d: shared and modspkg should both have been served from cache, not re-checked", got, want)
	}

	lhsType, rhsType := genericSplitVarDeclTypes(t, cp)
	lhsNamed, ok := lhsType.(*types.Named)
	if !ok || lhsNamed.TypeArgs().Len() != 1 {
		t.Fatalf("LHS type %v is not an instantiated named type with one type argument", lhsType)
	}
	rhsPtr, ok := rhsType.(*types.Pointer)
	if !ok {
		t.Fatalf("RHS type %v is not a pointer type", rhsType)
	}
	rhsNamed, ok := rhsPtr.Elem().(*types.Named)
	if !ok || rhsNamed.TypeArgs().Len() != 1 {
		t.Fatalf("RHS pointer elem %v is not an instantiated named type with one type argument", rhsPtr.Elem())
	}
	lhsC, rhsC := lhsNamed.TypeArgs().At(0), rhsNamed.TypeArgs().At(0)
	if lhsC != rhsC {
		t.Errorf("LHS bobpkg.Mod[shared.C] type-arg shared.C = %p, RHS modspkg.Set[shared.C] type-arg shared.C = %p: "+
			"want the identical object (the LRU's dependency-based pin should have reused modspkg's own shared resolution across this second, separate call)",
			lhsC, rhsC)
	}
}

// genericSplitVarDeclTypes returns the LHS declared type and RHS value type
// of outer.go's single `var _ bobpkg.Mod[shared.C] = modspkg.MakeDefault()`
// declaration.
func genericSplitVarDeclTypes(t *testing.T, cp *CheckedPackage) (lhs, rhs types.Type) {
	t.Helper()
	file := cp.Files()[0]
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) != 1 {
				continue
			}
			lhs = cp.Info().Types[vs.Type].Type
			rhs = cp.Info().Types[vs.Values[0]].Type
		}
	}
	if lhs == nil || rhs == nil {
		t.Fatal("could not locate outer.go's var decl in cp.Info().Types")
	}
	return lhs, rhs
}
