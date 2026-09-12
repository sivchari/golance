package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// TestBuild_DependencyReadFailureDoesNotPersistDependentWithStaleKey pins
// the fix for keyTable.get's H1 fallback bug: leaf's own file is deleted
// out from under an otherwise-valid graph.Snapshot right before Build runs
// — a failing-read seam standing in for a real facts-extraction/I-O error —
// so leaf itself cannot be resolved this run. Build schedules every root
// package in one pass regardless of any single package's outcome (see
// scheduler.finish's unconditional call), so mid and top — both of which
// import leaf, directly or transitively — are still attempted in the very
// same run leaf failed in. Neither may be silently rebuilt and persisted
// keyed against a stale (here: nonexistent) export hash for leaf; each must
// instead come out of this run as its own reported error, so nothing at all
// is left recorded in db for it — visibly missing rather than invisibly
// wrong — and a later run retries it.
func TestBuild_DependencyReadFailureDoesNotPersistDependentWithStaleKey(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if err := os.Remove(filepath.Join(dir, "leaf", "leaf.go")); err != nil {
		t.Fatalf("remove leaf.go: %v", err)
	}

	stats, err := Build(ctx, snap, db, cas, &Options{})
	if err != nil {
		t.Fatalf("Build: %v (a single package's own failure must not fail the whole run, see Build's doc)", err)
	}
	if stats.Errors != 3 {
		t.Errorf("stats.Errors = %d, want 3 (leaf, mid, top all unresolvable this run)", stats.Errors)
	}
	if stats.Processed != 0 {
		t.Errorf("stats.Processed = %d, want 0", stats.Processed)
	}

	for _, path := range []string{pkgLeaf, pkgMid, pkgTop} {
		if _, err := db.GetUnit(ctx, store.Hash(path)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetUnit(%s) = %v, want store.ErrNotFound: nothing should ever be persisted for a package that never resolved this run, whether directly (leaf) or via a dependency that failed to resolve (mid, top)", path, err)
		}
	}
}

// TestKeyTable_Get_FailedPathNeverFallsBackToDB pins keyTable's own
// invariant directly, at the narrowest possible seam: once fail has
// recorded that a path could not be resolved this run, get must report it
// unresolvable even though db still holds a perfectly well-formed
// UnitPointer for it from an earlier, successful run.
func TestKeyTable_Get_FailedPathNeverFallsBackToDB(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const path = "example.com/x/leaf"

	old := store.UnitPointer{BlobKey: 111, ContentHash: 222, ExportHash: 333, ToolchainFingerprint: "go1.26"}
	if err := db.PutUnit(&store.UnitEntry{PkgHash: store.Hash(path), Pointer: old}); err != nil {
		t.Fatalf("PutUnit: %v", err)
	}

	keys := newKeyTable(ctx, db)
	wantErr := errors.New("simulated resolution failure")
	keys.fail(path, wantErr)

	if _, ok := keys.get(path); ok {
		t.Error("get() ok = true after fail(), want false: a path this run could not resolve must never fall back to db's earlier-run record")
	}
	if got := keys.failure(path); !errors.Is(got, wantErr) {
		t.Errorf("failure() = %v, want %v", got, wantErr)
	}
}

// TestKeyTable_Get_UntouchedPathFallsBackToDBAndIsMemoized is
// TestKeyTable_Get_FailedPathNeverFallsBackToDB's companion: it pins that
// the legitimate cache-hit path — a path this run never touches at all —
// still resolves via db exactly as before, and that a resolved lookup is
// memoized rather than re-reading db on every subsequent call.
func TestKeyTable_Get_UntouchedPathFallsBackToDBAndIsMemoized(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const path = "example.com/x/leaf"

	original := store.UnitPointer{BlobKey: 111, ContentHash: 222, ExportHash: 333, ToolchainFingerprint: "go1.26"}
	if err := db.PutUnit(&store.UnitEntry{PkgHash: store.Hash(path), Pointer: original}); err != nil {
		t.Fatalf("PutUnit: %v", err)
	}

	keys := newKeyTable(ctx, db)

	rec, ok := keys.get(path)
	if !ok {
		t.Fatal("get() ok = false, want true: a path this run never touched should resolve via db's stable record")
	}
	if rec.blobKey != original.BlobKey || rec.exportHash != original.ExportHash {
		t.Errorf("get() = %+v, want blobKey=%d exportHash=%d from db", rec, original.BlobKey, original.ExportHash)
	}

	// Overwrite db's record for path: if the second get() call below
	// actually re-read db instead of serving its memoized result, it would
	// observe this change instead of the original one.
	changed := store.UnitPointer{BlobKey: 999, ContentHash: 888, ExportHash: 777, ToolchainFingerprint: "go1.26"}
	if err := db.PutUnit(&store.UnitEntry{PkgHash: store.Hash(path), Pointer: changed}); err != nil {
		t.Fatalf("PutUnit (overwrite): %v", err)
	}

	rec2, ok := keys.get(path)
	if !ok {
		t.Fatal("get() ok = false on second call, want true")
	}
	if rec2 != rec {
		t.Errorf("get() second call = %+v, want the memoized %+v: a re-read of db here would mean the cache-hit fast path was regressed into recomputing on every call", rec2, rec)
	}
}
