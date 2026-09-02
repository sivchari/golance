package langfeat_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sivchari/golance/internal/langfeat"
	"github.com/sivchari/golance/internal/overlay"
)

// realBuiltinGo returns the path to the installed toolchain's
// $GOROOT/src/builtin/builtin.go, resolved independently of
// langfeat's own goroot() cache, so the ground-truth helpers below track
// whatever the installed toolchain's real source says rather than a
// hardcoded, version-fragile constant (mirrors wantDeclPosition in
// definition_test.go, and TestDependencyDefinition_Stdlib's own doc for
// why).
func realBuiltinGo(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "src", "builtin", "builtin.go")
}

// wantBuiltinDeclPosition returns the (line, column) of name's top-level
// declaring identifier in the real builtin.go, parsed independently of
// langfeat's own resolution.
func wantBuiltinDeclPosition(t *testing.T, name string) (line, col int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, realBuiltinGo(t), nil, 0)
	if err != nil {
		t.Fatalf("parse builtin.go: %v", err)
	}
	var found *ast.Ident
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				found = d.Name
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name == name {
						found = s.Name
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name == name {
							found = n
						}
					}
				}
			}
		}
	}
	if found == nil {
		t.Fatalf("no top-level declaration named %q in builtin.go", name)
	}
	p := fset.Position(found.Pos())
	return p.Line, p.Column
}

// wantErrorMethodPosition returns the (line, column) of the error
// interface's own "Error" method name in the real builtin.go.
func wantErrorMethodPosition(t *testing.T) (line, col int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, realBuiltinGo(t), nil, 0)
	if err != nil {
		t.Fatalf("parse builtin.go: %v", err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "error" {
				continue
			}
			iface, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				continue
			}
			id := iface.Methods.List[0].Names[0]
			p := fset.Position(id.Pos())
			return p.Line, p.Column
		}
	}
	t.Fatal("no error interface declaration found in builtin.go")
	return 0, 0
}

func TestBuiltinDefinition(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "builtinuse", "builtinuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	tests := []struct {
		name        string
		offset      int
		declName    string // top-level identifier wantBuiltinDeclPosition looks up
		errorMethod bool   // resolve via wantErrorMethodPosition instead
	}{
		{name: "nil", offset: mustIndex(t, text, "== nil") + len("== "), declName: "nil"},
		{name: "len", offset: mustIndex(t, text, "len(v)"), declName: "len"},
		{name: "make", offset: mustIndex(t, text, "make([]byte"), declName: "make"},
		{name: "iota", offset: mustIndex(t, text, "= iota") + len("= "), declName: "iota"},
		{name: "int", offset: mustIndex(t, text, "Answer int") + len("Answer "), declName: "int"},
		{name: "error type", offset: mustIndex(t, text, "err error") + len("err "), declName: "error"},
		{name: "error.Error method", offset: mustIndex(t, text, "err.Error") + len("err."), errorMethod: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := langfeat.BuiltinDefinition(cp, path, tt.offset)
			if err != nil {
				t.Fatalf("BuiltinDefinition: %v", err)
			}
			if got == nil {
				t.Fatal("BuiltinDefinition returned nil, want a result")
			}
			if !strings.HasSuffix(filepath.ToSlash(got.Filename), "builtin/builtin.go") {
				t.Errorf("Filename = %s, want it to end in builtin/builtin.go", got.Filename)
			}
			var wantLine, wantCol int
			if tt.errorMethod {
				wantLine, wantCol = wantErrorMethodPosition(t)
			} else {
				wantLine, wantCol = wantBuiltinDeclPosition(t, tt.declName)
			}
			if got.Line != wantLine || got.Col != wantCol {
				t.Errorf("position = %d:%d, want %d:%d", got.Line, got.Col, wantLine, wantCol)
			}
		})
	}
}

func TestBuiltinDefinition_NonBuiltinDeclines(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "builtinuse", "builtinuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// UseNil is declared in cp's own package (Pkg() != nil): not a builtin.
	offset := mustIndex(t, text, "func UseNil") + len("func ")

	got, err := langfeat.BuiltinDefinition(cp, path, offset)
	if err != nil {
		t.Fatalf("BuiltinDefinition: %v", err)
	}
	if got != nil {
		t.Errorf("BuiltinDefinition(UseNil) = %+v, want nil (declared in cp's own package)", got)
	}
}

func TestHoverBuiltins(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "builtinuse", "builtinuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	tests := []struct {
		name       string
		offset     int
		wantSigSub string
		wantDocSub string
	}{
		{
			name:       "len",
			offset:     mustIndex(t, text, "len(v)"),
			wantSigSub: "func len(v Type) int",
			wantDocSub: "The len built-in function returns the length of v",
		},
		{
			name:       "nil",
			offset:     mustIndex(t, text, "== nil") + len("== "),
			wantSigSub: "var nil Type",
			wantDocSub: "nil is a predeclared identifier",
		},
		{
			name:       "error type",
			offset:     mustIndex(t, text, "err error") + len("err "),
			wantSigSub: "interface",
			wantDocSub: "The error built-in interface type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := langfeat.Hover(cp, path, tt.offset)
			if err != nil {
				t.Fatalf("Hover: %v", err)
			}
			if got == nil {
				t.Fatal("Hover returned nil, want a result")
			}
			if !strings.Contains(got.Signature, tt.wantSigSub) {
				t.Errorf("Signature = %q, want it to contain %q", got.Signature, tt.wantSigSub)
			}
			if !strings.Contains(got.Doc, tt.wantDocSub) {
				t.Errorf("Doc = %q, want it to contain %q", got.Doc, tt.wantDocSub)
			}
		})
	}
}

// TestHoverBuiltin_ErrorMethod covers error.Error specifically (task item
// 4): unlike every other builtin, gopls's own hoverBuiltin gives it no doc
// comment at all, just its type-checked signature (see
// gopls@v0.23.0's internal/golang/hover.go, hoverBuiltin's "Error"
// special case) — mirrored here rather than improved on, for parity.
func TestHoverBuiltin_ErrorMethod(t *testing.T) {
	reader := overlay.New()
	cp, path := newCheckedPackage(t, reader, "builtinuse", "builtinuse.go")
	text, err := reader.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	offset := mustIndex(t, text, "err.Error") + len("err.")

	got, err := langfeat.Hover(cp, path, offset)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got == nil {
		t.Fatal("Hover returned nil, want a result")
	}
	if !strings.Contains(got.Signature, "Error() string") {
		t.Errorf("Signature = %q, want it to contain the Error method's signature", got.Signature)
	}
	if got.Doc != "" {
		t.Errorf("Doc = %q, want empty (gopls's own error.Error simplification)", got.Doc)
	}
}
