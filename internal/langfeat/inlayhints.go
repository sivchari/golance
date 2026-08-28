package langfeat

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"

	"github.com/sivchari/golance/internal/check"
)

// Hint is one inlay hint: a label to render at Offset (a byte offset from
// the start of the file).
type Hint struct {
	Offset int
	Label  string
}

// InlayHints returns inferred-type hints for file's ":=" short variable
// declarations, the only hint kind golance renders in v0.1.
func InlayHints(cp *check.CheckedPackage, file string) ([]Hint, error) {
	astFile, tf, err := astFileByName(cp, file)
	if err != nil {
		return nil, err
	}

	var hints []Hint
	ast.Inspect(astFile, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE {
			return true
		}
		for _, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name == "_" {
				continue
			}
			t := cp.Info().TypeOf(id)
			if t == nil {
				continue
			}
			hints = append(hints, Hint{
				Offset: tf.Offset(id.End()),
				Label:  ": " + types.TypeString(t, types.RelativeTo(cp.Package())),
			})
		}
		return true
	})
	sort.Slice(hints, func(i, j int) bool { return hints[i].Offset < hints[j].Offset })
	return hints, nil
}
