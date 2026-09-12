package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
)

// This file is the permanent regression suite for audit-navigation.md's
// finding 1 (an external "_test"-suffixed test package's files are never
// indexed at all) at the facts-index level: it verifies Build now schedules
// and type-checks such a package as its own unit (schedulableRoot,
// isExternalTestOfRoot), with its own outbound references recorded in the
// reverse posting index, without disturbing the base package's own unit.

// writeExternalTestModule writes a module with one directory ("foo")
// declaring a base package, an in-package _test.go file, and an external
// "_test"-suffixed test package file that imports the base package by its
// real import path — the shape isExternalTestOfRoot recognizes.
func writeExternalTestModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mustWrite("go.mod", "module example.com/exttestmod\n\ngo 1.23\n")
	mustWrite("foo/foo.go", "package foo\n\n// Symbol returns a constant used by tests.\nfunc Symbol() int { return 42 }\n")
	mustWrite("foo/foo_test.go", "package foo\n\nimport \"testing\"\n\nfunc TestInPackage(t *testing.T) {\n\tif Symbol() != 42 {\n\t\tt.Fatal(\"Symbol() != 42\")\n\t}\n}\n")
	mustWrite("foo/foo_ext_test.go", "package foo_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/exttestmod/foo\"\n)\n\nfunc TestExternal(t *testing.T) {\n\tif foo.Symbol() != 42 {\n\t\tt.Fatal(\"foo.Symbol() != 42\")\n\t}\n}\n")
	return dir
}

// findExternalTestPkgPath returns the pkgPath of basePkgPath's external
// "_test"-suffixed test package in snap (isExternalTestOfRoot), failing t
// if none is found — used instead of hardcoding go/packages' own PkgPath
// naming convention for it, so this test does not depend on that detail.
func findExternalTestPkgPath(t *testing.T, snap *graph.Snapshot, basePkgPath string) string {
	t.Helper()
	for path, pkg := range snap.Packages {
		if isExternalTestOfRoot(snap, pkg) && pkg.ForTest == basePkgPath {
			return path
		}
	}
	t.Fatalf("no external test package of %s found in snapshot", basePkgPath)
	return ""
}

// TestBuild_ExternalTestPackageGetsOwnUnit verifies that Build type-checks
// a directory's external "_test" package as its own facts-index unit,
// distinct from the base package's, and records its outbound reference to
// the base package's exported symbol in the reverse posting index — the
// property textDocument/references and rename both depend on (see
// internal/xref.Resolver.locationsForAll/postingsFor).
func TestBuild_ExternalTestPackageGetsOwnUnit(t *testing.T) {
	dir := writeExternalTestModule(t)
	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	const pkgFoo = "example.com/exttestmod/foo"
	pkgFooTest := findExternalTestPkgPath(t, snap, pkgFoo)

	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("Build: %d errors", stats.Errors)
	}
	if stats.Processed != 2 {
		t.Errorf("Processed = %d, want 2 (foo, foo_test)", stats.Processed)
	}

	checkExternalTestUnitFiles(t, db, cas, pkgFooTest)
	checkBaseUnitFiles(t, db, cas, pkgFoo)
	checkExternalTestPosting(t, db, cas, pkgFoo, pkgFooTest)
}

// checkExternalTestUnitFiles verifies pkgFooTest's own unit contains
// exactly its own file, not foo.go or foo_test.go too (testFilesInPackage's
// guard: no duplicate/foreign files folded in).
func checkExternalTestUnitFiles(t *testing.T, db *store.DB, cas *store.CAS, pkgFooTest string) {
	t.Helper()
	viewFacts(t, db, cas, pkgFooTest, func(v *store.View) {
		if v.FileCount() != 1 {
			t.Fatalf("external test unit FileCount = %d, want 1", v.FileCount())
		}
		f, err := v.FileAt(0)
		if err != nil {
			t.Fatalf("FileAt(0): %v", err)
		}
		if filepath.Base(f) != "foo_ext_test.go" {
			t.Errorf("external test unit's file = %s, want foo_ext_test.go", f)
		}
	})
}

// checkBaseUnitFiles verifies pkgFoo's own unit is unaffected by the
// external test package's existence: its own file plus the in-package test
// file, never the external test file.
func checkBaseUnitFiles(t *testing.T, db *store.DB, cas *store.CAS, pkgFoo string) {
	t.Helper()
	viewFacts(t, db, cas, pkgFoo, func(v *store.View) {
		var names []string
		for i := 0; i < v.FileCount(); i++ {
			f, err := v.FileAt(i)
			if err != nil {
				t.Fatalf("FileAt(%d): %v", i, err)
			}
			names = append(names, filepath.Base(f))
		}
		if len(names) != 2 {
			t.Fatalf("base unit files = %v, want [foo.go foo_test.go]", names)
		}
		for _, n := range names {
			if n == "foo_ext_test.go" {
				t.Errorf("base unit incorrectly includes the external test file: %v", names)
			}
		}
	})
}

// checkExternalTestPosting verifies the external test package's own
// outbound reference to foo.Symbol is recorded in the reverse posting
// index, keyed by foo's PkgHash and Symbol's IDHash — exactly what
// internal/xref.Resolver.References and Rename read from to discover a use
// in the external test file.
func checkExternalTestPosting(t *testing.T, db *store.DB, cas *store.CAS, pkgFoo, pkgFooTest string) {
	t.Helper()
	symbolIDHash := findSymbolByName(t, db, cas, pkgFoo, "Symbol")
	recs, err := db.PostingsFor(context.Background(), store.Hash(pkgFoo), symbolIDHash)
	if err != nil {
		t.Fatalf("PostingsFor: %v", err)
	}
	found := false
	for _, rec := range recs {
		if rec.SrcPkgHash == store.Hash(pkgFooTest) {
			found = true
			if len(rec.Locations) != 1 {
				t.Errorf("external test package's posting has %d locations, want 1: %+v", len(rec.Locations), rec.Locations)
			}
		}
	}
	if !found {
		t.Errorf("no posting recorded from the external test package (%s) referencing foo.Symbol; postings: %+v", pkgFooTest, recs)
	}
}

// TestBuild_ExternalTestPackage_ReindexPropagatesFromBase verifies that
// Reindex, driven off the base package's own path (as a didSave on foo.go
// would be), reprocesses the external test package too (via
// graph.Snapshot.ClosureUnits' reverse-dependency closure, which already
// includes it unconditionally — see graph.go's newSnapshot), keeping its
// facts current without a separate save on the external test file itself.
func TestBuild_ExternalTestPackage_ReindexPropagatesFromBase(t *testing.T) {
	dir := writeExternalTestModule(t)
	snap, err := graph.Load(graph.Options{Dir: dir}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	const pkgFoo = "example.com/exttestmod/foo"
	pkgFooTest := findExternalTestPkgPath(t, snap, pkgFoo)

	db := openTestDB(t)
	cas := openTestCAS(t)
	if _, err := Build(context.Background(), snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Add a new exported symbol to foo.go -- an export-data-changing edit,
	// unlike a body-only change (which Reindex's own early-cutoff behavior
	// deliberately does NOT propagate downstream, since it cannot change
	// what any importer, including the external test package, observes) --
	// and reindex from the base package, exactly as a didSave on foo.go
	// would.
	fooGo := filepath.Join(dir, "foo", "foo.go")
	fooSrc := "package foo\n\n// Symbol returns a constant used by tests.\nfunc Symbol() int { return 42 }\n\n// Extra is a new exported symbol, changing foo's export data.\nfunc Extra() int { return 1 }\n"
	if err := os.WriteFile(fooGo, []byte(fooSrc), 0o600); err != nil {
		t.Fatalf("rewrite foo.go: %v", err)
	}
	stats, err := Reindex(context.Background(), snap, db, cas, pkgFoo, readFileDisk, &Options{})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("Reindex: %d errors", stats.Errors)
	}

	changed := map[string]bool{}
	for _, p := range stats.Changed {
		changed[p] = true
	}
	if !changed[pkgFooTest] {
		t.Errorf("Reindex(%s).Changed = %v, want it to include the external test package %s", pkgFoo, stats.Changed, pkgFooTest)
	}
}
