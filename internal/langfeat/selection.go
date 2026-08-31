package langfeat

import (
	"github.com/sivchari/golance/internal/check"
	"golang.org/x/tools/go/ast/astutil"
)

// SelectionRanges returns the hierarchy of AST nodes enclosing offset (a
// byte offset from the start of file), innermost first, each strictly
// larger than the last (a node whose range exactly duplicates its child's —
// e.g. an ExprStmt wrapping its sole expression — is skipped, since it adds
// no useful selection step).
func SelectionRanges(cp *check.CheckedPackage, file string, offset int) ([]Range, error) {
	astFile, pos, tf, err := locate(cp, file, offset)
	if err != nil {
		return nil, err
	}
	path, _ := astutil.PathEnclosingInterval(astFile, pos, pos)

	out := make([]Range, 0, len(path))
	for _, n := range path {
		r := rangeOf(tf, n.Pos(), n.End())
		if len(out) > 0 && r == out[len(out)-1] {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
