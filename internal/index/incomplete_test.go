package index

import (
	"context"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// TestBuild_IncompletePackageReportedInStats verifies that a package whose
// type-check reports a real go/types error (but still produces a usable
// *types.Package -- see checkOnePackage's doc) is still indexed (no
// Stats.Errors, still gets a UnitPointer) but counted in Stats.Incomplete,
// so a degraded facts/export pair is no longer silently indistinguishable
// from a clean one. A sibling, well-typed package must not be counted.
func TestBuild_IncompletePackageReportedInStats(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/incompletemod\n\ngo 1.23\n")
	writeFile(t, dir, "broken/broken.go", `package broken

func F() int {
	return "not an int"
}
`)
	writeFile(t, dir, "clean/clean.go", `package clean

func G() int {
	return 1
}
`)

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)

	stats, err := Build(context.Background(), snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0 (a type error degrades the package, it does not fail it outright)", stats.Errors)
	}
	if stats.Processed != 2 {
		t.Errorf("Processed = %d, want 2", stats.Processed)
	}
	if stats.Incomplete != 1 {
		t.Errorf("Incomplete = %d, want 1 (only broken)", stats.Incomplete)
	}

	const brokenPkg = "example.com/incompletemod/broken"
	if _, err := db.GetUnit(context.Background(), store.Hash(brokenPkg)); err != nil {
		t.Errorf("GetUnit(%s): %v, want a UnitPointer despite the type error", brokenPkg, err)
	}
}
