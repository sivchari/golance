package langfeat

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/tools/go/ast/astutil"
)

// goroot resolves the toolchain's GOROOT once: "go env GOROOT" is the
// supported way to locate it (runtime.GOROOT is deprecated since Go 1.24
// and wrong for a relocated binary), with the GOROOT environment variable
// as a fallback when the go binary is not on PATH.
var goroot = sync.OnceValue(func() string {
	if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root
		}
	}
	return os.Getenv("GOROOT")
})

// parseBuiltinFile parses the toolchain's $GOROOT/src/builtin/builtin.go —
// gopls's own resolution target for every universe (predeclared) identifier
// and the error interface's Error method (see gopls@v0.23.0's
// internal/golang/definition.go, builtinDecl; internal/cache/snapshot.go,
// Snapshot.BuiltinFile) — with comments (needed for hover doc text). Unlike
// a workspace or module-dependency package, builtin.go is never in any
// import graph (see internal/depcheck's package doc: nothing imports the
// pseudo-package "builtin"), so it needs its own dedicated resolution
// rather than routing through internal/depcheck.Provider's metadata-driven
// one. Called at most once per process — see loadBuiltinFile below, which
// memoizes it.
func parseBuiltinFile() (*ast.File, *token.FileSet, error) {
	root := goroot()
	if root == "" {
		return nil, nil, errors.New("langfeat: GOROOT could not be determined")
	}
	path := filepath.Join(root, "src", "builtin", "builtin.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("langfeat: parse %s: %w", path, err)
	}
	return file, fset, nil
}

// builtinFileResult boxes parseBuiltinFile's three return values so
// loadBuiltinFile below can memoize them with sync.OnceValue (its
// two-value sibling sync.OnceValues cannot express a triple).
type builtinFileResult struct {
	file *ast.File
	fset *token.FileSet
	err  error
}

// loadBuiltinFileOnce memoizes parseBuiltinFile for golance's lifetime:
// builtin.go is part of the installed toolchain, immutable without a
// golance restart, exactly like every other GOROOT source
// internal/depcheck already treats as immutable.
var loadBuiltinFileOnce = sync.OnceValue(func() builtinFileResult {
	file, fset, err := parseBuiltinFile()
	return builtinFileResult{file: file, fset: fset, err: err}
})

// loadBuiltinFile returns loadBuiltinFileOnce's cached result, unboxed.
func loadBuiltinFile() (*ast.File, *token.FileSet, error) {
	r := loadBuiltinFileOnce()
	return r.file, r.fset, r.err
}

// resolveBuiltinIdent resolves obj — a universe (predeclared) object or the
// error interface's Error method, both identified by types.Object.Pkg() ==
// nil — to its declaring identifier in builtin.go. Mirrors gopls's own
// two-branch dispatch (gopls@v0.23.0's internal/golang/definition.go,
// builtinDecl): a plain top-level declaration for a Parent() ==
// types.Universe object (a builtin type/func/const/var), or the error
// interface literal's sole method for the special-cased "Error" name. ok is
// false if obj is not actually a builtin, or GOROOT/builtin.go could not be
// resolved or parsed (a missing or corrupt toolchain install).
func resolveBuiltinIdent(obj types.Object) (id *ast.Ident, file *ast.File, fset *token.FileSet, ok bool) {
	file, fset, err := loadBuiltinFile()
	if err != nil {
		return nil, nil, nil, false
	}
	switch {
	case obj.Parent() == types.Universe:
		id = topLevelBuiltinIdent(file, obj.Name())
	case obj.Name() == "Error":
		id = errorMethodIdent(file)
	}
	if id == nil {
		return nil, nil, nil, false
	}
	return id, file, fset, true
}

// topLevelBuiltinIdent returns the declaring identifier named name among
// file's top-level declarations (a *ast.FuncDecl's name, or a
// *ast.TypeSpec's/*ast.ValueSpec's, inside a *ast.GenDecl) — a manual walk
// rather than file.Scope's legacy ast.Object resolution (deprecated since
// Go 1.22 and never populated unless parsed without
// parser.SkipObjectResolution), for every builtin.go declaration kind
// BuiltinDefinition/Hover need to resolve (see resolveBuiltinIdent's Parent
// == types.Universe branch). nil if name has no top-level declaration.
func topLevelBuiltinIdent(file *ast.File, name string) *ast.Ident {
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				return d.Name
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name == name {
						return s.Name
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name == name {
							return n
						}
					}
				}
			}
		}
	}
	return nil
}

// errorMethodIdent returns the declaring identifier of the "Error" method
// inside file's "type error interface { Error() string }" declaration —
// the error interface's own sole method, resolveBuiltinIdent's special
// case for an object whose Name() is "Error" but whose Parent() is not
// types.Universe (see gopls's identical special case). nil if file has no
// such declaration.
func errorMethodIdent(file *ast.File) *ast.Ident {
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "error" {
				continue
			}
			iface, ok := ts.Type.(*ast.InterfaceType)
			if !ok || iface.Methods == nil || len(iface.Methods.List) == 0 || len(iface.Methods.List[0].Names) == 0 {
				return nil
			}
			return iface.Methods.List[0].Names[0]
		}
	}
	return nil
}

// builtinTypeReplacer replaces builtin.go's synthetic "type classes" with
// their most common constituent type, mirroring gopls's own cosmetic
// substitution for a builtin's rendered signature (gopls@v0.23.0's
// internal/golang/types_format.go).
var builtinTypeReplacer = strings.NewReplacer(
	`ComplexType`, `complex128`,
	`FloatType`, `float64`,
	`IntegerType`, `int`,
)

// builtinDeclText renders the nearest enclosing *ast.GenDecl or
// *ast.FuncDecl around id — found by walking file's AST up from id's own
// position, exactly like gopls's hoverBuiltin (gopls@v0.23.0's
// internal/golang/hover.go) — as a signature (the declaration with its doc
// comment stripped out, formatted through go/format like every other
// AST-to-text rendering in this package; see codeaction.go) and doc (that
// stripped doc comment's own text). Both are "" if id has no enclosing
// GenDecl/FuncDecl (unreachable for any real builtin.go declaration) or
// formatting fails.
func builtinDeclText(fset *token.FileSet, file *ast.File, id *ast.Ident) (signature, doc string) {
	path, _ := astutil.PathEnclosingInterval(file, id.Pos(), id.Pos())
	for _, n := range path {
		var docGroup *ast.CommentGroup
		var decl ast.Node
		switch d := n.(type) {
		case *ast.FuncDecl:
			docGroup = d.Doc
			stripped := *d
			stripped.Doc = nil
			decl = &stripped
		case *ast.GenDecl:
			docGroup = d.Doc
			stripped := *d
			stripped.Doc = nil
			decl = &stripped
		default:
			continue
		}
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, decl); err != nil {
			return "", docGroup.Text()
		}
		return builtinTypeReplacer.Replace(buf.String()), docGroup.Text()
	}
	return "", ""
}
