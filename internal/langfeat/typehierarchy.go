package langfeat

import (
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/objectpath"
)

// TypeHierarchyPrepareInfo is the result of a prepareTypeHierarchy query:
// the *types.TypeName at the cursor's own name and kind, plus where it is
// declared, in the same same-package-or-not shape TypeDefInfo/FuncDeclInfo
// already use elsewhere in this package. PkgPath is always populated
// (unlike TypeDefInfo, which only needs it for the cross-package case): the
// server layer needs it either way to build the LSP item's Detail field.
type TypeHierarchyPrepareInfo struct {
	Name        string
	IsInterface bool
	PkgPath     string

	SameFile string
	Range    Range

	ObjPath string
}

// TypeHierarchyPrepare resolves the identifier at offset (a byte offset
// from the start of file) to the *types.TypeName it names -- the type NAMED
// at the cursor, not (unlike TypeDefinition) the type OF whatever the
// cursor's identifier denotes -- per gopls's own PrepareTypeHierarchy, which
// requires the selection to literally be a type name (see
// golang.org/x/tools/gopls/internal/golang/type_hierarchy.go). It returns
// (nil, nil) if offset is not on an identifier, the identifier denotes
// something other than a type name (a func, var, method, package, ...), or
// the type is predeclared (e.g. error, any), which has no declaration to
// jump to -- gopls answers the "not a type name" case with an LSP error, but
// golance follows CallHierarchyFuncAt/prepareCallHierarchy's own established
// convention of a graceful empty result for "nothing hierarchy-relevant at
// the cursor" instead of an error.
func TypeHierarchyPrepare(cp *check.CheckedPackage, file string, offset int) (*TypeHierarchyPrepareInfo, error) {
	astFile, pos, _, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, nil
	}
	tn, ok := cp.Info().ObjectOf(id).(*types.TypeName)
	if !ok || tn.Pkg() == nil {
		return nil, nil
	}
	isInterface := types.IsInterface(tn.Type())

	if tn.Pkg() == cp.Package() {
		declFile, tf, ok := fileContaining(cp, tn.Pos())
		if !ok {
			return nil, nil
		}
		declPath, _ := astutil.PathEnclosingInterval(declFile, tn.Pos(), tn.Pos())
		declID := identAt(declPath)
		if declID == nil {
			return nil, nil
		}
		return &TypeHierarchyPrepareInfo{
			Name: tn.Name(), IsInterface: isInterface, PkgPath: tn.Pkg().Path(),
			SameFile: tf.Name(), Range: rangeOf(tf, declID.Pos(), declID.End()),
		}, nil
	}

	objPath, err := objectpath.For(tn)
	if err == nil {
		return &TypeHierarchyPrepareInfo{
			Name: tn.Name(), IsInterface: isInterface, PkgPath: tn.Pkg().Path(),
			ObjPath: string(objPath),
		}, nil
	}
	return nil, nil // not reachable via export data (e.g. a function-local type)
}
