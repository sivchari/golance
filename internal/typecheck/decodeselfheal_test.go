package typecheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

type selfHealBlobSource struct{ blobs map[string][]byte }

func (s selfHealBlobSource) ExportData(pkgPath string) ([]byte, bool, error) {
	b, ok := s.blobs[pkgPath]
	return b, ok, nil
}

func mustCheckSelfHealSrc(t *testing.T, fset *token.FileSet, pkgPath, src string, imp types.Importer) *types.Package {
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

// TestImporter_DecodeSelfHealsPoisonedCacheEntry is the regression pin for
// decode's own condemn-and-heal response to a gcexportdata.Read failure
// (see decode's own doc). The poison is a bare *types.Package{} inserted
// directly into the shared cache under "shared"'s own path: Complete() is
// false (satisfying gcexportdata.Read's own documented contract, "imports
// [path] does not exist, or exists but is incomplete"), but its internal
// scope is nil, standing in for whatever partially-constructed state a
// real production corruption (a torn decode, a stale generation left
// behind by a since-fixed race) leaves in the shared cache — the class of
// bug the corpus surfaced as a plain "typecheck: decode export data for X:
// internal error ... nil pointer dereference" with no depexport round-trip
// prefix, meaning the blob itself was fine and only the shared decode
// CONTEXT was not.
func TestImporter_DecodeSelfHealsPoisonedCacheEntry(t *testing.T) {
	const sharedPath = "example.com/selfheal/shared"
	const consumerPath = "example.com/selfheal/consumer"

	fset0 := token.NewFileSet()
	sharedPkg := mustCheckSelfHealSrc(t, fset0, sharedPath, "package shared\n\ntype C struct{ V int }\n", nil)
	sharedBlob, err := WriteExport(sharedPkg, fset0)
	if err != nil {
		t.Fatalf("WriteExport(shared): %v", err)
	}

	// Prove the poison is real before relying on it: a direct ReadExport
	// against a cache already holding the broken entry fails cleanly
	// (gcexportdata's own iimportCommon recover converts the nil-scope
	// panic into a returned error — decode never sees a raw panic here).
	poisonedCache := NewCache()
	poisonedCache.mu.Lock()
	poisonedCache.pkgs[sharedPath] = new(types.Package)
	poisonedCache.mu.Unlock()
	if _, err := ReadExport(sharedBlob, token.NewFileSet(), sharedPath, poisonedCache); err == nil {
		t.Fatal("ReadExport against the poisoned cache unexpectedly succeeded — this test's own poison is not reproducing a real failure")
	} else {
		t.Logf("confirmed poison: a direct decode against it fails as expected: %v", err)
	}

	// Exercise the real production path: imp's own shared cache starts
	// with the identical poison.
	fset := token.NewFileSet()
	cache := NewCache()
	cache.mu.Lock()
	cache.pkgs[sharedPath] = new(types.Package)
	cache.mu.Unlock()
	imp := NewImporter(fset, nil, selfHealBlobSource{blobs: map[string][]byte{sharedPath: sharedBlob}}, cache)

	if got := cache.Resets(); got != 0 {
		t.Fatalf("cache.Resets() = %d before any decode, want 0", got)
	}
	scope := imp.NewCheck()
	pkg, err := scope.ImportFrom(sharedPath, "", 0)
	if err != nil {
		t.Fatalf("ImportFrom(%s) = %v, want the self-heal to make this succeed", sharedPath, err)
	}
	if got := cache.Resets(); got != 1 {
		t.Errorf("cache.Resets() = %d after the self-healed decode, want exactly 1", got)
	}
	if !pkg.Complete() || pkg.Scope().Lookup("C") == nil {
		t.Fatalf("ImportFrom(%s) returned a package with no usable C: %v", sharedPath, pkg)
	}

	// The final type-check: a consumer package importing "shared" through
	// the SAME, now-healed imp/cache completes with zero errors.
	f, err := parser.ParseFile(fset, "consumer.go", "package consumer\n\nimport \"example.com/selfheal/shared\"\n\nvar V shared.C\n", parser.ParseComments)
	if err != nil {
		t.Fatalf("parse consumer.go: %v", err)
	}
	_, _, errs := CheckPackage(fset, []*ast.File{f}, consumerPath, scope)
	scope.Close()
	if len(errs) != 0 {
		t.Errorf("CheckPackage(consumer) reported %d error(s), want 0: %v", len(errs), errs)
	}
}

// TestImporter_DecodeSelfHealSkipsPinnedEntries pins the safety guarantee
// resetUnpinnedLocked's own doc argues for: a poisoned entry still held
// live by another in-flight CheckScope is left untouched rather than
// discarded, so the self-heal can never manufacture a second, non-
// identical instance of something a live check has already embedded (the
// exact split #116 and CheckScope's own doc describe). The retry then
// fails identically against the still-broken entry, surfacing the error
// exactly as before the self-heal existed — never silently serving a
// broken result, and never splitting identity to "fix" it.
func TestImporter_DecodeSelfHealSkipsPinnedEntries(t *testing.T) {
	const sharedPath = "example.com/selfheal2/shared"

	fset0 := token.NewFileSet()
	sharedPkg := mustCheckSelfHealSrc(t, fset0, sharedPath, "package shared\n\ntype C struct{ V int }\n", nil)
	sharedBlob, err := WriteExport(sharedPkg, fset0)
	if err != nil {
		t.Fatalf("WriteExport(shared): %v", err)
	}

	fset := token.NewFileSet()
	cache := NewCache()
	cache.mu.Lock()
	cache.pkgs[sharedPath] = new(types.Package)
	cache.pins[sharedPath] = 1 // simulates a different, still-active CheckScope holding this entry live
	cache.mu.Unlock()
	imp := NewImporter(fset, nil, selfHealBlobSource{blobs: map[string][]byte{sharedPath: sharedBlob}}, cache)

	if _, err := imp.ImportFrom(sharedPath, "", 0); err == nil {
		t.Fatal("ImportFrom succeeded despite the poisoned entry staying pinned — resetUnpinnedLocked must never clear a pinned entry")
	} else {
		t.Logf("correctly surfaced the error instead of clearing a pinned, poisoned entry: %v", err)
	}
	if got := cache.Resets(); got != 1 {
		t.Errorf("cache.Resets() = %d, want 1: the reset attempt still runs (harmlessly discarding nothing, since the only entry is pinned)", got)
	}
}
