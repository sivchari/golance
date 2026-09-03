package langfeat_test

import (
	"path/filepath"
	"strings"
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

// TestTypeDefinition_Builtin covers item 1(a)'s residual hole: typeDefinition
// on an identifier whose static type is predeclared -- a basic type like
// int, or a predeclared named type like error -- should resolve into
// builtin.go the same way BuiltinDefinition already does for plain "Go to
// Definition" on a builtin identifier itself (see builtin_test.go's
// TestBuiltinDefinition), rather than the (nil, nil) "no declaration to
// jump to" this used to return for every predeclared type. Mirrors gopls's
// own TypeDefinition, which routes both cases through ObjectLocation ->
// isBuiltin -> builtinDecl (gopls@v0.23.0's internal/golang/
// type_definition.go's typeToObjects *types.Basic case, and definition.go's
// isBuiltin/builtinDecl).
func TestTypeDefinition_Builtin(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "builtinuse", "builtinuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	tests := []struct {
		name     string
		offset   int
		declName string // top-level identifier wantBuiltinDeclPosition looks up
	}{
		{name: "basic type (var of type int)", offset: mustIndex(t, text, "Answer int"), declName: "int"},
		{name: "predeclared named type (param of type error)", offset: mustIndex(t, text, "err error"), declName: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := langfeat.TypeDefinition(cp, path, tt.offset)
			if err != nil {
				t.Fatalf("TypeDefinition: %v", err)
			}
			if got == nil || got.Builtin == nil {
				t.Fatalf("TypeDefinition = %+v, want a Builtin result", got)
			}
			if got.SameFile != "" || got.PkgPath != "" {
				t.Errorf("TypeDefinition = %+v, want only Builtin set", got)
			}
			if !strings.HasSuffix(filepath.ToSlash(got.Builtin.Filename), "builtin/builtin.go") {
				t.Errorf("Builtin.Filename = %s, want it to end in builtin/builtin.go", got.Builtin.Filename)
			}
			wantLine, wantCol := wantBuiltinDeclPosition(t, tt.declName)
			if got.Builtin.Line != wantLine || got.Builtin.Col != wantCol {
				t.Errorf("Builtin position = %d:%d, want %d:%d (%s's own declaration)", got.Builtin.Line, got.Builtin.Col, wantLine, wantCol, tt.declName)
			}
		})
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
