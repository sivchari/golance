package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// TestTypeHierarchyPrepare exercises TypeHierarchyPrepare's cases via named
// helpers (rather than inline t.Run closures) so each case's assertions live
// in their own top-level function: gocognit scores nested control flow
// inside a closure against the ENCLOSING function, and six cases' worth of
// if-statements inlined here would push this function well past the
// project's complexity limit.
func TestTypeHierarchyPrepare(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "typehierarchy", "typehierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	t.Run("interface declaration", func(t *testing.T) { checkPrepareInterfaceDecl(t, cp, path, text) })
	t.Run("concrete type declaration", func(t *testing.T) { checkPrepareConcreteDecl(t, cp, path, text) })
	t.Run("method declaration is not a type name", func(t *testing.T) {
		checkPrepareNil(t, cp, path, text, "func (S) F() {}", "func (S) ", "F is a method, not a type name")
	})
	t.Run("cross-package type name", func(t *testing.T) { checkPrepareCrossPackage(t, cp, path, text) })
	t.Run("not a type name", func(t *testing.T) {
		checkPrepareNil(t, cp, path, text, "func notAType()", "func ", "notAType is a func, not a type name")
	})
	t.Run("no identifier", func(t *testing.T) {
		checkPrepareNil(t, cp, path, text, "\n\n// I declares F", "", "no identifier at offset")
	})
}

// checkPrepareInterfaceDecl asserts TypeHierarchyPrepare on I's own
// declaration resolves to a same-package interface item at exactly the
// queried offset.
func checkPrepareInterfaceDecl(t *testing.T, cp *check.CheckedPackage, path string, text []byte) {
	t.Helper()
	offset := mustIndex(t, text, "type I interface") + len("type ")
	info, err := langfeat.TypeHierarchyPrepare(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeHierarchyPrepare: %v", err)
	}
	if info == nil {
		t.Fatal("TypeHierarchyPrepare = nil, want a result")
	}
	if info.Name != "I" {
		t.Errorf("Name = %q, want %q", info.Name, "I")
	}
	if !info.IsInterface {
		t.Error("IsInterface = false, want true")
	}
	if info.SameFile == "" {
		t.Error("SameFile is empty, want a same-package result")
	}
	if info.PkgPath != "example.com/langfeatmod/typehierarchy" {
		t.Errorf("PkgPath = %q, want typehierarchy's own import path", info.PkgPath)
	}
	if info.Range.StartOffset != offset {
		t.Errorf("Range.StartOffset = %d, want %d", info.Range.StartOffset, offset)
	}
}

// checkPrepareConcreteDecl asserts TypeHierarchyPrepare on S's own
// declaration resolves to a non-interface item.
func checkPrepareConcreteDecl(t *testing.T, cp *check.CheckedPackage, path string, text []byte) {
	t.Helper()
	offset := mustIndex(t, text, "type S int") + len("type ")
	info, err := langfeat.TypeHierarchyPrepare(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeHierarchyPrepare: %v", err)
	}
	if info == nil {
		t.Fatal("TypeHierarchyPrepare = nil, want a result")
	}
	if info.Name != "S" {
		t.Errorf("Name = %q, want %q", info.Name, "S")
	}
	if info.IsInterface {
		t.Error("IsInterface = true, want false")
	}
}

// checkPrepareCrossPackage asserts TypeHierarchyPrepare on a reference to a
// type declared in a different workspace package resolves via
// (PkgPath, ObjPath) rather than SameFile.
func checkPrepareCrossPackage(t *testing.T, cp *check.CheckedPackage, path string, text []byte) {
	t.Helper()
	offset := mustIndex(t, text, "var Var other.Remote") + len("var Var other.")
	info, err := langfeat.TypeHierarchyPrepare(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeHierarchyPrepare: %v", err)
	}
	if info == nil {
		t.Fatal("TypeHierarchyPrepare = nil, want a result")
	}
	if info.Name != "Remote" {
		t.Errorf("Name = %q, want %q", info.Name, "Remote")
	}
	if info.SameFile != "" {
		t.Errorf("SameFile = %q, want \"\" (a cross-package result)", info.SameFile)
	}
	if info.PkgPath != "example.com/langfeatmod/typehierarchy/other" {
		t.Errorf("PkgPath = %q, want other's import path", info.PkgPath)
	}
	if info.ObjPath != "Remote" {
		t.Errorf("ObjPath = %q, want %q", info.ObjPath, "Remote")
	}
}

// checkPrepareNil asserts TypeHierarchyPrepare at the offset find plus
// len(suffix) returns (nil, nil), for the shared "nothing hierarchy-relevant
// at the cursor" cases (a method identifier, a plain func, no identifier at
// all). why describes the case for a failing assertion's message.
func checkPrepareNil(t *testing.T, cp *check.CheckedPackage, path string, text []byte, find, suffix, why string) {
	t.Helper()
	offset := mustIndex(t, text, find) + len(suffix)
	info, err := langfeat.TypeHierarchyPrepare(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeHierarchyPrepare: %v", err)
	}
	if info != nil {
		t.Errorf("TypeHierarchyPrepare = %+v, want nil (%s)", info, why)
	}
}
