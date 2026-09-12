package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/objectpath"
)

// TypeDefInfo is the result of a TypeDefinition query: where the named type
// of the queried identifier is declared.
//
// When the type is declared in cp's own package, SameFile and Range locate
// it directly (the same byte-offset coordinate system every other position
// in this package uses). When it is declared in a different package,
// SameFile is "" and PkgPath/ObjPath identify it instead, for the server
// layer to resolve through the on-disk facts index (internal/xref): unlike
// Hover's docForObject, a different package's source position is not
// available from cp's own AST/types.Info. When the type is predeclared
// (e.g. error, or a basic type like int -- gopls's own TypeDefinition
// resolves both the same way, see typeToObjects's *types.Basic case in
// gopls@v0.23.0's internal/golang/identifier.go), Builtin identifies its
// declaration in the toolchain's $GOROOT/src/builtin/builtin.go instead,
// mirroring BuiltinDefInfo's identical role for plain "Go to Definition".
type TypeDefInfo struct {
	SameFile string
	Range    Range

	PkgPath string
	ObjPath string

	Builtin *BuiltinDefInfo
}

// TypeDefinition resolves the identifier at offset (a byte offset from the
// start of file) to the named type of its static type, and returns where
// that type is declared. It returns (nil, nil) if offset is not on an
// identifier, the identifier's type is not (and does not contain) a named
// or predeclared type, or the type is predeclared but GOROOT/builtin.go
// could not be resolved (see TypeDefInfo's Builtin field).
func TypeDefinition(cp *check.CheckedPackage, file string, offset int) (*TypeDefInfo, error) {
	astFile, pos, _, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, nil
	}
	obj := cp.Info().ObjectOf(id)
	if obj == nil {
		return nil, nil
	}
	tn := typeNameOf(obj.Type())
	if tn == nil {
		return nil, nil
	}

	if tn.Pkg() == nil {
		// A predeclared type (error, int, string, ...): resolved into
		// builtin.go, the same pseudo-package gopls's own TypeDefinition
		// resolves these against (see builtinDecl in
		// gopls@v0.23.0's internal/golang/definition.go).
		info, ok := builtinDefInfoFor(tn)
		if !ok {
			return nil, nil
		}
		return &TypeDefInfo{Builtin: info}, nil
	}
	if tn.Pkg() == cp.Package() {
		return sameFileTypeDef(cp, tn)
	}

	objPath, err := objectpath.For(tn)
	if err == nil {
		return &TypeDefInfo{PkgPath: tn.Pkg().Path(), ObjPath: string(objPath)}, nil
	}
	return nil, nil // not reachable via export data (e.g. a function-local type)
}

// sameFileTypeDef resolves tn's own declaring identifier within cp's
// already-parsed files, for a type declared in cp's own package.
func sameFileTypeDef(cp *check.CheckedPackage, tn *types.TypeName) (*TypeDefInfo, error) {
	declFile, tf, ok := fileContaining(cp, tn.Pos())
	if !ok {
		return nil, nil
	}
	declPath, _ := astutil.PathEnclosingInterval(declFile, tn.Pos(), tn.Pos())
	declID := identAt(declPath)
	if declID == nil {
		return nil, nil
	}
	return &TypeDefInfo{SameFile: tf.Name(), Range: rangeOf(tf, declID.Pos(), declID.End())}, nil
}

// typeNameOf unwraps t through pointer, slice, array, map, and channel
// element types to find the *types.TypeName it ultimately names, if any: a
// *types.Named's own Obj(), or -- mirroring gopls's identical *types.Basic
// case in typeToObjects (gopls@v0.23.0's internal/golang/identifier.go) --
// a predeclared basic type's (int, string, bool, ...) entry in
// types.Universe, which has no *types.Named of its own to unwrap through.
//
// A *types.Alias (a `type X = Y` declaration's own TypeName.Type(), the
// distinct wrapper go/types has represented this as since Go 1.23's
// gotypesalias=1 default) resolves to its own Obj(), exactly like
// *types.Named: this is what makes a query on the alias declaration's own
// name self-point to that declaration, the same way a plain named type's
// own name does, instead of falling through to nil.
//
// A *types.Signature -- t is obj.Type() for a func/method identifier --
// unwraps to its single result's type, mirroring gopls's TypeDefinition on
// such an identifier resolving to the type of the value it evaluates to.
// Zero or multiple results have no single answer this way; gopls itself
// reports a protocol error for those rather than degrading, so this simply
// returns nil, matching golance's existing empty-result behavior for them.
func typeNameOf(t types.Type) *types.TypeName {
	for range 10 { // bound against implausibly deep nesting
		switch tt := t.(type) {
		case *types.Named:
			return tt.Obj()
		case *types.Alias:
			return tt.Obj()
		case *types.Basic:
			tn, _ := types.Universe.Lookup(tt.Name()).(*types.TypeName)
			return tn
		case *types.Pointer:
			t = tt.Elem()
		case *types.Slice:
			t = tt.Elem()
		case *types.Array:
			t = tt.Elem()
		case *types.Chan:
			t = tt.Elem()
		case *types.Map:
			t = tt.Elem()
		case *types.Signature:
			if tt.Results().Len() != 1 {
				return nil
			}
			t = tt.Results().At(0).Type()
		default:
			return nil
		}
	}
	return nil
}

// fileContaining returns the *ast.File and *token.File in cp's own package
// containing pos.
func fileContaining(cp *check.CheckedPackage, pos token.Pos) (*ast.File, *token.File, bool) {
	tf := cp.FileSet().File(pos)
	if tf == nil {
		return nil, nil, false
	}
	for _, f := range cp.Files() {
		if cp.FileSet().File(f.Pos()) == tf {
			return f, tf, true
		}
	}
	return nil, nil, false
}
