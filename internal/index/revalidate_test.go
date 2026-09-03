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
