package index

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sivchari/golance/internal/graph"
	"github.com/sivchari/golance/internal/store"
)

// loadTestSnapshotWithBuildFlags loads testdata/module with buildFlags
// forwarded to graph.Load, for tests that need two snapshots of the same
// byte-identical source distinguished only by their build configuration.
func loadTestSnapshotWithBuildFlags(t *testing.T, buildFlags []string) *graph.Snapshot {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "module"))
	if err != nil {
		t.Fatalf("abs testdata root: %v", err)
	}
	snap, err := graph.Load(graph.Options{Dir: root, BuildFlags: buildFlags}, "./...")
	if err != nil {
		t.Fatalf("graph.Load: %v", err)
	}
	return snap
}

// TestBuild_BuildFlagsFingerprintDefaultsFromSnapshot verifies H3's fix:
// Build, left to default Options.BuildFlagsFingerprint (every real
// production call site does — see that field's own doc), still invalidates
// a package's content hash across a build-flags change by reading the
// fingerprint from snap itself. Without this default, two Build calls
// against byte-identical source loaded under different BuildFlags would
// produce identical ContentHash/BlobKey values, and neither GOFLAGS nor
// GOOS/GOARCH/CGO_ENABLED changes would ever be able to invalidate the
// index.
func TestBuild_BuildFlagsFingerprintDefaultsFromSnapshot(t *testing.T) {
	snapPlain := loadTestSnapshotWithBuildFlags(t, nil)
	snapTagged := loadTestSnapshotWithBuildFlags(t, []string{"-tags=regressionh3"})
	ctx := context.Background()

	dbPlain, casPlain := openTestDB(t), openTestCAS(t)
	if _, err := Build(ctx, snapPlain, dbPlain, casPlain, &Options{}); err != nil {
		t.Fatalf("Build(snapPlain): %v", err)
	}
	dbTagged, casTagged := openTestDB(t), openTestCAS(t)
	if _, err := Build(ctx, snapTagged, dbTagged, casTagged, &Options{}); err != nil {
		t.Fatalf("Build(snapTagged): %v", err)
	}

	ptrPlain, err := dbPlain.GetUnit(ctx, store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf, plain): %v", err)
	}
	ptrTagged, err := dbTagged.GetUnit(ctx, store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf, tagged): %v", err)
	}

	if ptrPlain.ContentHash == ptrTagged.ContentHash {
		t.Error("ContentHash unchanged across a BuildFlags difference, want it to differ (Options.BuildFlagsFingerprint should default from snap.BuildFlagsFingerprint())")
	}
	if ptrPlain.BlobKey == ptrTagged.BlobKey {
		t.Error("BlobKey unchanged across a BuildFlags difference, want it to differ")
	}
}

// TestReindex_BuildFlagsFingerprintDefaultsFromSnapshot mirrors the Build
// test above for Reindex, the actual production entry point every
// didSave-triggered reindex goes through (see internal/server.reindex).
func TestReindex_BuildFlagsFingerprintDefaultsFromSnapshot(t *testing.T) {
	ctx := context.Background()

	snapPlain := loadTestSnapshotWithBuildFlags(t, nil)
	dbPlain, casPlain := openTestDB(t), openTestCAS(t)
	if _, err := Build(ctx, snapPlain, dbPlain, casPlain, &Options{}); err != nil {
		t.Fatalf("initial Build(snapPlain): %v", err)
	}
	if _, err := Reindex(ctx, snapPlain, dbPlain, casPlain, pkgLeaf, readFileDisk, &Options{}); err != nil {
		t.Fatalf("Reindex(snapPlain): %v", err)
	}

	snapTagged := loadTestSnapshotWithBuildFlags(t, []string{"-tags=regressionh3"})
	dbTagged, casTagged := openTestDB(t), openTestCAS(t)
	if _, err := Build(ctx, snapTagged, dbTagged, casTagged, &Options{}); err != nil {
		t.Fatalf("initial Build(snapTagged): %v", err)
	}
	if _, err := Reindex(ctx, snapTagged, dbTagged, casTagged, pkgLeaf, readFileDisk, &Options{}); err != nil {
		t.Fatalf("Reindex(snapTagged): %v", err)
	}

	ptrPlain, err := dbPlain.GetUnit(ctx, store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf, plain): %v", err)
	}
	ptrTagged, err := dbTagged.GetUnit(ctx, store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf, tagged): %v", err)
	}
	if ptrPlain.ContentHash == ptrTagged.ContentHash {
		t.Error("ContentHash unchanged across a BuildFlags difference after Reindex, want it to differ")
	}
}

// TestResolveBuildFlagsFingerprint verifies the small defaulting rule
// itself: an explicit fp always wins over snap's own, and an empty fp falls
// back to it.
func TestResolveBuildFlagsFingerprint(t *testing.T) {
	snap := loadTestSnapshotWithBuildFlags(t, []string{"-tags=regressionh3"})

	if got := resolveBuildFlagsFingerprint(snap, ""); got != snap.BuildFlagsFingerprint() {
		t.Errorf("resolveBuildFlagsFingerprint(snap, \"\") = %q, want snap.BuildFlagsFingerprint() = %q", got, snap.BuildFlagsFingerprint())
	}
	if got := resolveBuildFlagsFingerprint(snap, "pinned"); got != "pinned" {
		t.Errorf("resolveBuildFlagsFingerprint(snap, \"pinned\") = %q, want %q (explicit value must win)", got, "pinned")
	}
}
