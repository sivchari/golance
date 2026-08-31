package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/overlay"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/objectpath"
)

// CompletionDocKey identifies a completionItem/resolve request's originating
// completion query (document, cursor offset, and the candidate's label)
// well enough to re-resolve the same types.Object Completion would have
// produced it from, without the server needing to keep any completion
// session state around between the two requests.
type CompletionDocKey struct {
	File   string
	Offset int
	Label  string
}

// CompletionDocInfo is the result of resolving a CompletionDocKey: the
// object's doc comment, if it is declared in the same package as the query
// (Doc is then already the answer), or enough to look it up in a different
// package's on-disk facts index (PkgPath/ObjPath) otherwise.
type CompletionDocInfo struct {
	Doc string

	PkgPath string
	ObjPath string
}

// ResolveCompletionDoc re-derives the completion context at (key.File,
// key.Offset) the same way Completion does, and returns doc info for
// whichever candidate object's name equals key.Label. It returns (nil, nil)
// if no such candidate is found.
func ResolveCompletionDoc(cp *check.CheckedPackage, reader overlay.FileReader, key CompletionDocKey) (*CompletionDocInfo, error) {
	text, err := reader.ReadFile(key.File)
	if err != nil {
		return nil, err
	}
	prefixStart := scanIdentBack(text, key.Offset)
	astFile, ctxPos, _, err := locate(cp, key.File, prefixStart)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, ctxPos, ctxPos)

	obj := objectForLabel(cp, ctxPos, path, key.Label)
	if obj == nil {
		return nil, nil
	}
	if obj.Pkg() == cp.Package() {
		return &CompletionDocInfo{Doc: docForObject(cp, obj)}, nil
	}
	if obj.Pkg() == nil {
		return nil, nil // universe/builtin object: no doc source
	}
	objPath, err := objectpath.For(obj)
	if err == nil {
		return &CompletionDocInfo{PkgPath: obj.Pkg().Path(), ObjPath: string(objPath)}, nil
	}
	return nil, nil
}

// objectForLabel re-resolves the same completion context Completion uses
// (an enclosing selector, or lexical scope) and looks up label directly as
// a types.Object, rather than building the full []CompletionItem list.
func objectForLabel(cp *check.CheckedPackage, ctxPos token.Pos, path []ast.Node, label string) types.Object {
	if sel := enclosingSelector(path); sel != nil {
		return selectorObjectForLabel(cp, sel, label)
	}
	scope := cp.Package().Scope().Innermost(ctxPos)
	if scope == nil {
		scope = cp.Package().Scope()
	}
	for s := scope; s != nil; s = s.Parent() {
		if obj := s.Lookup(label); obj != nil {
			return obj
		}
	}
	return nil
}

// selectorObjectForLabel is objectForLabel's counterpart for "x.<label>"
// completion: a package member if x names an imported package, otherwise a
// field or method of x's type.
func selectorObjectForLabel(cp *check.CheckedPackage, sel *ast.SelectorExpr, label string) types.Object {
	if id, ok := sel.X.(*ast.Ident); ok {
		if pn, ok := cp.Info().ObjectOf(id).(*types.PkgName); ok {
			return pn.Imported().Scope().Lookup(label)
		}
	}
	xType := cp.Info().TypeOf(sel.X)
	if xType == nil {
		return nil
	}
	obj, _, _ := types.LookupFieldOrMethod(xType, true, cp.Package(), label)
	return obj
}
