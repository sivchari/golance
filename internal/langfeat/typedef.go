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
// available from cp's own AST/types.Info.
type TypeDefInfo struct {
	SameFile string
	Range    Range

	PkgPath string
	ObjPath string
}

// TypeDefinition resolves the identifier at offset (a byte offset from the
// start of file) to the named type of its static type, and returns where
// that type is declared. It returns (nil, nil) if offset is not on an
// identifier, the identifier's type is not (and does not contain) a named
// type, or the type is predeclared (e.g. error, any), which has no
// declaration to jump to.
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
	named := namedTypeOf(obj.Type())
	if named == nil {
		return nil, nil
	}

	tn := named.Obj()
	if tn.Pkg() == nil {
		return nil, nil // predeclared type (e.g. error, any): no declaration to jump to
	}
	if tn.Pkg() == cp.Package() {
		return sameFileTypeDef(cp, tn)
	}

	objPath, err := objectpath.For(tn)
	if err != nil {
		return nil, nil // not reachable via export data (e.g. a function-local type)
	}
	return &TypeDefInfo{PkgPath: tn.Pkg().Path(), ObjPath: string(objPath)}, nil
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

// namedTypeOf unwraps t through pointer, slice, array, map, and channel
// element types to find the *types.Named it ultimately names, if any.
func namedTypeOf(t types.Type) *types.Named {
	for range 10 { // bound against implausibly deep nesting
		switch tt := t.(type) {
		case *types.Named:
			return tt
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
