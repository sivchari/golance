package index

import (
	"context"
	"go/token"
	"os"
	"testing"

	"github.com/sivchari/golance/internal/depcheck"
	"github.com/sivchari/golance/internal/depexport"
	"github.com/sivchari/golance/internal/typecheck"
)

// TestCASExportSource_WithheldExportIsReportedAsMiss pins the contract
// casExportSource.ExportData's own doc describes: Put with nil bytes
// (checkAndStoreOutcome's exact call when writeAndValidateExport withholds
// pkgPath's export — see checkOnePackage's own doc) must be reported as a
// miss, never a hit with empty data — otherwise Importer.resolve decodes
// zero bytes and fails outright instead of falling through to its fallback
// tier.
func TestCASExportSource_WithheldExportIsReportedAsMiss(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	cas := openTestCAS(t)
	keys := newKeyTable(ctx, db)
	exp := newCASExportSource(ctx, cas, keys)

	exp.Put("example.com/withheld", nil)

	blob, ok, err := exp.ExportData("example.com/withheld")
	if err != nil {
		t.Fatalf("ExportData: %v", err)
	}
	if ok {
		t.Errorf("ExportData(withheld) = (%d byte(s), ok=true), want ok=false: a withheld export must never look like a resolvable hit", len(blob))
	}
}

// TestCheckOnePackage_WithheldRootExportFallsBackToDeclarationCheck proves
// the cascade-softening this run's own casExportSource fix exists for, end
// to end: when a direct dependency's compiled export was withheld this run
// (its own round-trip check failed — see checkOnePackage's doc), a package
// importing it still gets a complete, usable answer via
// internal/depexport.Cache's declaration-only source-check fallback
// (depcheck.GraphMetadataSource resolves a root package exactly like a
// stdlib/module one — see its own doc), instead of every dependent
// hard-failing on an empty blob.
func TestCheckOnePackage_WithheldRootExportFallsBackToDeclarationCheck(t *testing.T) {
	snap := loadTestSnapshot(t) // testdata/module: leaf, mid (imports leaf), top (imports both)

	ctx := context.Background()
	db := openTestDB(t)
	cas := openTestCAS(t)
	keys := newKeyTable(ctx, db)
	exp := newCASExportSource(ctx, cas, keys)
	fset := token.NewFileSet()
	readFile := func(path string) ([]byte, error) { return os.ReadFile(path) }

	// Build leaf and mid normally first, exactly as Build's own scheduler
	// would (dependency order), populating exp the way checkAndStoreOutcome
	// does on success.
	buildImp := typecheck.NewImporter(fset, exp, nil, typecheck.NewCache())

	leafPkg, ok := snap.Package(pkgLeaf)
	if !ok {
		t.Fatalf("package %s not found in snapshot", pkgLeaf)
	}
	leafResult, err := checkOnePackage(fset, buildImp, pkgLeaf, leafPkg.GoFiles, nil, readFile, "", false)
	if err != nil {
		t.Fatalf("checkOnePackage(leaf): %v", err)
	}
	exp.Put(pkgLeaf, leafResult.Export)

	midPkg, ok := snap.Package(pkgMid)
	if !ok {
		t.Fatalf("package %s not found in snapshot", pkgMid)
	}
	midResult, err := checkOnePackage(fset, buildImp, pkgMid, midPkg.GoFiles, nil, readFile, "", false)
	if err != nil {
		t.Fatalf("checkOnePackage(mid): %v", err)
	}
	exp.Put(pkgMid, midResult.Export)

	// Simulate leaf's compiled export failing its own round-trip check —
	// checkAndStoreOutcome's exact Put on that path (see checkOnePackage's
	// doc).
	exp.Put(pkgLeaf, nil)

	depMeta := depcheck.NewGraphMetadataSource(snap)
	depProvider := depcheck.NewProvider(depMeta, depcheck.Options{})
	depExp := depexport.NewCache(nil, depMeta, depProvider, depexport.Options{})

	// A fresh Cache forces top to re-resolve leaf/mid through exp/depExp
	// rather than reuse buildImp's own already-decoded entries — otherwise
	// this test would never actually exercise the fallback path at all.
	topImp := typecheck.NewImporter(fset, exp, depExp, typecheck.NewCache())

	topPkg, ok := snap.Package(pkgTop)
	if !ok {
		t.Fatalf("package %s not found in snapshot", pkgTop)
	}
	topResult, err := checkOnePackage(fset, topImp, pkgTop, topPkg.GoFiles, nil, readFile, "", false)
	if err != nil {
		t.Fatalf("checkOnePackage(top): %v", err)
	}

	if topResult.Incomplete {
		t.Errorf("top.Incomplete = true, want false: leaf's declaration-only fallback check should have resolved it cleanly; first error: %s", topResult.FirstError)
	}
	if len(topResult.Export) == 0 {
		t.Error("top.Export is empty, want a produced export")
	}
}
