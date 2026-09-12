package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"github.com/sivchari/golance/internal/depcheck"
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
// (Doc is then already the answer), enough to look it up in a different
// package's on-disk facts index (PkgPath/ObjPath) otherwise, or — if the
// candidate was actually an unimported-package-member completion item (see
// Unimported) — UnimportedSelector, since this package has no graph access
// to resolve that case itself (see ResolveCompletionDoc's own doc).
type CompletionDocInfo struct {
	Doc string

	PkgPath string
	ObjPath string

	// UnimportedSelector is the base identifier's name (e.g. "fmt" for a
	// "fmt.Sp" candidate with fmt not yet imported) when key's candidate came
	// from the unimported-package-member completion path instead of an
	// ordinary resolved object. The caller must redo the same
	// package-name-to-import-path lookup its own unimported-completion
	// pipeline used to build the candidate in the first place, find
	// key.Label among that package's members, and doc it the same way a
	// resolved PkgPath/ObjPath above would be.
	UnimportedSelector string
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

	obj, unimportedSelector := objectForLabel(cp, ctxPos, path, key.Label)
	if obj == nil {
		if unimportedSelector != "" {
			return &CompletionDocInfo{UnimportedSelector: unimportedSelector}, nil
		}
		return nil, nil
	}
	if obj.Pkg() == cp.Package() {
		return &CompletionDocInfo{Doc: docForObject(cp, obj)}, nil
	}
	if obj.Pkg() == nil {
		return nil, nil // universe/builtin object: no doc source
	}
	// depcheck.OriginObject normalizes a field/method reached through an
	// instantiated generic type to its origin declaration -- see its doc for
	// why objectpath.For cannot encode a path for the synthetic,
	// as-instantiated object directly.
	objPath, err := objectpath.For(depcheck.OriginObject(obj))
	if err == nil {
		return &CompletionDocInfo{PkgPath: obj.Pkg().Path(), ObjPath: string(objPath)}, nil
	}
	return nil, nil
}

// objectForLabel re-resolves the same completion context Completion uses
// (an enclosing selector, or lexical scope) and looks up label directly as
// a types.Object, rather than building the full []CompletionItem list.
// unimportedSelector is non-empty only when obj is nil because the
// enclosing selector's base matches unresolvedSelectorBase's shape (see its
// own doc) — the caller's only path to a useful answer for that case.
func objectForLabel(cp *check.CheckedPackage, ctxPos token.Pos, path []ast.Node, label string) (obj types.Object, unimportedSelector string) {
	if sel := enclosingSelector(path); sel != nil {
		if obj := selectorObjectForLabel(cp, sel, label); obj != nil {
			return obj, ""
		}
		name, ok := unresolvedSelectorBase(cp, sel)
		if !ok {
			return nil, ""
		}
		return nil, name
	}
	scope := cp.Package().Scope().Innermost(ctxPos)
	if scope == nil {
		scope = cp.Package().Scope()
	}
	for s := scope; s != nil; s = s.Parent() {
		if obj := s.Lookup(label); obj != nil {
			return obj, ""
		}
	}
	return nil, ""
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
