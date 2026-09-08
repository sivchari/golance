package index

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/sivchari/golance/internal/store"
)

// TestRevalidate_NothingChanged verifies that a freshly built, untouched
// workspace reports no changes.
func TestRevalidate_NothingChanged(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	changed, err := Revalidate(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if changed {
		t.Error("Revalidate() = true, want false for an untouched workspace")
	}
}

// TestRevalidate_ContentChangeDetectedWithoutWriting verifies that
// Revalidate detects a real content change made outside of Build, and does
// not itself write anything to db while doing so.
func TestRevalidate_ContentChangeDetectedWithoutWriting(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	before, err := db.GetUnit(context.Background(), store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf): %v", err)
	}

	leafPath := filepath.Join(dir, "leaf", "leaf.go")
	edited := []byte("package leaf\n\n// Greeting is a friendly greeting.\ntype Greeting struct{ Message string }\n\n// Hello returns a Greeting for name.\nfunc Hello(name string) Greeting { return Greeting{Message: \"hi \" + name} }\n")
	if err := os.WriteFile(leafPath, edited, 0o600); err != nil {
		t.Fatalf("edit leaf.go: %v", err)
	}

	changed, err := Revalidate(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if !changed {
		t.Error("Revalidate() = false, want true after editing leaf.go's content")
	}

	after, err := db.GetUnit(context.Background(), store.Hash(pkgLeaf))
	if err != nil {
		t.Fatalf("GetUnit(leaf) after Revalidate: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("UnitPointer changed after Revalidate: before=%+v after=%+v (Revalidate must never write)", before, after)
	}
}

// TestRevalidate_ToolchainMismatchShortCircuits verifies that a database
// built under a different toolchain is reported as changed via the
// whole-database BuildFingerprint check, without needing to inspect any
// individual package.
func TestRevalidate_ToolchainMismatchShortCircuits(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{ToolchainFingerprint: "go1.0-fake"}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	changed, err := Revalidate(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if !changed {
		t.Error("Revalidate() = false, want true for a mismatched toolchain fingerprint")
	}
}

// TestRevalidate_NothingChangedWithInPackageTestFile verifies that a
// package with an in-package _test.go file reports no changes right after
// Build: without folding test files into the same effective file set
// processUnit itself indexes, this would previously mismatch db's stored
// [store.UnitPointer].Files/ContentHash (which cover the test file) against
// pkg.GoFiles alone (which does not), reporting spurious churn on every
// call.
func TestRevalidate_NothingChangedWithInPackageTestFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/revaltest\n\ngo 1.23\n")
	writeFile(t, dir, "pkg/pkg.go", "package pkg\n\n// V returns 1.\nfunc V() int { return 1 }\n")
	writeFile(t, dir, "pkg/pkg_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {\n\tif V() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	changed, err := Revalidate(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if changed {
		t.Error("Revalidate() = true, want false for an untouched package with an in-package test file")
	}
}

// TestRevalidate_InPackageTestFileContentChangeDetected verifies that
// Revalidate detects a content change to an in-package _test.go file made
// outside of Build, the same way TestRevalidate_ContentChangeDetectedWithoutWriting
// verifies it for an ordinary file.
func TestRevalidate_InPackageTestFileContentChangeDetected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/revaltest2\n\ngo 1.23\n")
	writeFile(t, dir, "pkg/pkg.go", "package pkg\n\n// V returns 1.\nfunc V() int { return 1 }\n")
	writeFile(t, dir, "pkg/pkg_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {\n\tif V() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")

	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	testPath := filepath.Join(dir, "pkg", "pkg_test.go")
	edited := []byte("package pkg\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {\n\tif V() != 2 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n")
	if err := os.WriteFile(testPath, edited, 0o600); err != nil {
		t.Fatalf("edit pkg_test.go: %v", err)
	}

	changed, err := Revalidate(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if !changed {
		t.Error("Revalidate() = false, want true after editing the in-package test file's content")
	}
}

// TestRevalidate_NewPackageNotYetInDB verifies that a package with no
// UnitPointer at all (never indexed) is reported as changed.
func TestRevalidate_NewPackageNotYetInDB(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	ctx := context.Background()

	changed, err := Revalidate(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if !changed {
		t.Error("Revalidate() = false, want true for a database with nothing built yet")
	}
}

// TestRevalidateStale_ListsOnlyStalePackages verifies that RevalidateStale
// returns exactly the stale root packages' import paths, leaving both an
// unaffected sibling (leaf) and an unaffected dependent (top, which imports
// the corrupted package but not any of its changed fields) out of the
// result.
func TestRevalidateStale_ListsOnlyStalePackages(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	old, err := db.GetUnit(ctx, store.Hash(pkgMid))
	if err != nil {
		t.Fatalf("GetUnit(mid): %v", err)
	}
	corrupted := old
	corrupted.ToolchainFingerprint = "corrupted-fingerprint"
	if err := db.PutUnitPointersBatch(map[uint64]store.UnitPointer{store.Hash(pkgMid): corrupted}); err != nil {
		t.Fatalf("PutUnitPointersBatch: %v", err)
	}

	pkgs, wholeDBStale, err := RevalidateStale(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("RevalidateStale: %v", err)
	}
	if wholeDBStale {
		t.Error("RevalidateStale() wholeDBStale = true, want false")
	}
	if want := []string{pkgMid}; !reflect.DeepEqual(pkgs, want) {
		t.Errorf("RevalidateStale() pkgs = %v, want %v", pkgs, want)
	}
}

// TestRevalidateStale_MismatchedFingerprintReportsWholeDBStale verifies that
// RevalidateStale reports wholeDBStale via the same whole-database
// short-circuit Revalidate uses, without a per-package fan-out.
func TestRevalidateStale_MismatchedFingerprintReportsWholeDBStale(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{ToolchainFingerprint: "go1.0-fake"}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	pkgs, wholeDBStale, err := RevalidateStale(ctx, snap, db, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("RevalidateStale: %v", err)
	}
	if !wholeDBStale {
		t.Error("RevalidateStale() wholeDBStale = false, want true for a mismatched toolchain fingerprint")
	}
	if len(pkgs) != 0 {
		t.Errorf("RevalidateStale() pkgs = %v, want empty when wholeDBStale", pkgs)
	}
}

// TestPackageChanged_MissingUnitPointer verifies that PackageChanged reports
// true for a package snap knows about but db has never recorded a
// store.UnitPointer for, without that alone making the whole database look
// stale (db's build fingerprint still matches).
func TestPackageChanged_MissingUnitPointer(t *testing.T) {
	dir := mutableTestModule(t)
	snap := loadSnapshot(t, dir)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	writeFile(t, dir, "extra/extra.go", "package extra\n\n// V returns 1.\nfunc V() int { return 1 }\n")
	snap = loadSnapshot(t, dir)

	const pkgExtra = "example.com/idxmod/extra"
	changed, err := PackageChanged(ctx, snap, db, pkgExtra, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("PackageChanged: %v", err)
	}
	if !changed {
		t.Error("PackageChanged() = false, want true for a package with no recorded UnitPointer")
	}
}

// TestPackageChanged_Unchanged verifies that PackageChanged reports false
// for a package right after Build, matching Revalidate's own
// TestRevalidate_NothingChanged.
func TestPackageChanged_Unchanged(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	changed, err := PackageChanged(ctx, snap, db, pkgLeaf, runtime.Version(), "", false)
	if err != nil {
		t.Fatalf("PackageChanged: %v", err)
	}
	if changed {
		t.Error("PackageChanged() = true, want false for an untouched package")
	}
}

// TestPackageChanged_UnknownPackage verifies that PackageChanged reports an
// error, not a boolean, for a path snap does not know about at all — a
// caller bug, not a "changed" condition.
func TestPackageChanged_UnknownPackage(t *testing.T) {
	snap := loadTestSnapshot(t)
	db := openTestDB(t)
	cas := openTestCAS(t)
	ctx := context.Background()

	if _, err := Build(ctx, snap, db, cas, &Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	if _, err := PackageChanged(ctx, snap, db, "example.com/idxmod/nonexistent", runtime.Version(), "", false); err == nil {
		t.Error("PackageChanged() error = nil, want an error for an unknown package path")
	}
}
