package typecheck_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/sivchari/golance/internal/typecheck"
)

// The three packages below mirror internal/depcheck/testdata/genericsplit
// (github.com/stephenafamo/bob's Mod[T]/mods.Set[T] shape): sharedSrc is a
// plain concrete type, modspkgSrc's MakeDefault returns a generic type
// instantiated against it, and bobpkgSrc's Mod[T] interface is satisfied by
// modspkgSrc's Set[T] for any T. consumerSrc mirrors testdata/genericsplit's
// own outer.go: `var _ bobpkg.Mod[shared.C] = modspkg.MakeDefault()`
// type-checks under the real toolchain only if both sides agree on exactly
// which shared.C they mean.
const (
	genericSplitSharedPath  = "example.com/genericsplit/shared"
	genericSplitBobpkgPath  = "example.com/genericsplit/bobpkg"
	genericSplitModspkgPath = "example.com/genericsplit/modspkg"

	genericSplitSharedSrc = `package shared

type C struct{}
`
	genericSplitBobpkgSrc = `package bobpkg

type Mod[T any] interface {
	Apply(T) error
}
`
	genericSplitModspkgSrc = `package modspkg

import "example.com/genericsplit/shared"

type Set[T any] struct{}

func (s *Set[T]) Apply(T) error { return nil }

func MakeDefault() *Set[shared.C] { return &Set[shared.C]{} }
`
	genericSplitConsumerSrc = `package consumer

import (
	"example.com/genericsplit/bobpkg"
	"example.com/genericsplit/modspkg"
	"example.com/genericsplit/shared"
)

var _ bobpkg.Mod[shared.C] = modspkg.MakeDefault()
`
)

// buildGenericSplitBlobs type-checks shared/bobpkg/modspkg from source (a
// throwaway fset/importer, independent of the fset/Cache under test below —
// mirroring a real indexer run's own dependencies, whose export data was
// produced by an entirely separate check) and returns each one's WriteExport
// blob, keyed by import path.
func buildGenericSplitBlobs(t *testing.T) map[string][]byte {
	t.Helper()
	fset := token.NewFileSet()

	sharedPkg := mustCheckSource(t, fset, genericSplitSharedPath, genericSplitSharedSrc, nil)
	sharedBlob, err := typecheck.WriteExport(sharedPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport(shared): %v", err)
	}

	bobpkgPkg := mustCheckSource(t, fset, genericSplitBobpkgPath, genericSplitBobpkgSrc, nil)
	bobpkgBlob, err := typecheck.WriteExport(bobpkgPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport(bobpkg): %v", err)
	}

	modspkgImp := typecheck.NewImporter(fset, nil, blobSource{blobs: map[string][]byte{genericSplitSharedPath: sharedBlob}}, typecheck.NewCache())
	modspkgPkg := mustCheckSource(t, fset, genericSplitModspkgPath, genericSplitModspkgSrc, modspkgImp)
	modspkgBlob, err := typecheck.WriteExport(modspkgPkg, fset)
	if err != nil {
		t.Fatalf("WriteExport(modspkg): %v", err)
	}

	return map[string][]byte{
		genericSplitSharedPath:  sharedBlob,
		genericSplitBobpkgPath:  bobpkgBlob,
		genericSplitModspkgPath: modspkgBlob,
	}
}

// mustCheckSource parses and type-checks src as pkgPath, failing the test on
// any error (shared/bobpkg/modspkg all compile cleanly under the real
// toolchain — see the fixture's own doc).
func mustCheckSource(t *testing.T, fset *token.FileSet, pkgPath, src string, imp types.Importer) *types.Package {
	t.Helper()
	f, err := parser.ParseFile(fset, pkgPath+".go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", pkgPath, err)
	}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(pkgPath, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("check %s: %v", pkgPath, err)
	}
	return pkg
}

// sharedFromImports returns pkg's own reference to the "shared" package
// among its Imports() list (see CheckScope's doc for why decoding pkg
// necessarily resolved it, even though pkg never asked for it as its own
// top-level ImportFrom argument).
func sharedFromImports(t *testing.T, pkg *types.Package) *types.Package {
	t.Helper()
	for _, imp := range pkg.Imports() {
		if imp.Path() == genericSplitSharedPath {
			return imp
		}
	}
	t.Fatalf("%s.Imports() does not include %s", pkg.Path(), genericSplitSharedPath)
	return nil
}

// TestCheckScope_PreventsMidCheckEvictionSplit is the regression pin for the
// indexer-side counterpart to #116's internal/depcheck fix: internal/index's
// scheduler (computeNonRootFanIn) evicts a shared, non-root dependency from
// the shared typecheck.Cache once every root package that DIRECTLY imports
// it has finished — accounting with no visibility at all into a dependency
// only ever reached TRANSITIVELY, through another already-decoded package's
// own export data (see CheckScope's doc for the exact gcexportdata
// mechanism). Without CheckScope pinning modspkg's own reference to "shared"
// for this whole check's duration, a concurrently-finishing, unrelated root
// package's own Cache.Delete("shared") landing between this check's
// decode(modspkg) and its own later direct ImportFrom("shared") reproduces
// exactly the identity split TestProvider_ClosureScope_PreventsGenericTypeIdentitySplit
// guards against inside internal/depcheck — except here nothing needs LRU
// capacity tuning or goroutine timing to force it: Cache.Delete is called
// directly, deterministically, at the exact moment that mid-check eviction
// would land in production.
func TestCheckScope_PreventsMidCheckEvictionSplit(t *testing.T) {
	blobs := buildGenericSplitBlobs(t)
	fset := token.NewFileSet()
	cache := typecheck.NewCache()
	imp := typecheck.NewImporter(fset, nil, blobSource{blobs: blobs}, cache)

	scope := imp.NewCheck()

	// modspkg's own blob references "shared" internally (MakeDefault's
	// return type); decoding it creates/completes "shared"'s entry in the
	// shared Cache as a side effect, without this scope ever asking for
	// "shared" as its own top-level ImportFrom argument yet.
	modspkgPkg, err := scope.ImportFrom(genericSplitModspkgPath, "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(modspkg): %v", err)
	}

	// Simulate a DIFFERENT, concurrently-finishing root package whose last
	// direct importer of "shared" just completed: internal/index/scheduler.go's
	// finish() would call exactly this.
	cache.Delete(genericSplitSharedPath)

	if got := cache.Len(); got == 0 {
		t.Fatalf("Cache.Len() = 0 after Delete(shared) while modspkg's own check is still pinning it: "+
			"want the deferred-delete entry to still be present (got %d)", got)
	}

	// This scope's own later, direct resolution of "shared" must reuse the
	// exact same instance modspkg's own decoded types already reference —
	// not decode a second, non-identical *types.Package for it.
	sharedDirect, err := scope.ImportFrom(genericSplitSharedPath, "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(shared): %v", err)
	}
	sharedViaModspkg := sharedFromImports(t, modspkgPkg)
	if sharedViaModspkg != sharedDirect {
		t.Fatalf("shared.C's home package split within one check: modspkg's embedded shared package = %p, "+
			"this scope's own direct shared resolution = %p — want the identical object", sharedViaModspkg, sharedDirect)
	}

	bobpkgPkg, err := scope.ImportFrom(genericSplitBobpkgPath, "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(bobpkg): %v", err)
	}

	// End to end: a real consumer package's var decl, checked through the
	// same scope, must type-check cleanly and agree on shared.C's identity
	// on both sides — mirroring
	// TestProvider_ClosureScope_PreventsGenericTypeIdentitySplit's own
	// stronger, pointer-identity assertion (not just "no error").
	consumerPkg, lhsC, rhsC := checkGenericSplitConsumer(t, fset, scope, modspkgPkg, bobpkgPkg, sharedDirect)
	if consumerPkg == nil {
		t.Fatal("check(consumer) produced no package")
	}
	if lhsC != rhsC {
		t.Errorf("consumer's bobpkg.Mod[shared.C] type-arg = %p, modspkg.Set[shared.C] type-arg = %p: want the identical object", lhsC, rhsC)
	}

	// cache holds exactly shared/bobpkg/modspkg at this point (consumer's
	// own check resolved no package beyond these three) — before Close,
	// "shared" is still counted despite the Delete above (deferred, see
	// Cache.Delete's doc).
	before := cache.Len()
	if before != 3 {
		t.Fatalf("Cache.Len() = %d before Close(), want 3 (shared, bobpkg, modspkg)", before)
	}

	scope.Close()

	// Close releases only scope's own IN-FLIGHT pins. "shared" stays
	// cached regardless: modspkg's own decode claimed it on modspkg's own
	// CACHED-LIFETIME behalf (see Cache.claimLocked's doc) the moment
	// modspkg was decoded, independent of any CheckScope — the deferred
	// Delete from earlier still cannot apply while modspkg itself, which
	// transitively embeds "shared", remains cached.
	if after := cache.Len(); after != before {
		t.Errorf("Cache.Len() = %d after Close(), want %d: modspkg's own cached-lifetime claim on shared must survive scope.Close()", after, before)
	}

	// Only once modspkg ITSELF is removed (releasing modspkg's own claim
	// on shared) does shared's earlier-deferred Delete finally apply.
	cache.Delete(genericSplitModspkgPath)
	if got, want := cache.Len(), before-2; got != want {
		t.Errorf("Cache.Len() = %d after deleting modspkg, want %d: removing modspkg should release its claim on shared, applying shared's own deferred Delete too", got, want)
	}
}

// checkGenericSplitConsumer type-checks genericSplitConsumerSrc through
// scope (so its own bobpkg/modspkg/shared imports resolve against the exact
// instances already pinned above) and returns the checked package plus the
// LHS (bobpkg.Mod[shared.C]) and RHS (modspkg.MakeDefault()'s result)
// declarations' own shared.C type arguments.
func checkGenericSplitConsumer(t *testing.T, fset *token.FileSet, scope *typecheck.CheckScope, modspkgPkg, bobpkgPkg, sharedPkg *types.Package) (pkg *types.Package, lhsC, rhsC types.Type) {
	t.Helper()
	f, err := parser.ParseFile(fset, "consumer.go", genericSplitConsumerSrc, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse consumer.go: %v", err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	conf := types.Config{Importer: scope}
	pkg, err = conf.Check("example.com/genericsplit/consumer", fset, []*ast.File{f}, info)
	if err != nil {
		t.Fatalf("check consumer: %v", err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) != 1 {
				continue
			}
			lhsC = typeArgOf(t, info.Types[vs.Type].Type)
			rhsC = typeArgOf(t, info.Types[vs.Values[0]].Type)
		}
	}
	if lhsC == nil || rhsC == nil {
		t.Fatal("could not locate consumer.go's var decl types")
	}
	return pkg, lhsC, rhsC
}

// typeArgOf returns t's own single generic type argument, unwrapping one
// pointer indirection first if present (RHS's *Set[shared.C] vs LHS's plain
// Mod[shared.C]).
func typeArgOf(t *testing.T, typ types.Type) types.Type {
	t.Helper()
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	named, ok := typ.(*types.Named)
	if !ok || named.TypeArgs().Len() != 1 {
		t.Fatalf("%v is not an instantiated named type with one type argument", typ)
	}
	return named.TypeArgs().At(0)
}

// TestImporter_UnscopedDecodeStillProtectsCachedDependencyClaims is the
// regression anchor for TestCheckScope_PreventsMidCheckEvictionSplit under
// the CACHED-LIFETIME half of Cache's protection: decode's own claimLocked
// call (see its doc) pins pkg.Imports() on pkg's own cached-lifetime
// behalf unconditionally, inside decode itself, regardless of whether the
// caller goes through a CheckScope at all — so even bypassing CheckScope
// entirely and calling Importer.ImportFrom directly (the way production
// code did before CheckScope existed, and still a supported, exported
// entry point for callers that don't need per-CALL identity protection,
// e.g. internal/check/recheck.go) no longer reproduces the split
// TestCheckScope_PreventsMidCheckEvictionSplit guards against: modspkg's
// own decode already claims "shared" for as long as modspkg itself stays
// cached, before Cache.Delete(shared) is even called.
func TestImporter_UnscopedDecodeStillProtectsCachedDependencyClaims(t *testing.T) {
	blobs := buildGenericSplitBlobs(t)
	fset := token.NewFileSet()
	cache := typecheck.NewCache()
	imp := typecheck.NewImporter(fset, nil, blobSource{blobs: blobs}, cache)

	modspkgPkg, err := imp.ImportFrom(genericSplitModspkgPath, "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(modspkg): %v", err)
	}

	// Simulate a DIFFERENT, concurrently-finishing root package whose last
	// direct importer of "shared" just completed: unlike before
	// claimLocked existed, this is now deferred — modspkg's own decode
	// already pinned "shared" on modspkg's own behalf.
	cache.Delete(genericSplitSharedPath)
	if got := cache.Len(); got != 2 {
		t.Fatalf("Cache.Len() = %d after Delete(shared), want 2 (modspkg still claims shared): modspkg's own claim should have deferred this Delete", got)
	}

	sharedDirect, err := imp.ImportFrom(genericSplitSharedPath, "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(shared): %v", err)
	}
	sharedViaModspkg := sharedFromImports(t, modspkgPkg)
	if sharedViaModspkg != sharedDirect {
		t.Fatalf("shared.C's home package split despite modspkg's own cached-lifetime claim: modspkg's embedded shared package = %p, "+
			"a fresh direct resolution after Delete = %p — want the identical object", sharedViaModspkg, sharedDirect)
	}
}
