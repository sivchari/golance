package langfeat_test

import (
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

func TestTypeDefinition_SamePackage(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "typedef", "typedef.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "UseLocal Local")

	got, err := langfeat.TypeDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("TypeDefinition returned nil, want a result")
	}
	if got.SameFile == "" {
		t.Fatalf("TypeDefinition = %+v, want a same-package result", got)
	}
	wantOffset := mustIndex(t, text, "type Local") + len("type ")
	if got.Range.StartOffset != wantOffset {
		t.Errorf("Range.StartOffset = %d, want %d (Local's own declaration)", got.Range.StartOffset, wantOffset)
	}
}

func TestTypeDefinition_CrossPackage(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "typedef", "typedef.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "UseRemote typedefdep.Remote")

	got, err := langfeat.TypeDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeDefinition: %v", err)
	}
	if got == nil {
		t.Fatal("TypeDefinition returned nil, want a result")
	}
	if got.SameFile != "" {
		t.Errorf("SameFile = %q, want \"\" (a cross-package result)", got.SameFile)
	}
	if got.PkgPath != "example.com/langfeatmod/typedefdep" {
		t.Errorf("PkgPath = %q, want typedefdep's import path", got.PkgPath)
	}
	if got.ObjPath == "" {
		t.Error("ObjPath is empty, want a resolvable objectpath")
	}
}

func TestTypeDefinition_PointerUnwrap(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "typedef", "typedef.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "UsePointer *Local")

	got, err := langfeat.TypeDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeDefinition: %v", err)
	}
	if got == nil || got.SameFile == "" {
		t.Fatalf("TypeDefinition = %+v, want a same-package result through the pointer", got)
	}
}

func TestTypeDefinition_NoIdentifier(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "typedef", "typedef.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "\n\n// UseLocal")

	got, err := langfeat.TypeDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("TypeDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("TypeDefinition = %+v, want nil (no identifier at offset)", got)
	}
}
