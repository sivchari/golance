package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
)

// danglingBlobPath mirrors (*store.CAS)'s own on-disk blob sharding (two
// hex digits of the key as a subdirectory, the rest as the file name — see
// store's CAS doc) so a test can remove one specific blob file directly:
// simulating exactly what a [store.CAS.GC] pass racing an incomplete mark
// set leaves behind — a [store.UnitPointer] whose BlobKey names a blob that
// no longer exists.
func danglingBlobPath(casDir string, key uint64) string {
	hex := fmt.Sprintf("%016x", key)
	return filepath.Join(casDir, hex[:2], hex[2:]+".blob")
}

// TestProcessUnit_DanglingBlobKeyIsReprocessed pins the fix for the
// production bug where a package's recorded [store.UnitPointer] still
// matched db's own bookkeeping (its content and dependencies were
// untouched) but the blob that BlobKey named had gone missing from the
// CAS: PackageChanged, RevalidateStale and Build's own unchanged fast path
// all used to conclude "unchanged" from the matching key alone, so the
// dangling pointer was never repaired and every read of the package kept
// failing forever. This verifies the repair now happens end to end: the
// staleness checks notice it, and a follow-up Build rewrites the blob under
// the same key.
func TestProcessUnit_DanglingBlobKeyIsReprocessed(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	casDir := filepath.Join(t.TempDir(), "cas")
	cas, err := store.OpenCAS(casDir)
	if err != nil {
		t.Fatalf("store.OpenCAS: %v", err)
	}
	ctx := context.Background()

	dropLeafBlob(ctx, t, snap, db, cas, casDir)

	assertPackageChanged(ctx, t, snap, db, true,
		"PackageChanged() = false, want true for a BlobKey whose blob is missing from the CAS")

	pkgs, wholeDBStale, err := RevalidateStale(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("RevalidateStale: %v", err)
	}
	if wholeDBStale {
		t.Error("RevalidateStale() wholeDBStale = true, want false")
	}
	if want := []string{pkgLeaf}; !reflect.DeepEqual(pkgs, want) {
		t.Errorf("RevalidateStale() pkgs = %v, want %v", pkgs, want)
	}

	stats, err := Build(ctx, snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("Build (repair): %v", err)
	}
	if stats.TypeChecked == 0 {
		t.Error("Build (repair) stats.TypeChecked = 0, want leaf to have been genuinely reprocessed, not skipped or served from a stale CAS hit")
	}

	assertLeafBlobReadable(ctx, t, db, cas)

	assertPackageChanged(ctx, t, snap, db, false, "PackageChanged() = true after repair, want false")
}

// dropLeafBlob builds the index, points db at casDir the way production
// does (see blobCAS's doc on why PackageChanged/Revalidate need this), then
// deletes pkgLeaf's blob file directly: simulating exactly what a
// [store.CAS.GC] pass racing an incomplete mark set leaves behind.
func dropLeafBlob(ctx context.Context, t *testing.T, snap *graph.Snapshot, db *store.DB, cas *store.CAS, casDir string) {
	t.Helper()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := db.PutCASDir(casDir); err != nil {
		t.Fatalf("PutCASDir: %v", err)
	}

	before, err := db.GetUnit(ctx, store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf): %v", err)
	}
	if !cas.Has(before.BlobKey) {
		t.Fatalf("leaf's blob %x missing from CAS right after Build; test setup is broken", before.BlobKey)
	}
	if err := os.Remove(danglingBlobPath(casDir, before.BlobKey)); err != nil {
		t.Fatalf("remove leaf's blob: %v", err)
	}
}

// assertPackageChanged fails the test unless PackageChanged(pkgLeaf) reports
// want.
func assertPackageChanged(ctx context.Context, t *testing.T, snap *graph.Snapshot, db *store.DB, want bool, msg string) {
	t.Helper()

	changed, err := PackageChanged(ctx, snap, db, pkgLeaf, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("PackageChanged: %v", err)
	}
	if changed != want {
		t.Error(msg)
	}
}

// assertLeafBlobReadable fails the test unless pkgLeaf's current blob is
// both recorded in db and actually readable back out of cas.
func assertLeafBlobReadable(ctx context.Context, t *testing.T, db *store.DB, cas *store.CAS) {
	t.Helper()

	after, err := db.GetUnit(ctx, store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf) after repair: %v", err)
	}
	if !cas.Has(after.BlobKey) {
		t.Fatalf("blob %x missing from CAS after repair Build", after.BlobKey)
	}
	if _, ok, err := cas.Get(ctx, after.BlobKey); err != nil || !ok {
		t.Fatalf("CAS.Get(leaf) after repair = ok=%v err=%v, want a readable blob", ok, err)
	}
}
