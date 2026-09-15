package index

import (
	"bytes"
	"context"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/store"
	"github.com/sivchari/golance/internal/typecheck"
)

// rootExportBlob fetches pkgPath's current [store.UnitBlob] via db and cas,
// returning its BlobKey and Export bytes for a test to assert on.
func rootExportBlob(t *testing.T, db *store.DB, cas *store.CAS, pkgPath string) (blobKey uint64, export []byte) {
	t.Helper()
	ptr, err := db.GetUnit(context.Background(), store.Hash(pkgPath))
	if err != nil {
		t.Fatalf("GetUnit(%s): %v", pkgPath, err)
	}
	blob, ok, err := cas.Get(context.Background(), ptr.BlobKey)
	if err != nil {
		t.Fatalf("cas.Get(%s): %v", pkgPath, err)
	}
	if !ok {
		t.Fatalf("cas.Get(%s): no blob for recorded BlobKey", pkgPath)
	}
	u, err := store.DecodeUnitBlob(blob)
	if err != nil {
		t.Fatalf("DecodeUnitBlob(%s): %v", pkgPath, err)
	}
	return ptr.BlobKey, u.Export
}

// TestBuild_PersistsDeclarationOnlyExportBlobForRootPackage verifies that
// Build writes a gcexportdata-decodable, declarations-only export blob for
// every root package it type-checks, keyed by the same content-addressed
// BlobKey processUnit already computes for facts (see checkAndStoreOutcome):
// leaf.Hello and leaf.Greeting, both exported, must be visible in the
// decoded *types.Package without leaf's own real source ever being read
// again.
func TestBuild_PersistsDeclarationOnlyExportBlobForRootPackage(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	stats, err := Build(ctx, snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.TypeChecked != 3 {
		t.Fatalf("first Build TypeChecked = %d, want 3 (leaf, mid, top all fresh)", stats.TypeChecked)
	}

	_, export := rootExportBlob(t, db, cas, pkgLeaf)
	if len(export) == 0 {
		t.Fatal("leaf's persisted UnitBlob.Export is empty, want a gcexportdata blob")
	}

	pkg, err := typecheck.ReadExport(export, token.NewFileSet(), pkgLeaf, typecheck.NewCache())
	if err != nil {
		t.Fatalf("decode leaf's persisted export data: %v", err)
	}
	if pkg.Scope().Lookup("Hello") == nil {
		t.Error(`decoded leaf export data has no "Hello" declaration`)
	}
	if pkg.Scope().Lookup("Greeting") == nil {
		t.Error(`decoded leaf export data has no "Greeting" declaration`)
	}
}

// TestBuild_CASRestoredPackageWritesNoFreshExportBlob verifies the other
// half of export-data persistence: a package Build restores from an
// existing CAS entry (its recomputed combined key matches a blob some
// EARLIER run already wrote, without matching what db currently has
// recorded — the "switching back to a previously-visited branch" case
// processUnit's own doc describes) is never re-type-checked and never gets
// a fresh cas.Put — only checkAndStoreOutcome, reached on a genuine miss,
// ever writes a blob. This matters because export data must only ever come
// from a package THIS run source-checked (see this package's own corruption
// class elsewhere): a decode-restored blob must be reused byte-for-byte,
// never regenerated.
//
// leaf is edited (an exported decl added, changing its own content AND
// export hash) and rebuilt, then reverted back to its original content and
// rebuilt again: leaf's combined key on the third build recomputes back to
// its FIRST build's key, which is still cached in cas from that build, so
// leaf (and, transitively, mid and top, whose own combined keys fold in
// leaf's export hash) all take the CAS-hit path on the third build, not the
// type-check path.
func TestBuild_CASRestoredPackageWritesNoFreshExportBlob(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	origKey, origExport := rootExportBlob(t, db, cas, pkgLeaf)

	leafPath := filepath.Join(dir, "leaf", "leaf.go")
	original, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("read leaf.go: %v", err)
	}
	edited := append(bytes.Clone(original), []byte("\n// Extra is a temporary exported declaration.\nfunc Extra() int { return 1 }\n")...)
	if err := os.WriteFile(leafPath, edited, 0o600); err != nil {
		t.Fatalf("edit leaf.go: %v", err)
	}

	snap = loadSnapshot(t, dir)
	stats2, err := Build(ctx, snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if stats2.TypeChecked != 3 {
		t.Fatalf("second Build TypeChecked = %d, want 3 (leaf's export hash change forces mid and top to rebuild too)", stats2.TypeChecked)
	}
	editedKey, _ := rootExportBlob(t, db, cas, pkgLeaf)
	if editedKey == origKey {
		t.Fatal("leaf's BlobKey did not change after editing its exported API")
	}

	if err := os.WriteFile(leafPath, original, 0o600); err != nil {
		t.Fatalf("revert leaf.go: %v", err)
	}
	snap = loadSnapshot(t, dir)
	stats3, err := Build(ctx, snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("third Build: %v", err)
	}
	if stats3.TypeChecked != 0 {
		t.Errorf("third Build TypeChecked = %d, want 0 (leaf, mid, and top must all restore from cas, none re-type-checked)", stats3.TypeChecked)
	}
	if stats3.Processed != 3 {
		t.Errorf("third Build Processed = %d, want 3 (none of the three stat-match db's post-edit recording, so all three must at least re-derive their combined key)", stats3.Processed)
	}

	restoredKey, restoredExport := rootExportBlob(t, db, cas, pkgLeaf)
	if restoredKey != origKey {
		t.Errorf("leaf's BlobKey after reverting = %d, want the original build's key %d", restoredKey, origKey)
	}
	if !bytes.Equal(restoredExport, origExport) {
		t.Error("leaf's restored Export bytes differ from the original build's — the CAS-hit path must reuse the original blob byte-for-byte, not regenerate one")
	}
}
