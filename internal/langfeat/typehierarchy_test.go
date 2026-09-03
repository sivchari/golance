package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestTypeHierarchyPrepare(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "typehierarchy", "typehierarchy.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	t.Run("interface declaration", func(t *testing.T) {
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
	})

	t.Run("concrete type declaration", func(t *testing.T) {
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
	})

	t.Run("method declaration is not a type name", func(t *testing.T) {
		offset := mustIndex(t, text, "func (S) F() {}") + len("func (S) ")
		info, err := langfeat.TypeHierarchyPrepare(cp, path, offset)
		if err != nil {
			t.Fatalf("TypeHierarchyPrepare: %v", err)
		}
		if info != nil {
			t.Errorf("TypeHierarchyPrepare = %+v, want nil (F is a method, not a type name)", info)
		}
	})

	t.Run("cross-package type name", func(t *testing.T) {
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
	})

	t.Run("not a type name", func(t *testing.T) {
		offset := mustIndex(t, text, "func notAType()") + len("func ")
		info, err := langfeat.TypeHierarchyPrepare(cp, path, offset)
		if err != nil {
			t.Fatalf("TypeHierarchyPrepare: %v", err)
		}
		if info != nil {
			t.Errorf("TypeHierarchyPrepare = %+v, want nil (notAType is a func, not a type name)", info)
		}
	})

	t.Run("no identifier", func(t *testing.T) {
		offset := mustIndex(t, text, "\n\n// I declares F")
		info, err := langfeat.TypeHierarchyPrepare(cp, path, offset)
		if err != nil {
			t.Fatalf("TypeHierarchyPrepare: %v", err)
		}
		if info != nil {
			t.Errorf("TypeHierarchyPrepare = %+v, want nil (no identifier at offset)", info)
		}
	})
}
