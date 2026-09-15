package index

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
)

// TestBuild_LayeredTestOnlyCycleIndexesCleanly reproduces the released
// v0.7.4 approvalagency/universal failure this test pins the fix for:
// index: dependency <A> of <B> has no recorded blob key (processed out of
// order?), reported by internal/index/unit.go's directDepExports.
//
// testdata/layeredcycle mirrors the real shape: usecase and infrastructure
// have zero production (non-test) imports between them, but usecase's own
// in-package _test.go file imports infrastructure and infrastructure's own
// in-package _test.go file imports usecase back — a mutual test-only import
// cycle only legal across the test/production split (see
// graph.Package.TestImports's own doc). service, server, and cmd then
// depend on usecase/infrastructure (and each other) purely through real,
// one-directional production imports, mirroring approvalagency's own
// service/server/cmd-plugin/cmd-connect cluster.
//
// Before the fix (graph.Snapshot.Before plus directDepImports' filtering by
// it, plus topoOrder's second Imports-only pass — see their own docs),
// topoOrder's cycle fallback dumped every package downstream of the cycle
// into one lexicographically-ordered tail with no regard for their real
// dependency edges, so unit.go's directDepExports asked keys.get for a
// dependency not yet processed this run and failed outright — not just for
// usecase/infrastructure themselves, but for every real dependent of them
// too (service, server, cmd).
func TestBuild_LayeredTestOnlyCycleIndexesCleanly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("testdata", "layeredcycle"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Errorf("stats.Errors = %d, want 0", stats.Errors)
	}

	const (
		pkgDomain         = "example.com/layeredcycle/domain"
		pkgUsecase        = "example.com/layeredcycle/usecase"
		pkgInfrastructure = "example.com/layeredcycle/infrastructure"
		pkgService        = "example.com/layeredcycle/service"
		pkgServer         = "example.com/layeredcycle/server"
		pkgCmd            = "example.com/layeredcycle/cmd"
	)
	for _, pkg := range []string{pkgDomain, pkgUsecase, pkgInfrastructure, pkgService, pkgServer, pkgCmd} {
		if _, err := db.GetUnit(context.Background(), store.Hash(pkg)); err != nil {
			t.Errorf("GetUnit(%s): %v (every package in the cluster must still be indexed)", pkg, err)
		}
	}

	// The real, one-directional production chain (cmd -> server -> service
	// -> {usecase, infrastructure}) must resolve with full fidelity despite
	// being downstream of the test-only cycle: each hop's reference to the
	// symbol it actually calls resolves to that symbol's own SymbolID.
	assertRefResolves(t, db, cas, pkgServer, pkgService, "New")
	assertRefResolves(t, db, cas, pkgService, pkgUsecase, "NewInteractor")
	assertRefResolves(t, db, cas, pkgService, pkgInfrastructure, "Repository")
	assertRefResolves(t, db, cas, pkgCmd, pkgServer, "NewHandler")
}

// assertRefResolves fails t unless fromPkg's facts blob records an outbound
// reference resolving to symbolName's SymbolID in toPkg.
func assertRefResolves(t *testing.T, db *store.DB, cas *store.CAS, fromPkg, toPkg, symbolName string) {
	t.Helper()
	symID := findSymbolByName(t, db, cas, toPkg, symbolName)
	var found bool
	viewFacts(t, db, cas, fromPkg, func(v *store.View) {
		for _, r := range v.RefsTo(symID) {
			if r.ToPkgHash() == store.Hash(toPkg) {
				found = true
			}
		}
	})
	if !found {
		t.Errorf("%s has no ref resolving to %s.%s's SymbolID", fromPkg, toPkg, symbolName)
	}
}
