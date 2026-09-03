package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/types/objectpath"
	"golang.org/x/tools/go/types/typeutil"
)

// CallHierarchyFuncAt resolves the identifier at offset (a byte offset from
// the start of file) to the *types.Func it denotes, for
// textDocument/prepareCallHierarchy and callHierarchy/outgoingCalls (which
// re-resolves an item's own declaring identifier the same way). It returns
// (nil, false) if offset is not on an identifier, the identifier resolves
// to no object or a non-func object, or the func has no home package (a
// builtin, or the universe error interface's Error method) -- gopls itself
// supports a builtin target too, but golance's facts index has no builtin
// symbols to answer callHierarchy/incomingCalls with and OutgoingCalls
// already treats any builtin callee as a dead end (see OutgoingCalls' own
// doc), so a builtin target would only ever produce empty incoming/outgoing
// results; excluding it here keeps prepareCallHierarchy from advertising a
// call hierarchy root that can never expand.
func CallHierarchyFuncAt(cp *check.CheckedPackage, file string, offset int) (*types.Func, bool) {
	astFile, pos, _, err := locate(cp, file, offset)
	if err != nil {
		return nil, false
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	id := identAt(path)
	if id == nil {
		return nil, false
	}
	fn, ok := cp.Info().ObjectOf(id).(*types.Func)
	if !ok || fn.Pkg() == nil {
		return nil, false
	}
	return fn, true
}

// FuncDeclInfo is the result of a FuncDeclaration query: where fn is
// declared, in the same same-package-or-not shape TypeDefInfo/
// SamePackageDefInfo already use elsewhere in this package.
type FuncDeclInfo struct {
	SameFile string
	Range    Range

	PkgPath string
	ObjPath string
}

// FuncDeclaration resolves fn (found via CallHierarchyFuncAt, or a callee
// from OutgoingCalls) to its declaring identifier's location: directly from
// cp's own AST when fn is declared in cp's own package, or via
// (PkgPath, ObjPath) for the server layer to resolve through the workspace
// facts index or internal/depcheck otherwise -- the same split
// TypeDefinition uses for a named type's declaration. It returns (nil, nil)
// if fn's declaration is unreachable via objectpath (e.g. a function-local
// type's method, which can never be fn here since fn always names a
// package-scope func or method -- see CallHierarchyFuncAt/OutgoingCalls).
func FuncDeclaration(cp *check.CheckedPackage, fn *types.Func) (*FuncDeclInfo, error) {
	if fn.Pkg() == cp.Package() {
		declFile, tf, ok := fileContaining(cp, fn.Pos())
		if !ok {
			return nil, nil
		}
		declPath, _ := astutil.PathEnclosingInterval(declFile, fn.Pos(), fn.Pos())
		declID := identAt(declPath)
		if declID == nil {
			return nil, nil
		}
		return &FuncDeclInfo{SameFile: tf.Name(), Range: rangeOf(tf, declID.Pos(), declID.End())}, nil
	}
	objPath, err := objectpath.For(fn)
	if err != nil {
		return nil, nil
	}
	return &FuncDeclInfo{PkgPath: fn.Pkg().Path(), ObjPath: string(objPath)}, nil
}

// OutgoingCall is one entry of an OutgoingCalls result: a callee reached
// from the queried function, and every call site (in source order) that
// reaches it.
type OutgoingCall struct {
	Callee     *types.Func
	FromRanges []Range
}

// OutgoingCalls returns every function/method fn's own body calls,
// aggregated by callee (so two calls to the same callee become one entry
// with two FromRanges, in source order), for callHierarchy/outgoingCalls.
// It returns (nil, nil) if fn has no reachable *ast.FuncDecl in cp's own
// files (an interface method, which has no body -- see
// CallHierarchyFuncAt's doc for why a builtin can never reach here) or that
// FuncDecl has no body (an external/assembly-only declaration).
//
// A call is collected wherever it lexically appears in fn's declaration,
// including inside a nested function literal, a deferred or go-statement
// call, and a method-expression or interface-mediated call: gopls treats a
// named function and every function literal nested inside it as one call
// hierarchy entity (see golang.org/x/tools/gopls's OutgoingCalls), which
// ast.Inspect over the whole FuncDecl body already achieves without special
// casing *ast.FuncLit, *ast.DeferStmt, or *ast.GoStmt individually -- each
// wraps an *ast.CallExpr that Inspect visits like any other.
//
// typeutil.Callee resolves both a static call (a plain function, or a
// method reached through a concrete receiver) and an interface-mediated
// call (a method reached through an interface-typed value or a method
// expression) to the same *types.Func -- gopls's own OutgoingCalls
// (golang.org/x/tools/gopls) uses the identical call to treat both
// uniformly, and this does too. A call whose target is not a *types.Func at
// all -- every builtin (including unsafe.Slice and similar, which gopls
// itself special-cases to keep; see typeutil.Callee's own doc), a type
// conversion, or a value held in a variable/field/func-returning expression
// -- is skipped: this is a deliberate, narrower divergence from gopls,
// justified by golance's cross-package resolution (FuncDeclaration) having
// no counterpart for a *types.Builtin declaration to resolve a location
// for, and unsafe.* as an outgoing-call target being of little value on its
// own. A callee with no home package (Pkg() == nil; the universe error
// interface's Error method reached through an embedding interface) is
// skipped too, for the same reason.
func OutgoingCalls(cp *check.CheckedPackage, fn *types.Func) ([]OutgoingCall, error) {
	declFile, tf, ok := fileContaining(cp, fn.Pos())
	if !ok {
		return nil, nil
	}
	decl := funcDeclAt(declFile, fn.Pos())
	if decl == nil || decl.Body == nil {
		return nil, nil
	}

	byCallee := make(map[*types.Func]*OutgoingCall)
	var order []*types.Func
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, ok := typeutil.Callee(cp.Info(), call).(*types.Func)
		if !ok || callee.Pkg() == nil {
			return true
		}
		id := calleeIdent(call.Fun)
		if id == nil {
			return true
		}
		oc, exists := byCallee[callee]
		if !exists {
			oc = &OutgoingCall{Callee: callee}
			byCallee[callee] = oc
			order = append(order, callee)
		}
		oc.FromRanges = append(oc.FromRanges, rangeOf(tf, id.Pos(), id.End()))
		return true
	})

	out := make([]OutgoingCall, 0, len(order))
	for _, callee := range order {
		out = append(out, *byCallee[callee])
	}
	return out, nil
}

// funcDeclAt returns the *ast.FuncDecl in astFile whose Name identifier is
// at pos, or nil if pos names something else (an interface method, an
// abstract declaration with no body of its own).
func funcDeclAt(astFile *ast.File, pos token.Pos) *ast.FuncDecl {
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)
	for _, n := range path {
		if fd, ok := n.(*ast.FuncDecl); ok {
			return fd
		}
	}
	return nil
}

// calleeIdent returns the identifier a call expression's Fun names the
// callee by -- fun itself if it is a plain identifier, its Sel if it is a
// selector expression (pkg.Fn or recv.Method), or the same unwrapped
// through a generic instantiation's index expression -- after stripping any
// enclosing parentheses. It returns nil for a call through an expression
// with no single naming identifier (e.g. a function returned by another
// call), which typeutil.StaticCallee's own nil result already excludes
// before this is ever reached.
func calleeIdent(fun ast.Expr) *ast.Ident {
	switch e := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return e
	case *ast.SelectorExpr:
		return e.Sel
	case *ast.IndexExpr:
		return calleeIdent(e.X)
	case *ast.IndexListExpr:
		return calleeIdent(e.X)
	default:
		return nil
	}
}
