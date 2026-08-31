package langfeat

import (
	"go/ast"
	"go/types"
	"sort"

	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
)

// HighlightKind categorizes a Highlight, independent of any LSP protocol
// type.
type HighlightKind int

// Kinds a Highlight can have.
const (
	HighlightRead HighlightKind = iota
	HighlightWrite
)

// Highlight is one occurrence, within a single file, of the symbol a
// DocumentHighlight query resolved.
type Highlight struct {
	Range Range
	Kind  HighlightKind
}

// DocumentHighlight returns every occurrence, within file, of the symbol at
// offset (a byte offset from the start of file): its declaration and every
// use in that file, classified as a Write (the declaration itself, or an
// assignment/increment-decrement/range-clause target) or a Read (every
// other use). It returns (nil, nil) if offset does not land on a
// resolvable identifier.
func DocumentHighlight(cp *check.CheckedPackage, file string, offset int) ([]Highlight, error) {
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

	var out []Highlight
	ast.Inspect(astFile, func(n ast.Node) bool {
		idn, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if objectOf(cp.Info(), idn) != obj {
			return true
		}
		kind := HighlightRead
		if isWriteOccurrence(cp.Info(), astFile, idn) {
			kind = HighlightWrite
		}
		out = append(out, Highlight{Range: rangeOf(tf, idn.Pos(), idn.End()), Kind: kind})
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Range.StartOffset < out[j].Range.StartOffset })
	return out, nil
}

// objectOf returns the object idn denotes, preferring a Defs entry (idn is
// a declaration) over a Uses entry.
func objectOf(info *types.Info, idn *ast.Ident) types.Object {
	if obj, ok := info.Defs[idn]; ok && obj != nil {
		return obj
	}
	return info.Uses[idn]
}

// isWriteOccurrence reports whether idn is a declaration or is used as an
// assignment/increment-decrement/range-clause target.
func isWriteOccurrence(info *types.Info, astFile *ast.File, idn *ast.Ident) bool {
	if _, isDef := info.Defs[idn]; isDef {
		return true
	}
	path, _ := astutil.PathEnclosingInterval(astFile, idn.Pos(), idn.Pos())
	if len(path) < 2 {
		return false
	}
	switch p := path[1].(type) {
	case *ast.AssignStmt:
		return identInExprList(idn, p.Lhs)
	case *ast.IncDecStmt:
		return p.X == ast.Expr(idn)
	case *ast.RangeStmt:
		return p.Key == ast.Expr(idn) || p.Value == ast.Expr(idn)
	}
	return false
}

func identInExprList(idn *ast.Ident, list []ast.Expr) bool {
	for _, e := range list {
		if e == ast.Expr(idn) {
			return true
		}
	}
	return false
}
