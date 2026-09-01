package langfeat

import (
	"go/ast"
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/objectpath"
)

// HoverInfo is the result of a Hover query: a symbol's type signature, its
// doc comment (if available), and the source range of the identifier the
// query resolved. Doc is already the answer for an object declared in cp's
// own package; for one declared in a DIFFERENT package, Doc is "" and
// PkgPath/ObjPath identify it instead — the same split
// CompletionDocInfo/TypeDefInfo already use — for the server layer to
// resolve through the workspace facts index (a root package) or
// internal/depcheck (a standard library/module dependency), mirroring
// handlers_nav.go's completionDoc.
type HoverInfo struct {
	Signature string
	Doc       string
	Range     Range

	PkgPath string
	ObjPath string
}

// Hover resolves the identifier at offset (a byte offset from the start of
// file) to its type signature and doc comment. It returns (nil, nil) if
// offset does not land on a resolvable identifier.
func Hover(cp *check.CheckedPackage, file string, offset int) (*HoverInfo, error) {
	astFile, pos, tf, err := locate(cp, file, offset)
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
	info := &HoverInfo{
		Signature: types.ObjectString(obj, qualifier(cp.Package())),
		Range:     rangeOf(tf, id.Pos(), id.End()),
	}
	if obj.Pkg() == cp.Package() {
		info.Doc = docForObject(cp, obj)
	} else if obj.Pkg() != nil {
		if objPath, err := objectpath.For(obj); err == nil {
			info.PkgPath, info.ObjPath = obj.Pkg().Path(), string(objPath)
		}
	}
	return info, nil
}

// identAt returns the innermost *ast.Ident in path, or nil if path's
// innermost node is not an identifier.
func identAt(path []ast.Node) *ast.Ident {
	if len(path) == 0 {
		return nil
	}
	id, _ := path[0].(*ast.Ident)
	return id
}

// docForObject returns obj's doc comment, if obj is declared within cp's
// own package (so its declaration is among cp.Files()). Objects from
// imported packages have no doc: only export data, not source, is
// available for them.
func docForObject(cp *check.CheckedPackage, obj types.Object) string {
	if obj.Pkg() != cp.Package() {
		return ""
	}
	pos := obj.Pos()
	tf := cp.FileSet().File(pos)
	if tf == nil {
		return ""
	}
	var declFile *ast.File
	for _, f := range cp.Files() {
		if cp.FileSet().File(f.Pos()) == tf {
			declFile = f
			break
		}
	}
	if declFile == nil {
		return ""
	}
	path, _ := astutil.PathEnclosingInterval(declFile, pos, pos)
	for _, n := range path {
		switch d := n.(type) {
		case *ast.FuncDecl:
			return d.Doc.Text()
		case *ast.TypeSpec:
			if d.Doc != nil {
				return d.Doc.Text()
			}
		case *ast.ValueSpec:
			if d.Doc != nil {
				return d.Doc.Text()
			}
		case *ast.Field:
			if d.Doc != nil {
				return d.Doc.Text()
			}
		case *ast.GenDecl:
			return d.Doc.Text()
		}
	}
	return ""
}
